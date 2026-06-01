// Package triage classifies Terraform drift by risk level using a
// deterministic rule engine. No API key, no network calls, fully offline.
//
// Three severity levels:
//
//	Safe   — tags, descriptions, scalar size/type changes. Absorb freely.
//	Review — capacity/autoscaler attributes that may be externally managed.
//	         Verify intent; consider lifecycle.ignore_changes.
//	Flag   — security-sensitive resources or attributes (IAM, firewall,
//	         encryption, public access). Investigate before absorbing.
//
// Rules are extensible via .osmo.json "triage" section without code changes.
package triage

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pandey-raghvendra/osmo/internal/tfplan"
)

// Severity is the risk level of a drifted resource.
type Severity int

const (
	Safe   Severity = iota // absorb without concern
	Review                 // verify intent first
	Flag                   // security-sensitive; must investigate
)

func (s Severity) String() string {
	switch s {
	case Safe:
		return "safe"
	case Review:
		return "review"
	case Flag:
		return "flag"
	}
	return "unknown"
}

// Verdict is the triage result for one drifted resource.
type Verdict struct {
	Address      string
	Severity     Severity
	ChangedAttrs []string // all changed attribute names
	FlaggedAttrs []string // subset that triggered Review or Flag
	Reasons      []string // human-readable explanation
	Suggestion   string   // optional recommended action
}

// Summary is the count breakdown across all verdicts.
type Summary struct {
	Safe   int
	Review int
	Flag   int
}

// Result is the full triage output for a plan.
type Result struct {
	Verdicts         []Verdict
	Summary          Summary
	SuggestedCommand string // ready-to-run osmo command absorbing only safe drift
}

// Config holds per-project triage overrides from .osmo.json.
type Config struct {
	FlagResources []string // additional resource types to always flag
	FlagAttrs     []string // additional attribute name patterns to flag
	SafeAttrs     []string // additional attribute name patterns to treat as safe
}

// Run classifies each drift in drifts and returns a triage Result.
// dir is used only to construct the suggested command string.
func Run(drifts []tfplan.Drift, dir string, cfg Config) Result {
	reg := buildRegistry(cfg)
	verdicts := make([]Verdict, 0, len(drifts))
	for _, d := range drifts {
		verdicts = append(verdicts, classify(d, reg))
	}
	return Result{
		Verdicts:         verdicts,
		Summary:          summarise(verdicts),
		SuggestedCommand: suggestCmd(dir, verdicts),
	}
}

func classify(d tfplan.Drift, reg *registry) Verdict {
	changed := changedAttrs(d.Before, d.After)

	v := Verdict{
		Address:      d.Address,
		Severity:     Safe,
		ChangedAttrs: changed,
	}

	// Whole-resource flag: security-sensitive resource type.
	if reason, ok := reg.flagResources[d.Type]; ok {
		v.Severity = Flag
		v.FlaggedAttrs = changed
		v.Reasons = []string{d.Type + ": " + reason}
		v.Suggestion = investigateSuggestion(d.Address)
		return v
	}

	// Per-attribute classification; overall severity = max across all attrs.
	for _, attr := range changed {
		sev, reason := reg.classifyAttr(attr)
		if sev > v.Severity {
			v.Severity = sev
		}
		if sev >= Review {
			v.FlaggedAttrs = append(v.FlaggedAttrs, attr)
			v.Reasons = append(v.Reasons, attr+": "+reason)
		}
	}

	switch v.Severity {
	case Review:
		v.Suggestion = reviewSuggestion(v.FlaggedAttrs)
	case Flag:
		v.Suggestion = investigateSuggestion(d.Address)
	}

	if len(v.Reasons) == 0 {
		v.Reasons = []string{"metadata or scalar change, no security impact detected"}
	}

	return v
}

// changedAttrs returns the attribute names whose values differ between before
// and after (including attrs present in one state but absent from the other).
func changedAttrs(before, after tfplan.TFState) []string {
	seen := make(map[string]bool, len(before)+len(after))
	for k := range before {
		seen[k] = true
	}
	for k := range after {
		seen[k] = true
	}
	var changed []string
	for k := range seen {
		bv := before[k] // zero TFValue (TFNull) if absent
		av := after[k]
		if !bv.Equal(av) {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	return changed
}

func summarise(verdicts []Verdict) Summary {
	s := Summary{}
	for _, v := range verdicts {
		switch v.Severity {
		case Safe:
			s.Safe++
		case Review:
			s.Review++
		case Flag:
			s.Flag++
		}
	}
	return s
}

// suggestCmd builds a ready-to-run osmo command that absorbs only Safe drift
// while explicitly excluding Flagged resources.
func suggestCmd(dir string, verdicts []Verdict) string {
	var safe, flagged []string
	for _, v := range verdicts {
		switch v.Severity {
		case Safe:
			safe = append(safe, v.Address)
		case Flag:
			flagged = append(flagged, v.Address)
		}
	}

	if len(safe) == 0 {
		return ""
	}
	// Everything safe — no target scoping needed.
	if len(safe) == len(verdicts) {
		return fmt.Sprintf("osmo -dir %s -write", dir)
	}

	parts := []string{fmt.Sprintf("osmo -dir %s -write", dir)}
	for _, addr := range safe {
		parts = append(parts, fmt.Sprintf("  -target %s", addr))
	}
	for _, addr := range flagged {
		parts = append(parts, fmt.Sprintf("  -exclude %s", addr))
	}
	return strings.Join(parts, " \\\n")
}

func investigateSuggestion(addr string) string {
	return fmt.Sprintf("investigate before absorbing — if intentional: osmo -write -target %s -approve", addr)
}

func reviewSuggestion(attrs []string) string {
	for _, a := range attrs {
		if strings.Contains(a, "capacity") || strings.Contains(a, "desired") || strings.Contains(a, "count") {
			return "if autoscaler-managed, consider: lifecycle { ignore_changes = [" + strings.Join(attrs, ", ") + "] }"
		}
	}
	return "verify this change was intentional before absorbing"
}
