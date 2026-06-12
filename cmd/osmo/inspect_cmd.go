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
	"github.com/pandey-raghvendra/osmo/internal/inspect"
	"github.com/pandey-raghvendra/osmo/internal/tfplan"
)

// inspectCmd implements "osmo inspect" — classifies terraform plan block-level
// diffs as provider noise vs intentional semantic changes.
//
// Exit codes:
//
//	0 — no planned changes (or all noise/additions — safe to apply)
//	1 — execution error
//	2 — removals or semantic attr changes detected (manual review required)
func inspectCmd(args []string) {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	dir := fs.String("dir", ".", "Terraform working directory")
	bin := fs.String("terraform", "", "Terraform/OpenTofu binary (default: auto-detect)")
	planFile := fs.String("plan-json", "", `Path to terraform show -json output; "-" reads stdin`)
	jsonOut := fs.Bool("json", false, "Emit JSON to stdout instead of human-readable output")
	verbose := fs.Bool("verbose", false, "Show noise attrs in full detail (default: summarise)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: osmo inspect [flags]

Classify a terraform plan's nested block diffs as provider noise (optional
attrs echoed back by the API, computed IDs, equivalent values) versus
intentional semantic changes. Helps you understand which delete+create churn
in azurerm_application_gateway and similar resources is safe to apply without
manual review.

Flags:`)
		fs.PrintDefaults()
		fmt.Fprintln(os.Stderr, `
Examples:
  osmo inspect -dir ./infra                             # run plan, then inspect
  terraform show -json plan.tfplan | osmo inspect -plan-json -
  osmo inspect -plan-json plan.json -verbose`)
	}
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if *bin == "" {
		*bin = inspectResolveBin()
	}

	raw, err := loadInspectPlan(ctx, *dir, *bin, *planFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(exitError)
	}

	idreg, err := blockid.Load(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not load .osmo.json: %v\n", err)
		idreg = nil
	}

	cfg := loadInspectNormals(*dir)
	result, err := inspect.Run(raw, idreg, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(exitError)
	}

	if len(result.Resources) == 0 {
		if !*jsonOut {
			fmt.Fprintln(os.Stderr, "no planned changes detected.")
		}
		os.Exit(exitOK)
	}

	if *jsonOut {
		printInspectJSON(result)
	} else {
		printInspectHuman(result, *verbose)
	}

	if result.AllSafe {
		os.Exit(exitOK)
	}
	os.Exit(exitChanges)
}

// ---- plan loading ----------------------------------------------------------

func loadInspectPlan(ctx context.Context, dir, bin, planFile string) ([]byte, error) {
	if planFile == "-" {
		return io.ReadAll(os.Stdin)
	}
	if planFile != "" {
		return os.ReadFile(planFile)
	}
	if !isTerminal(os.Stdin) {
		return io.ReadAll(os.Stdin)
	}
	fmt.Fprintln(os.Stderr, "running terraform plan...")
	return tfplan.PlanJSON(ctx, dir, bin)
}

func loadInspectNormals(dir string) inspect.NormalsConfig {
	cfg, err := blockid.LoadConfig(dir)
	if err != nil || cfg == nil {
		return inspect.NormalsConfig{}
	}
	return inspect.NormalsConfig(cfg.Inspect.Normals)
}

func inspectResolveBin() string {
	if env := os.Getenv("OSMO_TF_BINARY"); env != "" {
		return env
	}
	if _, err := exec.LookPath("tofu"); err == nil {
		return "tofu"
	}
	return "terraform"
}

// ---- human output ----------------------------------------------------------

func printInspectHuman(r inspect.Result, verbose bool) {
	total := len(r.Resources)
	fmt.Fprintf(os.Stderr, "\nosmo inspect: %d resource(s) with planned changes\n", total)

	for _, res := range r.Resources {
		actStr := strings.Join(res.Actions, "+")
		fmt.Fprintf(os.Stderr, "\n● %s  [%s]\n", res.Address, actStr)

		if len(res.NoiseBlocks) > 0 {
			fmt.Fprintf(os.Stderr, "  ✓ noise (%d block(s) — provider normalisation, safe to apply):\n", len(res.NoiseBlocks))
			for _, nb := range res.NoiseBlocks {
				fmt.Fprintf(os.Stderr, "    %s[%q]\n", nb.BlockType, nb.Key)
				if verbose {
					for _, d := range nb.Attrs {
						printAttrDiff(d, 6)
					}
				} else {
					fmt.Fprintf(os.Stderr, "      %d attr(s): %s\n", len(nb.Attrs), attrNames(nb.Attrs))
				}
			}
		}

		if len(res.AddedBlocks) > 0 {
			fmt.Fprintf(os.Stderr, "  + added (%d new block(s)):\n", len(res.AddedBlocks))
			for _, ab := range res.AddedBlocks {
				fmt.Fprintf(os.Stderr, "    %s[%q]\n", ab.BlockType, ab.Key)
			}
		}

		if len(res.RemovedBlocks) > 0 {
			fmt.Fprintf(os.Stderr, "  - removed (%d block(s) — verify this is intentional):\n", len(res.RemovedBlocks))
			for _, rb := range res.RemovedBlocks {
				fmt.Fprintf(os.Stderr, "    %s[%q]\n", rb.BlockType, rb.Key)
			}
		}

		if len(res.ChangedBlocks) > 0 {
			fmt.Fprintf(os.Stderr, "  ~ changed (%d block(s) — review before applying):\n", len(res.ChangedBlocks))
			for _, cb := range res.ChangedBlocks {
				fmt.Fprintf(os.Stderr, "    %s[%q]\n", cb.BlockType, cb.Key)
				for _, d := range cb.SemanticDiffs {
					printAttrDiff(d, 6)
				}
				if verbose && len(cb.NoiseDiffs) > 0 {
					fmt.Fprintf(os.Stderr, "      (+ %d noise attr(s): %s)\n", len(cb.NoiseDiffs), attrNames(cb.NoiseDiffs))
				}
			}
		}

		fmt.Fprintf(os.Stderr, "  verdict: %s\n", verdictMessage(res))
	}

	fmt.Fprintln(os.Stderr)
	if r.AllSafe {
		fmt.Fprintln(os.Stderr, "✓ all changes are safe to apply (noise + additions only)")
	} else {
		fmt.Fprintln(os.Stderr, "⚠  review required before applying (see changed/removed blocks above)")
	}
	fmt.Fprintln(os.Stderr)
}

func printAttrDiff(d inspect.AttrDiff, pad int) {
	prefix := strings.Repeat(" ", pad)
	bStr := renderValue(d.Before)
	aStr := renderValue(d.After)
	if d.After == nil {
		fmt.Fprintf(os.Stderr, "%s%-36s %s  (%s)\n", prefix, d.Attr, bStr, d.Reason)
	} else if d.Noise && d.Reason == "computed by provider" {
		fmt.Fprintf(os.Stderr, "%s%-36s (computed)\n", prefix, d.Attr)
	} else {
		fmt.Fprintf(os.Stderr, "%s%-36s %s → %s", prefix, d.Attr, bStr, aStr)
		if d.Noise {
			fmt.Fprintf(os.Stderr, "  (%s)", d.Reason)
		}
		fmt.Fprintln(os.Stderr)
	}
}

func renderValue(v interface{}) string {
	if v == nil {
		return "(absent)"
	}
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

func attrNames(diffs []inspect.AttrDiff) string {
	names := make([]string, len(diffs))
	for i, d := range diffs {
		names[i] = d.Attr
	}
	return strings.Join(names, ", ")
}

func verdictMessage(r inspect.ResourceReport) string {
	parts := []string{}
	if len(r.NoiseBlocks) > 0 {
		parts = append(parts, fmt.Sprintf("%d noise", len(r.NoiseBlocks)))
	}
	if len(r.AddedBlocks) > 0 {
		parts = append(parts, fmt.Sprintf("%d addition(s)", len(r.AddedBlocks)))
	}
	if len(r.RemovedBlocks) > 0 {
		parts = append(parts, fmt.Sprintf("%d removal(s) ⚠", len(r.RemovedBlocks)))
	}
	if len(r.ChangedBlocks) > 0 {
		parts = append(parts, fmt.Sprintf("%d change(s) ⚠", len(r.ChangedBlocks)))
	}
	if len(parts) == 0 {
		return "no block-level changes detected"
	}
	msg := strings.Join(parts, " + ")
	switch r.Verdict {
	case "noise-only":
		return msg + " — safe to apply"
	case "has-additions":
		return msg + " — safe to apply"
	default:
		return msg + " — review before applying"
	}
}

// ---- JSON output -----------------------------------------------------------

type inspectJSONResult struct {
	OsmoVersion string                  `json:"osmo_version"`
	AllSafe     bool                    `json:"all_safe"`
	Resources   []inspectJSONResource   `json:"resources"`
}

type inspectJSONResource struct {
	Address       string                 `json:"address"`
	Type          string                 `json:"type"`
	Actions       []string               `json:"actions"`
	Verdict       string                 `json:"verdict"`
	NoiseBlocks   []inspectJSONBlock     `json:"noise_blocks,omitempty"`
	AddedBlocks   []inspectJSONSummary   `json:"added_blocks,omitempty"`
	RemovedBlocks []inspectJSONSummary   `json:"removed_blocks,omitempty"`
	ChangedBlocks []inspectJSONChange    `json:"changed_blocks,omitempty"`
}

type inspectJSONBlock struct {
	BlockType string               `json:"block_type"`
	Key       string               `json:"key"`
	Attrs     []inspectJSONAttr    `json:"attrs"`
}

type inspectJSONSummary struct {
	BlockType string                 `json:"block_type"`
	Key       string                 `json:"key"`
	MainAttrs map[string]interface{} `json:"main_attrs,omitempty"`
}

type inspectJSONChange struct {
	BlockType     string            `json:"block_type"`
	Key           string            `json:"key"`
	SemanticDiffs []inspectJSONAttr `json:"semantic_diffs"`
	NoiseDiffs    []inspectJSONAttr `json:"noise_diffs,omitempty"`
}

type inspectJSONAttr struct {
	Attr   string      `json:"attr"`
	Before interface{} `json:"before"`
	After  interface{} `json:"after,omitempty"`
	Noise  bool        `json:"noise"`
	Reason string      `json:"reason,omitempty"`
}

func printInspectJSON(r inspect.Result) {
	out := inspectJSONResult{
		OsmoVersion: version,
		AllSafe:     r.AllSafe,
	}
	for _, res := range r.Resources {
		jr := inspectJSONResource{
			Address: res.Address,
			Type:    res.Type,
			Actions: res.Actions,
			Verdict: res.Verdict,
		}
		for _, nb := range res.NoiseBlocks {
			jb := inspectJSONBlock{BlockType: nb.BlockType, Key: nb.Key}
			for _, a := range nb.Attrs {
				jb.Attrs = append(jb.Attrs, inspectJSONAttr{Attr: a.Attr, Before: a.Before, After: a.After, Noise: true, Reason: a.Reason})
			}
			jr.NoiseBlocks = append(jr.NoiseBlocks, jb)
		}
		for _, ab := range res.AddedBlocks {
			jr.AddedBlocks = append(jr.AddedBlocks, inspectJSONSummary{BlockType: ab.BlockType, Key: ab.Key, MainAttrs: ab.MainAttrs})
		}
		for _, rb := range res.RemovedBlocks {
			jr.RemovedBlocks = append(jr.RemovedBlocks, inspectJSONSummary{BlockType: rb.BlockType, Key: rb.Key, MainAttrs: rb.MainAttrs})
		}
		for _, cb := range res.ChangedBlocks {
			jc := inspectJSONChange{BlockType: cb.BlockType, Key: cb.Key}
			for _, d := range cb.SemanticDiffs {
				jc.SemanticDiffs = append(jc.SemanticDiffs, inspectJSONAttr{Attr: d.Attr, Before: d.Before, After: d.After, Noise: false})
			}
			for _, d := range cb.NoiseDiffs {
				jc.NoiseDiffs = append(jc.NoiseDiffs, inspectJSONAttr{Attr: d.Attr, Before: d.Before, After: d.After, Noise: true, Reason: d.Reason})
			}
			jr.ChangedBlocks = append(jr.ChangedBlocks, jc)
		}
		out.Resources = append(out.Resources, jr)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}
