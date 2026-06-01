package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	"github.com/pandey-raghvendra/osmo/internal/blockid"
	"github.com/pandey-raghvendra/osmo/internal/triage"
	"github.com/pandey-raghvendra/osmo/internal/tfplan"
)

// triageCmd implements the "osmo triage" subcommand.
//
// Usage:
//
//	osmo triage                       read plan JSON from stdin (piped)
//	osmo triage -dir ./infra          detect drift, then triage
//	osmo triage -plan-json plan.json  triage a saved plan file
//
// Exit codes:
//
//	0  all resources Safe
//	1  execution error
//	2  any Review or Flag resources (attention required)
func triageCmd(args []string) {
	fs := flag.NewFlagSet("triage", flag.ExitOnError)
	dir := fs.String("dir", ".", "Terraform working directory")
	bin := fs.String("terraform", "", "Terraform/OpenTofu binary (default: auto-detect)")
	planFile := fs.String("plan-json", "", `Path to pre-generated "terraform show -json" output; "-" reads stdin`)
	jsonOut := fs.Bool("json", false, "Emit JSON to stdout instead of human-readable output")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: osmo triage [flags]

Classify drifted resources by risk level without absorbing anything.
Prints SAFE / REVIEW / FLAG verdict per resource and a ready-to-run
osmo command that absorbs only the safe ones.

Flags:`)
		fs.PrintDefaults()
		fmt.Fprintln(os.Stderr, `
Examples:
  osmo triage -dir ./infra                  # detect and triage
  osmo -dir ./infra -json | osmo triage     # pipe from osmo -json
  osmo triage -plan-json plan.json          # pre-generated plan`)
	}
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	raw, err := loadTriagePlan(ctx, *dir, *bin, *planFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(exitError)
	}

	drifts, err := tfplan.ParseDrift(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: parse drift:", err)
		os.Exit(exitError)
	}

	cfg := loadTriageConfig(*dir)
	result := triage.Run(drifts, *dir, cfg)

	if *jsonOut {
		printTriageJSON(result)
	} else {
		printTriageHuman(result)
	}

	if result.Summary.Flag > 0 || result.Summary.Review > 0 {
		os.Exit(exitChanges)
	}
	os.Exit(exitOK)
}

// loadTriagePlan resolves raw plan JSON from one of three sources:
// explicit file, piped stdin, or live terraform detection.
func loadTriagePlan(ctx context.Context, dir, bin, planFile string) ([]byte, error) {
	if planFile == "-" {
		return io.ReadAll(os.Stdin)
	}
	if planFile != "" {
		return os.ReadFile(planFile)
	}
	// Stdin piped (not a TTY) → the user ran: osmo -json | osmo triage
	if !isTerminal(os.Stdin) {
		return io.ReadAll(os.Stdin)
	}
	// No plan source: run detection ourselves.
	if bin == "" {
		bin = triageResolveBin()
	}
	fmt.Fprintln(os.Stderr, "detecting drift (terraform plan -refresh-only)...")
	_, raw, err := tfplan.Detect(ctx, dir, bin)
	return raw, err
}

func loadTriageConfig(dir string) triage.Config {
	cfg, err := blockid.LoadConfig(dir)
	if err != nil || cfg == nil {
		return triage.Config{}
	}
	return triage.Config{
		FlagResources: cfg.Triage.FlagResources,
		FlagAttrs:     cfg.Triage.FlagAttrs,
		SafeAttrs:     cfg.Triage.SafeAttrs,
	}
}

func triageResolveBin() string {
	if env := os.Getenv("OSMO_TF_BINARY"); env != "" {
		return env
	}
	if _, err := exec.LookPath("tofu"); err == nil {
		return "tofu"
	}
	return "terraform"
}

func verdictsByKind(r triage.Result, s triage.Severity) []triage.Verdict {
	var out []triage.Verdict
	for _, v := range r.Verdicts {
		if v.Severity == s {
			out = append(out, v)
		}
	}
	return out
}

// ---- human output ----------------------------------------------------------

func printTriageHuman(r triage.Result) {
	total := r.Summary.Safe + r.Summary.Review + r.Summary.Flag
	fmt.Fprintf(os.Stderr, "\nosmo triage: %d resource(s) analysed\n\n", total)

	if r.Summary.Safe > 0 {
		fmt.Fprintf(os.Stderr, "✅ SAFE (%d) — absorb freely\n", r.Summary.Safe)
		for _, v := range verdictsByKind(r, triage.Safe) {
			fmt.Fprintf(os.Stderr, "   %-44s  %s\n", v.Address, strings.Join(v.ChangedAttrs, ", "))
		}
		fmt.Fprintln(os.Stderr)
	}

	if r.Summary.Review > 0 {
		fmt.Fprintf(os.Stderr, "⚠️  REVIEW (%d) — verify intent before absorbing\n", r.Summary.Review)
		for _, v := range verdictsByKind(r, triage.Review) {
			fmt.Fprintf(os.Stderr, "   %-44s  %s\n", v.Address, strings.Join(v.FlaggedAttrs, ", "))
			for _, reason := range v.Reasons {
				fmt.Fprintf(os.Stderr, "   %s  reason: %s\n", indent(44), reason)
			}
			if v.Suggestion != "" {
				fmt.Fprintf(os.Stderr, "   %s  tip:    %s\n", indent(44), v.Suggestion)
			}
		}
		fmt.Fprintln(os.Stderr)
	}

	if r.Summary.Flag > 0 {
		fmt.Fprintf(os.Stderr, "🚩 SECURITY FLAG (%d) — investigate before absorbing\n", r.Summary.Flag)
		for _, v := range verdictsByKind(r, triage.Flag) {
			fmt.Fprintf(os.Stderr, "   %-44s  %s\n", v.Address, strings.Join(v.FlaggedAttrs, ", "))
			for _, reason := range v.Reasons {
				fmt.Fprintf(os.Stderr, "   %s  reason: %s\n", indent(44), reason)
			}
			if v.Suggestion != "" {
				fmt.Fprintf(os.Stderr, "   %s  tip:    %s\n", indent(44), v.Suggestion)
			}
		}
		fmt.Fprintln(os.Stderr)
	}

	switch {
	case total == 0:
		fmt.Fprintln(os.Stderr, "No drift detected.")
	case r.SuggestedCommand != "":
		fmt.Fprintln(os.Stderr, "Suggested command (safe resources only):")
		fmt.Fprintf(os.Stderr, "  %s\n\n", r.SuggestedCommand)
	case r.Summary.Safe == 0:
		fmt.Fprintln(os.Stderr, "No resources safe to absorb automatically — review all drift manually.")
	}
}

func indent(n int) string { return strings.Repeat(" ", n) }

// ---- JSON output -----------------------------------------------------------

type triageJSONResult struct {
	OsmoVersion string              `json:"osmo_version"`
	Verdicts    []triageJSONVerdict `json:"verdicts"`
	Summary     triage.Summary      `json:"summary"`
	Suggested   string              `json:"suggested_command,omitempty"`
}

type triageJSONVerdict struct {
	Address      string   `json:"address"`
	Severity     string   `json:"severity"`
	ChangedAttrs []string `json:"changed_attrs"`
	FlaggedAttrs []string `json:"flagged_attrs,omitempty"`
	Reasons      []string `json:"reasons"`
	Suggestion   string   `json:"suggestion,omitempty"`
}

func printTriageJSON(r triage.Result) {
	out := triageJSONResult{
		OsmoVersion: version,
		Summary:     r.Summary,
		Suggested:   r.SuggestedCommand,
	}
	for _, v := range r.Verdicts {
		out.Verdicts = append(out.Verdicts, triageJSONVerdict{
			Address:      v.Address,
			Severity:     v.Severity.String(),
			ChangedAttrs: v.ChangedAttrs,
			FlaggedAttrs: v.FlaggedAttrs,
			Reasons:      v.Reasons,
			Suggestion:   v.Suggestion,
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}
