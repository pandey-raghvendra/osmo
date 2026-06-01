// Command osmo detects Terraform drift and proposes HCL changes that make
// configuration follow real-world reality (the "absorb" direction).
//
// Prints a unified diff to stdout by default. Pass -write to apply to disk.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/pandey-raghvendra/osmo/internal/absorb"
	"github.com/pandey-raghvendra/osmo/internal/diff"
	"github.com/pandey-raghvendra/osmo/internal/provenance"
	"github.com/pandey-raghvendra/osmo/internal/tfplan"
)

// Injected at build time by GoReleaser ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// repeatedFlag collects a comma-separated or repeated string flag.
type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, ",") }
func (r *repeatedFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			*r = append(*r, p)
		}
	}
	return nil
}

func main() {
	dir := flag.String("dir", ".", "Terraform working directory")
	bin := flag.String("terraform", "terraform", "Terraform binary to use")
	write := flag.Bool("write", false, "Write absorbed changes to disk (default: diff only)")
	planFile := flag.String("plan-json", "", "Path to pre-generated `terraform show -json` output (skips plan detection; use with Terraform Cloud or CI)")
	verify := flag.Bool("verify", false, "After writing, re-run a refresh-only plan; if drift remains on absorbed resources, roll back the files (requires -write; incompatible with -plan-json)")
	approve := flag.Bool("approve", false, "Interactively approve each file change before writing (requires -write)")
	var targets repeatedFlag
	var excludes repeatedFlag
	flag.Var(&targets, "target", "Only absorb drift on this resource address (repeatable / comma-separated; matches modules and indexed instances by prefix)")
	flag.Var(&excludes, "exclude", "Skip drift on this resource address (repeatable / comma-separated; takes precedence over -target)")
	ver := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *ver {
		fmt.Printf("osmo %s (%s, %s)\n", version, commit[:min(7, len(commit))], date)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	opts := runOpts{
		dir:      *dir,
		bin:      *bin,
		planFile: *planFile,
		write:    *write,
		verify:   *verify,
		approve:  *approve,
		targets:  targets,
		excludes: excludes,
	}
	if err := run(ctx, opts); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type runOpts struct {
	dir      string
	bin      string
	planFile string
	write    bool
	verify   bool
	approve  bool
	targets  []string
	excludes []string
}

func run(ctx context.Context, o runOpts) error {
	if o.verify {
		if !o.write {
			return fmt.Errorf("-verify requires -write (nothing is written to verify otherwise)")
		}
		if o.planFile != "" {
			return fmt.Errorf("-verify needs a live refresh-only plan and is incompatible with -plan-json")
		}
	}
	if o.approve && !o.write {
		return fmt.Errorf("-approve requires -write")
	}

	drifts, raw, err := loadDrift(ctx, o)
	if err != nil {
		return err
	}
	if len(drifts) == 0 {
		fmt.Println("no drift detected.")
		return nil
	}

	drifts = filterDrifts(drifts, o.targets, o.excludes)
	if len(drifts) == 0 {
		fmt.Println("no drift matched -target/-exclude selection.")
		return nil
	}

	changes, unresolved, err := absorb.Plan(o.dir, drifts, raw)
	if err != nil {
		return err
	}

	// Print every proposed change; in approve mode, collect the user's picks.
	var approved []absorb.FileChange
	in := bufio.NewReader(os.Stdin)
	for _, c := range changes {
		fmt.Printf("# %s\n", c.Path)
		for _, e := range c.Edits {
			fmt.Printf("#   %s  (attrs: %v)\n", e.Address, e.Attrs)
		}
		fmt.Print(diff.Unified(c.Path, c.Before, c.After))
		if o.write && o.approve {
			if promptYesNo(in, fmt.Sprintf("absorb changes to %s?", c.Path)) {
				approved = append(approved, c)
			} else {
				fmt.Printf("skipped %s\n", c.Path)
			}
		} else {
			approved = append(approved, c)
		}
	}

	printUnresolved(unresolved)

	if !o.write {
		reportDryRun(len(changes), len(unresolved), len(drifts))
		return nil
	}

	written, err := writeChanges(approved)
	if err != nil {
		return err
	}
	if len(written) == 0 {
		fmt.Println("\nno changes written.")
		return nil
	}
	fmt.Printf("\nwrote %d file change(s).\n", len(written))

	if o.verify {
		return verifyAndMaybeRollback(ctx, o, written)
	}
	fmt.Println("run `terraform plan` to verify drift resolved.")
	return nil
}

// loadDrift returns drift either from a supplied plan JSON or a live plan.
func loadDrift(ctx context.Context, o runOpts) ([]tfplan.Drift, []byte, error) {
	if o.planFile != "" {
		raw, err := os.ReadFile(o.planFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read plan json %s: %w", o.planFile, err)
		}
		drifts, err := tfplan.ParseDrift(raw)
		if err != nil {
			return nil, nil, err
		}
		fmt.Fprintf(os.Stderr, "using plan json: %s (%d drift(s))\n", o.planFile, len(drifts))
		return drifts, raw, nil
	}
	fmt.Fprintln(os.Stderr, "detecting drift (terraform plan -refresh-only)...")
	return tfplan.Detect(ctx, o.dir, o.bin)
}

// writeChanges writes each FileChange to disk, returning the ones written so
// they can be rolled back later.
func writeChanges(changes []absorb.FileChange) ([]absorb.FileChange, error) {
	var written []absorb.FileChange
	for _, c := range changes {
		if err := os.WriteFile(c.Path, c.After, 0o644); err != nil {
			return written, fmt.Errorf("write %s: %w", c.Path, err)
		}
		written = append(written, c)
	}
	return written, nil
}

// verifyAndMaybeRollback runs a normal (config-driven) plan after writing. The
// absorb edits config to match reality, so a converged resource shows no
// planned action. If any absorbed resource is still actionable, all written
// files are restored to their pre-absorb content and a non-nil error returned.
func verifyAndMaybeRollback(ctx context.Context, o runOpts, written []absorb.FileChange) error {
	fmt.Fprintln(os.Stderr, "verifying (terraform plan)...")
	actionable, err := tfplan.PlannedChanges(ctx, o.dir, o.bin)
	if err != nil {
		if rbErr := rollback(written); rbErr != nil {
			return fmt.Errorf("verify plan failed (%v) and rollback failed: %w", err, rbErr)
		}
		return fmt.Errorf("verify plan failed; rolled back %d file(s): %w", len(written), err)
	}

	absorbed := absorbedAddresses(written)
	var stillDrifting []string
	for _, addr := range actionable {
		if absorbed[addr] {
			stillDrifting = append(stillDrifting, addr)
		}
	}

	if len(stillDrifting) == 0 {
		fmt.Printf("verified: %d absorbed resource(s) show no planned changes.\n", len(absorbed))
		return nil
	}

	if rbErr := rollback(written); rbErr != nil {
		return fmt.Errorf("verification failed (changes remain on %v) and rollback failed: %w", stillDrifting, rbErr)
	}
	return fmt.Errorf("verification failed: planned changes remain on %v after absorb; rolled back %d file(s)", stillDrifting, len(written))
}

// rollback restores each written file to its pre-absorb content.
func rollback(written []absorb.FileChange) error {
	for _, c := range written {
		if err := os.WriteFile(c.Path, c.Before, 0o644); err != nil {
			return fmt.Errorf("restore %s: %w", c.Path, err)
		}
	}
	return nil
}

func absorbedAddresses(changes []absorb.FileChange) map[string]bool {
	addrs := make(map[string]bool)
	for _, c := range changes {
		for _, e := range c.Edits {
			addrs[e.Address] = true
		}
	}
	return addrs
}

// filterDrifts keeps only drifts matching the target selection (all if empty)
// and drops any matching the exclude selection (exclude wins).
func filterDrifts(drifts []tfplan.Drift, targets, excludes []string) []tfplan.Drift {
	if len(targets) == 0 && len(excludes) == 0 {
		return drifts
	}
	out := make([]tfplan.Drift, 0, len(drifts))
	for _, d := range drifts {
		if len(targets) > 0 && !matchesAny(d.Address, targets) {
			continue
		}
		if matchesAny(d.Address, excludes) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// matchesAny reports whether addr equals or is nested under any selector.
// "module.x" matches "module.x.aws_instance.y"; "aws_x.y" matches "aws_x.y[0]".
func matchesAny(addr string, selectors []string) bool {
	for _, s := range selectors {
		if addr == s || strings.HasPrefix(addr, s+".") || strings.HasPrefix(addr, s+"[") {
			return true
		}
	}
	return false
}

func promptYesNo(in *bufio.Reader, question string) bool {
	fmt.Printf("%s [y/N] ", question)
	line, err := in.ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

func printUnresolved(unresolved []provenance.Unresolved) {
	if len(unresolved) == 0 {
		return
	}
	fmt.Printf("\n%d drift(s) not auto-absorbed:\n", len(unresolved))
	for _, u := range unresolved {
		fmt.Printf("  ! %s.%s: %s\n", u.Address, u.Attr, u.Reason)
	}
}

func reportDryRun(changeCount, unresolvedCount, driftCount int) {
	switch {
	case changeCount == 0 && unresolvedCount == 0:
		fmt.Printf("%d resource(s) drifted, but no absorbable config attributes changed.\n", driftCount)
	case changeCount == 0:
		fmt.Println("\nno changes proposed; see unresolved drift above.")
	default:
		fmt.Printf("\n%d file change(s) proposed. re-run with -write to apply, then `terraform plan` to verify.\n", changeCount)
	}
}
