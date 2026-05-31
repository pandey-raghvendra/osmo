// Command drift-resolver detects Terraform drift and proposes HCL changes that
// make configuration follow real-world reality (the "absorb" direction).
//
// v1: prints a unified diff to stdout. It never writes files or applies changes
// unless -write is passed.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/raghav/osmo/internal/absorb"
	"github.com/raghav/osmo/internal/diff"
	"github.com/raghav/osmo/internal/tfplan"
)

func main() {
	dir := flag.String("dir", ".", "Terraform working directory")
	bin := flag.String("terraform", "terraform", "Terraform binary to use")
	write := flag.Bool("write", false, "Write absorbed changes to disk (default: diff only)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, *dir, *bin, *write); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dir, bin string, write bool) error {
	fmt.Fprintln(os.Stderr, "detecting drift (terraform plan -refresh-only)...")
	drifts, raw, err := tfplan.Detect(ctx, dir, bin)
	if err != nil {
		return err
	}
	if len(drifts) == 0 {
		fmt.Println("no drift detected.")
		return nil
	}

	changes, unresolved, err := absorb.Plan(dir, drifts, raw)
	if err != nil {
		return err
	}

	for _, c := range changes {
		fmt.Printf("# %s\n", c.Path)
		for _, e := range c.Edits {
			fmt.Printf("#   %s  (attrs: %v)\n", e.Address, e.Attrs)
		}
		fmt.Print(diff.Unified(c.Path, c.Before, c.After))
		if write {
			if err := os.WriteFile(c.Path, c.After, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", c.Path, err)
			}
		}
	}

	if len(unresolved) > 0 {
		fmt.Printf("\n%d drift(s) not auto-absorbed:\n", len(unresolved))
		for _, u := range unresolved {
			fmt.Printf("  ! %s.%s: %s\n", u.Address, u.Attr, u.Reason)
		}
	}

	switch {
	case len(changes) == 0 && len(unresolved) == 0:
		fmt.Printf("%d resource(s) drifted, but no absorbable config attributes changed.\n", len(drifts))
	case len(changes) == 0:
		fmt.Println("\nno changes proposed; see unresolved drift above.")
	case write:
		fmt.Printf("\nwrote %d file change(s). run `terraform plan` to verify drift resolved.\n", len(changes))
	default:
		fmt.Printf("\n%d file change(s) proposed. re-run with -write to apply, then `terraform plan` to verify.\n", len(changes))
	}
	return nil
}
