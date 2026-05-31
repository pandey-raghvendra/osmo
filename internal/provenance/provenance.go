// Package provenance traces a drifted resource attribute back to the single
// HCL literal that should be edited to absorb the drift.
//
// The tracer never edits anything; it only decides WHERE an edit belongs by
// walking the `configuration` tree from `terraform show -json`. Anything it
// cannot resolve to a concrete literal is returned as Unresolved with a reason,
// so the caller can report it instead of guessing.
package provenance

import (
	"fmt"
	"strings"

	"github.com/raghav/osmo/internal/address"
	"github.com/raghav/osmo/internal/config"
)

// Kind enumerates the sort of HCL construct an edit lands on.
type Kind int

const (
	ResourceAttr    Kind = iota // attribute literal inside a resource/data block
	ModuleArg                   // argument literal on a `module "x" {}` call
	VariableDefault             // `default` on a `variable "x" {}` declaration
)

// Target is a concrete, editable location for absorbing one attribute's drift.
type Target struct {
	Kind Kind
	// SourceModulePath is the module path (outermost->innermost) whose source
	// directory contains the construct. Empty = root module.
	SourceModulePath []address.Step

	// Resource identification (Kind == ResourceAttr).
	ResourceMode string // "managed" | "data"
	ResourceType string
	ResourceName string

	CallName string // module call label (Kind == ModuleArg)
	VarName  string // variable name (Kind == VariableDefault)

	// Attr is the attribute to set: the resource attribute, the module argument
	// name, or "default".
	Attr  string
	Value interface{}

	// InstanceKey, when non-nil, means set Attr's map entry [*InstanceKey] to
	// Value rather than replacing the whole attribute (for_each/count scoping).
	InstanceKey *string
}

// Unresolved explains why an attribute could not be absorbed automatically.
type Unresolved struct {
	Address string
	Attr    string
	Reason  string
}

func (u Unresolved) Error() string {
	return fmt.Sprintf("%s.%s: %s", u.Address, u.Attr, u.Reason)
}

// Trace resolves attr of the resource at addr (within root) to an edit Target,
// or returns Unresolved describing why it cannot.
func Trace(root *config.Module, addr address.Addr, attr string, value interface{}) (*Target, *Unresolved) {
	depth := len(addr.Modules)
	containing, err := navigate(root, addr.Modules)
	if err != nil {
		return nil, unresolved(addr, attr, err.Error())
	}
	res := containing.FindResource(addr.RelAddr())
	if res == nil {
		return nil, unresolved(addr, attr, "resource not found in configuration")
	}
	expr, ok := res.Expressions[attr]
	if !ok {
		return nil, unresolved(addr, attr, "attribute not present in configuration (computed/nested)")
	}

	// Is the drifted instance one of many? Then a shared literal can't isolate it.
	instanced, instKey := instancing(addr)

	if expr.HasConstant {
		// Literal lives in the resource block itself.
		if instanced {
			return nil, unresolved(addr, attr,
				"value is a constant inside a multi-instance/module resource; cannot isolate one instance")
		}
		return &Target{
			Kind:             ResourceAttr,
			SourceModulePath: addr.Modules,
			ResourceMode:     addr.Mode,
			ResourceType:     addr.Type,
			ResourceName:     addr.Name,
			Attr:             attr,
			Value:            value,
		}, nil
	}

	ref, uerr := singleVarRef(expr.References)
	if uerr != "" {
		return nil, unresolved(addr, attr, uerr)
	}
	// Resolve the variable upward through the module path.
	return resolveVar(root, addr, depth, ref, attr, value, instanced, instKey)
}

// resolveVar resolves varName in the scope of the module at the given depth
// (depth = number of module steps containing the resource), walking outward
// until it reaches a literal module argument or a variable default.
func resolveVar(root *config.Module, addr address.Addr, depth int, varName, attr string, value interface{}, instanced bool, instKey string) (*Target, *Unresolved) {
	if depth == 0 {
		// Root-scope variable: only a literal default is editable (broad).
		v := root.Variables[varName]
		if len(v.Default) == 0 {
			return nil, unresolved(addr, attr,
				fmt.Sprintf("traces to root var.%s which has no literal default", varName))
		}
		return &Target{
			Kind:    VariableDefault,
			VarName: varName,
			Attr:    "default",
			Value:   value,
		}, nil
	}

	// The module containing the resource at this depth was created by the call
	// modules[depth-1], defined in the parent module modules[:depth-1].
	parentPath := addr.Modules[:depth-1]
	callStep := addr.Modules[depth-1]
	parent, err := navigate(root, parentPath)
	if err != nil {
		return nil, unresolved(addr, attr, err.Error())
	}
	call, ok := parent.ModuleCalls[callStep.Name]
	if !ok {
		return nil, unresolved(addr, attr,
			fmt.Sprintf("module call %q not found in configuration", callStep.Name))
	}
	argExpr, ok := call.Expressions[varName]
	if !ok {
		return nil, unresolved(addr, attr,
			fmt.Sprintf("module %q does not pass argument %q", callStep.Name, varName))
	}

	if argExpr.HasConstant {
		t := &Target{
			Kind:             ModuleArg,
			SourceModulePath: parentPath,
			CallName:         callStep.Name,
			Attr:             varName,
			Value:            value,
		}
		// Instance scoping: if this drifted instance is keyed and the argument
		// is a map keyed the same way, target just that entry.
		if instanced && callStep.HasIndex {
			k := callStep.Index
			t.InstanceKey = &k
		} else if instanced && addr.HasIndex {
			k := addr.Index
			t.InstanceKey = &k
		}
		return t, nil
	}

	ref, uerr := singleVarRef(argExpr.References)
	if uerr != "" {
		return nil, unresolved(addr, attr, uerr)
	}
	return resolveVar(root, addr, depth-1, ref, attr, value, instanced, instKey)
}

// TraceForEach resolves the for_each collection variable of a dynamic block to
// an edit Target. varName is the raw variable name (no "var." prefix) extracted
// from the dynamic block's for_each expression in the HCL source; value is the
// full after-collection ([]interface{} of block attribute maps) that the
// for_each variable should be set to.
func TraceForEach(root *config.Module, addr address.Addr, varName string, value interface{}) (*Target, *Unresolved) {
	depth := len(addr.Modules)
	instanced, instKey := instancing(addr)
	return resolveVar(root, addr, depth, varName, "dynamic.for_each("+varName+")", value, instanced, instKey)
}

// TraceNested resolves a drifted attribute inside a nested block to an edit
// Target by reading the block's expression from the configuration tree and
// following the same var-chain logic as Trace.
//
// blockPath is the ordered slice of block type names from the resource body to
// the block that contains attr (e.g., ["ebs_block_device"] or
// ["server_side_encryption_configuration","rule","apply_server_side_encryption_by_default"]).
func TraceNested(root *config.Module, addr address.Addr, blockPath []string, attr string, value interface{}) (*Target, *Unresolved) {
	qualAttr := qualNestedAttr(blockPath, attr)
	depth := len(addr.Modules)

	containing, err := navigate(root, addr.Modules)
	if err != nil {
		return nil, unresolved(addr, qualAttr, err.Error())
	}
	res := containing.FindResource(addr.RelAddr())
	if res == nil {
		return nil, unresolved(addr, qualAttr, "resource not found in configuration")
	}
	expr := res.FindNestedExpression(blockPath, attr)
	if expr == nil {
		return nil, unresolved(addr, qualAttr, "nested block expression not found in configuration tree")
	}
	if expr.HasConstant {
		// Constant in config tree but evalBodyAttr failed — shouldn't normally
		// happen. Avoid double-edit by reporting unresolvable.
		return nil, unresolved(addr, qualAttr,
			"nested block attr is a constant in config tree but could not be evaluated (expression may be complex)")
	}
	ref, uerr := singleVarRef(expr.References)
	if uerr != "" {
		return nil, unresolved(addr, qualAttr, uerr)
	}
	instanced, instKey := instancing(addr)
	return resolveVar(root, addr, depth, ref, qualAttr, value, instanced, instKey)
}

// qualNestedAttr builds a dotted attribute name like "block.subblock.attr".
func qualNestedAttr(path []string, attr string) string {
	if len(path) == 0 {
		return attr
	}
	return strings.Join(path, ".") + "." + attr
}

// navigate walks from root down the module path and returns the innermost
// module reached.
func navigate(root *config.Module, steps []address.Step) (*config.Module, error) {
	cur := root
	for _, s := range steps {
		call, ok := cur.ModuleCalls[s.Name]
		if !ok {
			return nil, fmt.Errorf("module %q not found in configuration", s.Name)
		}
		cur = &call.Module
	}
	return cur, nil
}

// instancing reports whether the resource address denotes one of several
// instances (via module for_each/count or its own), and the relevant key.
func instancing(addr address.Addr) (bool, string) {
	if addr.HasIndex {
		return true, addr.Index
	}
	for _, m := range addr.Modules {
		if m.HasIndex {
			return true, m.Index
		}
	}
	return false, ""
}

// singleVarRef returns the variable name from a one-element var reference list,
// or a reason string explaining why the references are not absorbable.
func singleVarRef(refs []string) (string, string) {
	if len(refs) == 0 {
		return "", "attribute has no constant value and no references (computed)"
	}
	if len(refs) > 1 {
		return "", "value is a composed expression of multiple references"
	}
	r := refs[0]
	switch {
	case strings.HasPrefix(r, "var."):
		name := strings.TrimPrefix(r, "var.")
		if strings.Contains(name, ".") {
			return "", "value references a sub-attribute of an object variable (unsupported)"
		}
		return name, ""
	case strings.HasPrefix(r, "local."):
		return "", "value derives from a local; locals are not present in plan JSON"
	case strings.HasPrefix(r, "each.") || strings.HasPrefix(r, "count."):
		return "", "value derives from each/count meta-argument"
	default:
		return "", fmt.Sprintf("value references %q (resource/output reference, not a literal)", r)
	}
}

func unresolved(addr address.Addr, attr, reason string) *Unresolved {
	return &Unresolved{Address: fullAddr(addr), Attr: attr, Reason: reason}
}

// fullAddr reconstructs a human-readable address for reporting.
func fullAddr(a address.Addr) string {
	var b strings.Builder
	for _, m := range a.Modules {
		b.WriteString("module.")
		b.WriteString(m.Name)
		if m.HasIndex {
			b.WriteString("[" + quoteKey(m.Index) + "]")
		}
		b.WriteString(".")
	}
	if a.Mode == "data" {
		b.WriteString("data.")
	}
	b.WriteString(a.Type + "." + a.Name)
	if a.HasIndex {
		b.WriteString("[" + quoteKey(a.Index) + "]")
	}
	return b.String()
}

func quoteKey(k string) string {
	if k == "" {
		return ""
	}
	// Numeric count index stays bare; for_each key is quoted.
	for _, c := range k {
		if c < '0' || c > '9' {
			return `"` + k + `"`
		}
	}
	return k
}
