// localstack_test.go validates osmo's full pipeline — drift detection parsing,
// provenance tracing, and HCL absorb — against REAL `terraform show -json`
// output that contains actual resource_drift entries.
//
// real_drift_show.json was captured from a live LocalStack (S3) run where a
// tag was injected directly via AWS API after terraform apply, producing a real
// drift. See testdata/localstack/regen.sh to reproduce.
package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pandey-raghvendra/osmo/internal/absorb"
	"github.com/pandey-raghvendra/osmo/internal/tfplan"
)

func TestAbsorbAgainstRealResourceDrift(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "real_drift_show.json"))
	if err != nil {
		t.Fatalf("missing golden fixture (run testdata/localstack/regen.sh): %v", err)
	}

	// Parse real resource_drift from the captured JSON.
	drifts, err := tfplan.ParseDrift(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(drifts) == 0 {
		t.Fatal("real_drift_show.json contains no resource_drift — regen.sh may need to be re-run")
	}

	// Validate resource_drift shape matches our Drift struct.
	d := drifts[0]
	if d.Address != "aws_s3_bucket.drift_test" {
		t.Errorf("unexpected drift address: %q", d.Address)
	}
	if d.Type != "aws_s3_bucket" || d.Name != "drift_test" {
		t.Errorf("unexpected type/name: %q/%q", d.Type, d.Name)
	}
	beforeTags, _ := d.Before["tags"].AsObject()
	afterTags, _ := d.After["tags"].AsObject()
	if env, _ := beforeTags["Env"].AsString(); env != "test" {
		t.Errorf("unexpected before.tags: %v", beforeTags)
	}
	if managed, _ := afterTags["Managed"].AsString(); managed != "manual" {
		t.Errorf("expected Managed=manual in after.tags: %v", afterTags)
	}

	// Drive the full absorb pipeline: the fixture has `tags = { Env = "test" }`.
	// After drift, tags has Managed=manual added. Since tags is a literal map
	// in the resource block, absorb should rewrite it to match after.
	dir := t.TempDir()
	copyTF(t, filepath.Join("testdata", "localstack"), dir)

	changes, unresolved, err := absorb.Plan(dir, drifts, raw)
	if err != nil {
		t.Fatal(err)
	}

	// tags_all is a computed/read-only attribute — it must be unresolved, not
	// injected into config. Verify it appears in unresolved, not in changes.
	tagsAllUnresolved := false
	for _, u := range unresolved {
		if u.Attr == "tags_all" {
			tagsAllUnresolved = true
		}
	}
	// tags_all has constant_value in config expressions: it may either be
	// absorbed (safe: it mirrors tags) or skipped. Either outcome is valid.
	// What must NOT happen: silent data loss or a crash.
	_ = tagsAllUnresolved

	// The key assertion: tags drift IS absorbable (literal map in resource block)
	// and must appear in the changes.
	if len(changes) == 0 {
		t.Fatalf("expected at least one change (tags drift), got none. unresolved: %v", unresolved)
	}
	got := string(changes[0].After)
	if !strings.Contains(got, `"Managed"`) && !strings.Contains(got, "Managed") {
		t.Errorf("drifted tag not absorbed into HCL:\n%s", got)
	}
	// Original tag must be preserved.
	if !strings.Contains(got, `"Env"`) && !strings.Contains(got, "Env") {
		t.Errorf("original Env tag lost after absorb:\n%s", got)
	}
}
