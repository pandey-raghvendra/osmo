// Package address parses Terraform resource addresses such as
//
//	module.network["app"].module.sub.azurerm_subnet.this["a"]
//	data.aws_ami.ubuntu
//
// into structured parts so callers can walk the configuration tree and scope
// edits to a specific instance.
package address

import (
	"fmt"
	"strings"
)

// Step is one `module.<name>` hop, optionally instanced via for_each/count.
type Step struct {
	Name     string
	Index    string // unquoted for_each key or count number; empty if none
	HasIndex bool
}

// Addr is a parsed resource address.
type Addr struct {
	Modules  []Step // outermost -> innermost
	Mode     string // "managed" | "data"
	Type     string
	Name     string
	Index    string // instance key/index of the resource itself
	HasIndex bool
}

// RelAddr returns the resource's address relative to its module, matching the
// `address` field used inside `configuration` (no module prefix, no index).
func (a Addr) RelAddr() string {
	if a.Mode == "data" {
		return "data." + a.Type + "." + a.Name
	}
	return a.Type + "." + a.Name
}

// Parse splits a Terraform resource address into its parts.
func Parse(s string) (Addr, error) {
	segs := splitTopDots(s)
	if len(segs) == 0 {
		return Addr{}, fmt.Errorf("empty address")
	}

	var a Addr
	i := 0
	for i+1 < len(segs) && segs[i] == "module" {
		name, idx, has := splitIndex(segs[i+1])
		a.Modules = append(a.Modules, Step{Name: name, Index: idx, HasIndex: has})
		i += 2
	}

	a.Mode = "managed"
	if i < len(segs) && segs[i] == "data" {
		a.Mode = "data"
		i++
	}

	if i >= len(segs) {
		return Addr{}, fmt.Errorf("address %q missing resource type", s)
	}
	a.Type = segs[i]
	i++
	if i >= len(segs) {
		return Addr{}, fmt.Errorf("address %q missing resource name", s)
	}
	a.Name, a.Index, a.HasIndex = splitIndex(segs[i])
	return a, nil
}

// splitTopDots splits on '.' at bracket depth 0, ignoring dots inside [...]
// (which may appear in quoted for_each keys like ["a.b"]).
func splitTopDots(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case '.':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

// splitIndex separates a base identifier from a trailing [..] index, returning
// the base, the unquoted index, and whether an index was present.
func splitIndex(seg string) (base, index string, has bool) {
	open := strings.IndexByte(seg, '[')
	if open < 0 || !strings.HasSuffix(seg, "]") {
		return seg, "", false
	}
	base = seg[:open]
	inner := seg[open+1 : len(seg)-1]
	inner = strings.TrimSpace(inner)
	// for_each keys are quoted; count indices are bare numbers.
	if len(inner) >= 2 && inner[0] == '"' && inner[len(inner)-1] == '"' {
		inner = inner[1 : len(inner)-1]
	}
	return base, inner, true
}
