package absorb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raghav/osmo/internal/tfplan"
)

func TestPlanAbsorbsLiteralAttrs(t *testing.T) {
	dir := t.TempDir()
	src := `resource "aws_instance" "web" {
  ami           = "ami-old"
  instance_type = "t3.micro"
  monitoring    = false
  tags = {
    Name = "web"
  }
}
`
	tfPath := filepath.Join(dir, "main.tf")
	if err := os.WriteFile(tfPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	// Drift: someone hotfixed instance_type + monitoring in the console.
	// computed_id is a read-only attr that is NOT in config and must be ignored.
	drifts := []tfplan.Drift{{
		Address: "aws_instance.web",
		Type:    "aws_instance",
		Name:    "web",
		Before: map[string]interface{}{
			"ami":           "ami-old",
			"instance_type": "t3.micro",
			"monitoring":    false,
			"computed_id":   "i-111",
		},
		After: map[string]interface{}{
			"ami":           "ami-old",      // unchanged
			"instance_type": "t3.large",     // drifted, in config -> rewrite
			"monitoring":    true,           // drifted, in config -> rewrite
			"computed_id":   "i-999",        // drifted, NOT in config -> ignore
		},
	}}

	changes, err := Plan(dir, drifts)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change, got %d", len(changes))
	}
	got := string(changes[0].After)

	if !strings.Contains(got, `instance_type = "t3.large"`) {
		t.Errorf("instance_type not absorbed:\n%s", got)
	}
	if !strings.Contains(got, "monitoring    = true") {
		t.Errorf("monitoring not absorbed:\n%s", got)
	}
	if strings.Contains(got, "computed_id") {
		t.Errorf("computed_id leaked into config (should be ignored):\n%s", got)
	}
	// Untouched attr + comments/formatting preserved.
	if !strings.Contains(got, `ami           = "ami-old"`) {
		t.Errorf("unrelated attr or formatting lost:\n%s", got)
	}

	wantAttrs := []string{"instance_type", "monitoring"}
	if strings.Join(changes[0].Attrs, ",") != strings.Join(wantAttrs, ",") {
		t.Errorf("Attrs = %v, want %v", changes[0].Attrs, wantAttrs)
	}
}

func TestPlanNoChangeWhenDriftNotInConfig(t *testing.T) {
	dir := t.TempDir()
	src := `resource "aws_instance" "web" {
  ami = "ami-old"
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	drifts := []tfplan.Drift{{
		Address: "aws_instance.web", Type: "aws_instance", Name: "web",
		Before: map[string]interface{}{"computed_id": "i-1"},
		After:  map[string]interface{}{"computed_id": "i-2"},
	}}
	changes, err := Plan(dir, drifts)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("want 0 changes (drift only in computed attrs), got %d", len(changes))
	}
}
