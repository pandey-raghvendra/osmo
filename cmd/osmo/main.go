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
	drifts, err := tfplan.Detect(ctx, dir, bin)
	if err != nil {
		return err
	}
	if len(drifts) == 0 {
		fmt.Println("no drift detected.")
		return nil
	}

	changes, err := absorb.Plan(dir, drifts)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		fmt.Printf("%d resource(s) drifted, but no absorbable config attributes changed.\n", len(drifts))
		return nil
	}

	for _, c := range changes {
		fmt.Printf("# %s  (attrs: %v)\n", c.Address, c.Attrs)
		fmt.Print(diff.Unified(c.Path, c.Before, c.After))
		if write {
			if err := os.WriteFile(c.Path, c.After, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", c.Path, err)
			}
		}
	}

	if write {
		fmt.Printf("\nwrote %d file change(s). run `terraform plan` to verify drift resolved.\n", len(changes))
	} else {
		fmt.Printf("\n%d file change(s) proposed. re-run with -write to apply, then `terraform plan` to verify.\n", len(changes))
	}
	return nil
}
