// Package e2e validates osmo against REAL `terraform show -json` output.
//
// testdata/real_show.json is captured from an actual terraform run on
// testdata/fixture (see testdata/regen.sh). The test drives the full absorb
// pipeline with that real configuration tree, proving our struct assumptions
// and provenance tracing match what Terraform actually emits — without needing
// cloud credentials or a live run in CI.
package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raghav/osmo/internal/absorb"
	"github.com/raghav/osmo/internal/tfplan"
)

// copyTF replicates only the .tf files under src into dst, preserving layout.
func copyTF(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(p, ".tf") {
			return nil
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAbsorbAgainstRealShowJSON(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "real_show.json"))
	if err != nil {
		t.Fatalf("missing golden fixture (run testdata/regen.sh): %v", err)
	}

	dir := t.TempDir()
	copyTF(t, filepath.Join("testdata", "fixture"), dir)

	// Synthetic drift on the REAL resource addresses from the fixture:
	//  - root resource literal attribute
	//  - attribute that flows from a module-call argument literal
	drifts := []tfplan.Drift{
		{
			Address: "random_string.root", Type: "random_string", Name: "root",
			Before: map[string]interface{}{"length": float64(8)},
			After:  map[string]interface{}{"length": float64(12)},
		},
		{
			Address: "module.pet.random_pet.this", Type: "random_pet", Name: "this",
			Before: map[string]interface{}{"prefix": "alpha"},
			After:  map[string]interface{}{"prefix": "beta"},
		},
	}

	changes, unresolved, err := absorb.Plan(dir, drifts, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved against real config: %v", unresolved)
	}
	// Both edits land in the root main.tf -> one FileChange, two resource edits.
	if len(changes) != 1 {
		t.Fatalf("want 1 FileChange, got %d", len(changes))
	}
	if len(changes[0].Edits) != 2 {
		t.Fatalf("want 2 edits, got %d: %+v", len(changes[0].Edits), changes[0].Edits)
	}
	got := string(changes[0].After)
	if !strings.Contains(got, "length  = 12") {
		t.Errorf("root literal not absorbed:\n%s", got)
	}
	if !strings.Contains(got, `prefix = "beta"`) {
		t.Errorf("module-arg drift not absorbed at root call:\n%s", got)
	}

	// The module source must remain a var reference (never hardcoded).
	modSrc, err := os.ReadFile(filepath.Join(dir, "modules", "pet", "main.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(modSrc), "prefix = var.prefix") {
		t.Errorf("module source was wrongly modified:\n%s", modSrc)
	}
}
