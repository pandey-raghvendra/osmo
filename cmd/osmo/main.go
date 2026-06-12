// Command osmo detects Terraform drift and proposes HCL changes that make
// configuration follow real-world reality (the "absorb" direction).
//
// Exit codes:
//
//	0 — no drift detected (or selection matched nothing)
//	1 — execution error
//	2 — drift found: changes proposed/written and/or unresolved drift reported
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	"github.com/pandey-raghvendra/osmo/internal/absorb"
	"github.com/pandey-raghvendra/osmo/internal/blockid"
	"github.com/pandey-raghvendra/osmo/internal/diff"
	"github.com/pandey-raghvendra/osmo/internal/provenance"
	"github.com/pandey-raghvendra/osmo/internal/tfc"
	"github.com/pandey-raghvendra/osmo/internal/tfplan"
)

// Injected at build time by GoReleaser ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const (
	exitOK      = 0 // no drift, nothing to do
	exitError   = 1 // runtime or usage error
	exitChanges = 2 // drift found: changes proposed/written or unresolved drift
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
	// Subcommand dispatch (before flag.Parse so global flags don't shadow them).
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "triage":
			triageCmd(os.Args[2:])
			return
		case "ui":
			uiCmd(os.Args[2:])
			return
		case "inspect":
			inspectCmd(os.Args[2:])
			return
		}
	}

	dir := flag.String("dir", ".", "Terraform working directory")
	bin := flag.String("terraform", "terraform", "Terraform binary to use")
	write := flag.Bool("write", false, "Write absorbed changes to disk (default: diff only)")
	planFile := flag.String("plan-json", "", "Path to pre-generated `terraform show -json` output (skips plan detection; use with Terraform Cloud or CI)")
	verify := flag.Bool("verify", false, "After writing, run terraform plan; roll back files if any absorbed resource still has planned changes (requires -write; incompatible with -plan-json)")
	approve := flag.Bool("approve", false, "Interactively approve each file change before writing (requires -write; requires a TTY)")
	jsonOut := flag.Bool("json", false, "Emit a single JSON object to stdout instead of human-readable output")
	var targets repeatedFlag
	var excludes repeatedFlag
	flag.Var(&targets, "target", "Only absorb drift on this resource address (repeatable / comma-separated; matches modules and indexed instances by prefix)")
	flag.Var(&excludes, "exclude", "Skip drift on this resource address (repeatable / comma-separated; takes precedence over -target)")
	ver := flag.Bool("version", false, "Print version and exit")
	debug := flag.Bool("debug", false, "Print debug trace to stderr (also enabled by OSMO_DEBUG=1)")
	flag.Parse()

	if *ver {
		fmt.Printf("osmo %s (%s, %s)\n", version, commit[:min(7, len(commit))], date)
		return
	}

	// Track which flags were explicitly set by the user.
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	// Load .osmo.json from the working dir (which may still be "." at this
	// point) and apply config defaults for flags the user did not set.
	if cfg, err := blockid.LoadConfig(*dir); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not load .osmo.json:", err)
	} else {
		applyConfigDefaults(cfg, set, dir, bin, write, verify, jsonOut, &targets, &excludes)
	}

	// Resolve the Terraform-compatible binary.
	// Priority: OSMO_TF_BINARY env > -terraform flag > .osmo.json default > auto-detect (tofu → terraform).
	if envBin := os.Getenv("OSMO_TF_BINARY"); envBin != "" {
		*bin = envBin
	} else if !set["terraform"] && *bin == "terraform" {
		// Neither flag nor .osmo.json set a binary: prefer tofu when installed.
		if _, err := exec.LookPath("tofu"); err == nil {
			*bin = "tofu"
		}
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
		jsonOut:  *jsonOut,
		debug:    *debug || os.Getenv("OSMO_DEBUG") == "1",
		targets:  targets,
		excludes: excludes,
	}
	code, err := run(ctx, opts)
	if err != nil {
		if opts.jsonOut {
			writeJSON(errorJSON(err))
		} else {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
	}
	os.Exit(code)
}

type runOpts struct {
	dir      string
	bin      string
	planFile string
	write    bool
	verify   bool
	approve  bool
	jsonOut  bool
	debug    bool
	targets  []string
	excludes []string
}

// debugf writes a debug line to stderr when o.debug is true.
// Output always goes to stderr so it never pollutes -json stdout.
func (o runOpts) debugf(format string, args ...interface{}) {
	if !o.debug {
		return
	}
	fmt.Fprintf(os.Stderr, "[debug] "+format+"\n", args...)
}

// ---- JSON output types --------------------------------------------------

// JSONResult is the machine-readable output emitted by -json.
type JSONResult struct {
	OsmoVersion string           `json:"osmo_version"`
	Result      string           `json:"result"`
	DriftCount  int              `json:"drift_count"`
	Changes     []JSONChange     `json:"changes"`
	Unresolved  []JSONUnresolved `json:"unresolved"`
	Error       string           `json:"error,omitempty"`
}

// Result values:
//
//	"no_drift"           — no resource_drift in plan
//	"no_match"           — drift found but -target/-exclude filtered all
//	"proposed"           — dry-run: changes proposed, not written
//	"absorbed"           — changes written to disk
//	"nothing_absorbable" — drift found but nothing could be auto-absorbed
//	"verify_failed"      — written but verify showed remaining changes; rolled back
//	"error"              — execution error

type JSONChange struct {
	Path  string     `json:"path"`
	Edits []JSONEdit `json:"edits"`
	Diff  string     `json:"diff"`
}

type JSONEdit struct {
	Address string   `json:"address"`
	Attrs   []string `json:"attrs"`
}

type JSONUnresolved struct {
	Address string `json:"address"`
	Attr    string `json:"attr"`
	Reason  string `json:"reason"`
}

func errorJSON(err error) JSONResult {
	return JSONResult{
		OsmoVersion: version,
		Result:      "error",
		Changes:     []JSONChange{},
		Unresolved:  []JSONUnresolved{},
		Error:       err.Error(),
	}
}

func writeJSON(r JSONResult) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(r)
}

// ---- run ---------------------------------------------------------------

func run(ctx context.Context, o runOpts) (int, error) {
	if o.verify {
		if !o.write {
			return exitError, fmt.Errorf("-verify requires -write (nothing is written to verify otherwise)")
		}
		if o.planFile != "" {
			return exitError, fmt.Errorf("-verify needs a live plan and is incompatible with -plan-json")
		}
	}
	if o.approve {
		if !o.write {
			return exitError, fmt.Errorf("-approve requires -write")
		}
		if o.jsonOut {
			return exitError, fmt.Errorf("-approve is interactive and cannot be combined with -json")
		}
		if !isTerminal(os.Stdin) {
			return exitError, fmt.Errorf("-approve requires an interactive TTY; in CI use -target or -exclude instead")
		}
	}

	if !o.jsonOut {
		fmt.Fprintln(os.Stderr, "detecting drift (terraform plan -refresh-only)...")
	}
	drifts, raw, err := loadDrift(ctx, o)
	if err != nil {
		return exitError, err
	}
	o.debugf("drift detection: %d resource(s) drifted", len(drifts))
	for _, d := range drifts {
		o.debugf("  drift: %s", d.Address)
	}

	if len(drifts) == 0 {
		if o.jsonOut {
			writeJSON(JSONResult{
				OsmoVersion: version,
				Result:      "no_drift",
				DriftCount:  0,
				Changes:     []JSONChange{},
				Unresolved:  []JSONUnresolved{},
			})
		} else {
			fmt.Println("no drift detected.")
		}
		return exitOK, nil
	}

	totalDriftCount := len(drifts)
	drifts = filterDrifts(drifts, o.targets, o.excludes)
	o.debugf("after target/exclude filter: %d resource(s) selected", len(drifts))
	if len(drifts) == 0 {
		if o.jsonOut {
			writeJSON(JSONResult{
				OsmoVersion: version,
				Result:      "no_match",
				DriftCount:  totalDriftCount,
				Changes:     []JSONChange{},
				Unresolved:  []JSONUnresolved{},
			})
		} else {
			fmt.Println("no drift matched -target/-exclude selection.")
		}
		return exitOK, nil
	}

	changes, unresolved, err := absorb.Plan(o.dir, drifts, raw)
	if err != nil {
		return exitError, err
	}
	o.debugf("absorb plan: %d file change(s), %d unresolved attr(s)", len(changes), len(unresolved))
	for _, c := range changes {
		o.debugf("  change: %s (%d edit(s))", c.Path, len(c.Edits))
		for _, e := range c.Edits {
			o.debugf("    edit: %s attrs=%v", e.Address, e.Attrs)
		}
	}
	for _, u := range unresolved {
		o.debugf("  unresolved: %s.%s — %s", u.Address, u.Attr, u.Reason)
	}

	// Format each changed file in-memory so diffs and written files are both
	// terraform-fmt-clean. Failures are non-fatal: warn and keep unformatted.
	for i, c := range changes {
		formatted, fmtErr := tfplan.Fmt(ctx, o.bin, c.After)
		if fmtErr != nil {
			if !o.jsonOut {
				fmt.Fprintf(os.Stderr, "warning: terraform fmt failed for %s: %v\n", c.Path, fmtErr)
			}
			continue
		}
		changes[i].After = formatted
	}

	if o.jsonOut {
		return runJSON(ctx, o, changes, unresolved, len(drifts))
	}
	return runHuman(ctx, o, changes, unresolved, len(drifts))
}

// runHuman handles the human-readable output path.
func runHuman(ctx context.Context, o runOpts, changes []absorb.FileChange, unresolved []provenance.Unresolved, driftCount int) (int, error) {
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
		reportDryRun(len(changes), len(unresolved), driftCount)
		if len(changes) > 0 || len(unresolved) > 0 {
			return exitChanges, nil
		}
		return exitOK, nil
	}

	written, err := writeChanges(approved)
	if err != nil {
		return exitError, err
	}
	if len(written) == 0 {
		fmt.Println("\nno changes written.")
		if len(unresolved) > 0 {
			return exitChanges, nil
		}
		return exitOK, nil
	}
	fmt.Printf("\nwrote %d file change(s).\n", len(written))

	if o.verify {
		if err := verifyAndMaybeRollback(ctx, o, written); err != nil {
			return exitError, err
		}
	} else {
		fmt.Println("run `terraform plan` to verify drift resolved.")
	}
	return exitChanges, nil
}

// runJSON handles the -json output path.
func runJSON(ctx context.Context, o runOpts, changes []absorb.FileChange, unresolved []provenance.Unresolved, driftCount int) (int, error) {
	jChanges := make([]JSONChange, 0, len(changes))
	for _, c := range changes {
		jc := JSONChange{
			Path: c.Path,
			Diff: diff.Unified(c.Path, c.Before, c.After),
		}
		for _, e := range c.Edits {
			jc.Edits = append(jc.Edits, JSONEdit{Address: e.Address, Attrs: e.Attrs})
		}
		jChanges = append(jChanges, jc)
	}
	jUnresolved := make([]JSONUnresolved, 0, len(unresolved))
	for _, u := range unresolved {
		jUnresolved = append(jUnresolved, JSONUnresolved{Address: u.Address, Attr: u.Attr, Reason: u.Reason})
	}

	result := "nothing_absorbable"
	switch {
	case len(changes) > 0 && !o.write:
		result = "proposed"
	case len(changes) > 0 && o.write:
		result = "absorbed"
	}

	if !o.write {
		writeJSON(JSONResult{
			OsmoVersion: version,
			Result:      result,
			DriftCount:  driftCount,
			Changes:     jChanges,
			Unresolved:  jUnresolved,
		})
		if len(changes) > 0 || len(unresolved) > 0 {
			return exitChanges, nil
		}
		return exitOK, nil
	}

	// Write phase.
	written, err := writeChanges(changes)
	if err != nil {
		return exitError, err
	}

	if o.verify && len(written) > 0 {
		fmt.Fprintln(os.Stderr, "verifying (terraform plan)...")
		if verifyErr := verifyAndMaybeRollback(ctx, o, written); verifyErr != nil {
			writeJSON(JSONResult{
				OsmoVersion: version,
				Result:      "verify_failed",
				DriftCount:  driftCount,
				Changes:     jChanges,
				Unresolved:  jUnresolved,
				Error:       verifyErr.Error(),
			})
			return exitError, nil // error already in JSON; don't double-print
		}
	}

	writeJSON(JSONResult{
		OsmoVersion: version,
		Result:      result,
		DriftCount:  driftCount,
		Changes:     jChanges,
		Unresolved:  jUnresolved,
	})
	if len(changes) > 0 || len(unresolved) > 0 {
		return exitChanges, nil
	}
	return exitOK, nil
}

// ---- helpers ------------------------------------------------------------

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
		o.debugf("load: using -plan-json %s", o.planFile)
		if !o.jsonOut {
			fmt.Fprintf(os.Stderr, "using plan json: %s (%d drift(s))\n", o.planFile, len(drifts))
		}
		return drifts, raw, nil
	}
	o.debugf("load: running terraform plan -refresh-only in %s", o.dir)
	return tfplan.Detect(ctx, o.dir, o.bin)
}

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

// plannedChangesForVerify returns the resource addresses Terraform would still
// act on after absorb. It uses TFC API when a remote/cloud backend is detected
// (local terraform plan is unavailable in that case); otherwise runs locally.
func plannedChangesForVerify(ctx context.Context, o runOpts) ([]string, error) {
	b, err := tfc.DetectBackend(o.dir)
	if err != nil {
		return nil, fmt.Errorf("TFC backend detected: %w", err)
	}
	if b != nil {
		o.debugf("verify: TFC backend detected, workspace=%s org=%s", b.Workspace, b.Organization)
		if !o.jsonOut {
			fmt.Fprintf(os.Stderr, "TFC backend detected (%s) — using speculative plan via API\n", b.WorkspaceURL())
		}
		addrs, err := b.PlannedChanges(ctx, o.dir)
		o.debugf("verify: TFC plan returned %d actionable address(es): %v", len(addrs), addrs)
		return addrs, err
	}
	o.debugf("verify: running local terraform plan")
	addrs, err := tfplan.PlannedChanges(ctx, o.dir, o.bin)
	o.debugf("verify: local plan returned %d actionable address(es): %v", len(addrs), addrs)
	return addrs, err
}

func verifyAndMaybeRollback(ctx context.Context, o runOpts, written []absorb.FileChange) error {
	if !o.jsonOut {
		fmt.Fprintln(os.Stderr, "verifying (terraform plan)...")
	}
	actionable, err := plannedChangesForVerify(ctx, o)
	if err != nil {
		if rbErr := rollback(written); rbErr != nil {
			return fmt.Errorf("verify plan failed (%v) and rollback failed: %w", err, rbErr)
		}
		return fmt.Errorf("verify plan failed; rolled back %d file(s): %w", len(written), err)
	}

	absorbed := absorbedAddresses(written)
	var stillChanging []string
	for _, addr := range actionable {
		if absorbed[addr] {
			stillChanging = append(stillChanging, addr)
		}
	}

	if len(stillChanging) == 0 {
		if !o.jsonOut {
			fmt.Printf("verified: %d absorbed resource(s) show no planned changes.\n", len(absorbed))
		}
		return nil
	}

	if rbErr := rollback(written); rbErr != nil {
		return fmt.Errorf("verification failed (changes remain on %v) and rollback failed: %w", stillChanging, rbErr)
	}
	return fmt.Errorf("verification failed: planned changes remain on %v after absorb; rolled back %d file(s)", stillChanging, len(written))
}

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
		fmt.Printf("[dry run] %d resource(s) drifted — no absorbable config attributes found.\n", driftCount)
	case changeCount == 0:
		fmt.Println("\n[dry run] no changes proposed; see unresolved drift above.")
	default:
		fmt.Printf("\n[dry run] %d file change(s) proposed above — re-run with -write to apply.\n", changeCount)
	}
}

// applyConfigDefaults fills in values from cfg for flags the user did not set.
func applyConfigDefaults(
	cfg *blockid.Config,
	set map[string]bool,
	dir, bin *string,
	write, verify, jsonOut *bool,
	targets, excludes *repeatedFlag,
) {
	d := cfg.Defaults
	if !set["dir"] && d.Dir != "" {
		*dir = d.Dir
	}
	if !set["terraform"] && d.Terraform != "" {
		*bin = d.Terraform
	}
	if !set["target"] && len(d.Targets) > 0 {
		*targets = d.Targets
	}
	if !set["exclude"] && len(d.Excludes) > 0 {
		*excludes = d.Excludes
	}
	if !set["write"] && d.Write != nil {
		*write = *d.Write
	}
	if !set["verify"] && d.Verify != nil {
		*verify = *d.Verify
	}
	if !set["json"] && d.JSON != nil {
		*jsonOut = *d.JSON
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}
