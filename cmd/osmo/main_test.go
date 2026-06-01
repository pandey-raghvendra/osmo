package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pandey-raghvendra/osmo/internal/absorb"
	"github.com/pandey-raghvendra/osmo/internal/blockid"
	"github.com/pandey-raghvendra/osmo/internal/tfplan"
)

func addrs(drifts []tfplan.Drift) []string {
	out := make([]string, len(drifts))
	for i, d := range drifts {
		out[i] = d.Address
	}
	return out
}

func TestMatchesAny(t *testing.T) {
	cases := []struct {
		addr string
		sel  []string
		want bool
	}{
		{"aws_instance.web", []string{"aws_instance.web"}, true},
		{"aws_instance.web", []string{"aws_instance.db"}, false},
		{"aws_instance.web[0]", []string{"aws_instance.web"}, true},
		{"module.app.aws_instance.web", []string{"module.app"}, true},
		{"module.appendix.aws_instance.web", []string{"module.app"}, false}, // no false prefix match
		{"aws_instance.website", []string{"aws_instance.web"}, false},       // no substring match
		{"aws_instance.web", nil, false},
	}
	for _, c := range cases {
		if got := matchesAny(c.addr, c.sel); got != c.want {
			t.Errorf("matchesAny(%q, %v) = %v, want %v", c.addr, c.sel, got, c.want)
		}
	}
}

func TestFilterDrifts(t *testing.T) {
	drifts := []tfplan.Drift{
		{Address: "aws_instance.web"},
		{Address: "aws_instance.db"},
		{Address: "module.net.aws_vpc.main"},
	}

	t.Run("no selectors returns all", func(t *testing.T) {
		got := filterDrifts(drifts, nil, nil)
		if len(got) != 3 {
			t.Fatalf("want 3, got %v", addrs(got))
		}
	})

	t.Run("target narrows", func(t *testing.T) {
		got := filterDrifts(drifts, []string{"aws_instance.web", "module.net"}, nil)
		if len(got) != 2 || got[0].Address != "aws_instance.web" || got[1].Address != "module.net.aws_vpc.main" {
			t.Fatalf("unexpected: %v", addrs(got))
		}
	})

	t.Run("exclude wins over target", func(t *testing.T) {
		got := filterDrifts(drifts, []string{"aws_instance.web"}, []string{"aws_instance.web"})
		if len(got) != 0 {
			t.Fatalf("want 0, got %v", addrs(got))
		}
	})

	t.Run("exclude only", func(t *testing.T) {
		got := filterDrifts(drifts, nil, []string{"aws_instance.db"})
		if len(got) != 2 {
			t.Fatalf("want 2, got %v", addrs(got))
		}
		for _, d := range got {
			if d.Address == "aws_instance.db" {
				t.Fatalf("db should be excluded: %v", addrs(got))
			}
		}
	})

	t.Run("does not mutate input order", func(t *testing.T) {
		filterDrifts(drifts, []string{"aws_instance.db"}, nil)
		if drifts[0].Address != "aws_instance.web" {
			t.Fatalf("input slice was mutated: %v", addrs(drifts))
		}
	})
}

func TestIsTerminal(t *testing.T) {
	// A regular file is never a terminal.
	f, err := os.CreateTemp(t.TempDir(), "notty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Fatal("regular file should not be a terminal")
	}
}

func TestRunJSONNoDrift(t *testing.T) {
	// Write a minimal plan JSON with no resource_drift.
	planJSON := `{"resource_drift":[],"configuration":{"root_module":{}}}`
	tmp := t.TempDir()
	pf := filepath.Join(tmp, "plan.json")
	if err := os.WriteFile(pf, []byte(planJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// Capture stdout by redirecting it.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w

	code, runErr := run(t.Context(), runOpts{
		dir:      tmp,
		planFile: pf,
		jsonOut:  true,
	})

	w.Close()
	os.Stdout = old

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	r.Close()

	if runErr != nil {
		t.Fatal(runErr)
	}
	if code != exitOK {
		t.Fatalf("want exit %d, got %d", exitOK, code)
	}
	var out JSONResult
	if err := json.Unmarshal(buf[:n], &out); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, buf[:n])
	}
	if out.Result != "no_drift" {
		t.Fatalf("want result=no_drift, got %q", out.Result)
	}
	if out.OsmoVersion == "" {
		t.Fatal("osmo_version missing from JSON output")
	}
}

// ---- applyConfigDefaults ---------------------------------------------------

func boolPtr(b bool) *bool { return &b }

func TestApplyConfigDefaults_AllFieldsSet(t *testing.T) {
	cfg := &blockid.Config{
		Defaults: blockid.Defaults{
			Dir:       "./infra",
			Terraform: "/usr/bin/terraform",
			Targets:   []string{"module.app"},
			Excludes:  []string{"aws_instance.bastion"},
			Write:     boolPtr(true),
			Verify:    boolPtr(true),
			JSON:      boolPtr(true),
		},
	}
	dir := "."
	bin := "terraform"
	write := false
	verify := false
	jsonOut := false
	var targets repeatedFlag
	var excludes repeatedFlag

	// No flags explicitly set by user — all defaults should apply.
	set := map[string]bool{}
	applyConfigDefaults(cfg, set, &dir, &bin, &write, &verify, &jsonOut, &targets, &excludes)

	if dir != "./infra" {
		t.Errorf("dir: got %q", dir)
	}
	if bin != "/usr/bin/terraform" {
		t.Errorf("bin: got %q", bin)
	}
	if !write {
		t.Error("write should be true from defaults")
	}
	if !verify {
		t.Error("verify should be true from defaults")
	}
	if !jsonOut {
		t.Error("json should be true from defaults")
	}
	if len(targets) != 1 || targets[0] != "module.app" {
		t.Errorf("targets: got %v", []string(targets))
	}
	if len(excludes) != 1 || excludes[0] != "aws_instance.bastion" {
		t.Errorf("excludes: got %v", []string(excludes))
	}
}

func TestApplyConfigDefaults_FlagWinsOverConfig(t *testing.T) {
	cfg := &blockid.Config{
		Defaults: blockid.Defaults{
			Dir:       "./infra",
			Terraform: "/usr/bin/terraform",
		},
	}
	dir := "./my-dir"
	bin := "tofu"
	write := false
	verify := false
	jsonOut := false
	var targets repeatedFlag
	var excludes repeatedFlag

	// User explicitly set dir and terraform.
	set := map[string]bool{"dir": true, "terraform": true}
	applyConfigDefaults(cfg, set, &dir, &bin, &write, &verify, &jsonOut, &targets, &excludes)

	// Explicit flags must not be overridden by config defaults.
	if dir != "./my-dir" {
		t.Errorf("dir should be unchanged: got %q", dir)
	}
	if bin != "tofu" {
		t.Errorf("bin should be unchanged: got %q", bin)
	}
}

func TestApplyConfigDefaults_EmptyConfig(t *testing.T) {
	cfg := &blockid.Config{}
	dir := "."
	bin := "terraform"
	write := false
	verify := false
	jsonOut := false
	var targets repeatedFlag
	var excludes repeatedFlag

	set := map[string]bool{}
	applyConfigDefaults(cfg, set, &dir, &bin, &write, &verify, &jsonOut, &targets, &excludes)

	// Nothing should change with an empty config.
	if dir != "." || bin != "terraform" || write || verify || jsonOut {
		t.Error("empty config should not change any value")
	}
}

// ---- run: validation errors ------------------------------------------------

func TestRun_VerifyWithoutWrite(t *testing.T) {
	code, err := run(t.Context(), runOpts{verify: true, write: false})
	if code != exitError {
		t.Errorf("want exitError, got %d", code)
	}
	if err == nil {
		t.Error("want error")
	}
}

func TestRun_VerifyWithPlanJSON(t *testing.T) {
	code, err := run(t.Context(), runOpts{verify: true, write: true, planFile: "plan.json"})
	if code != exitError {
		t.Errorf("want exitError, got %d", code)
	}
	if err == nil {
		t.Error("want error")
	}
}

func TestRunJSONDriftFound(t *testing.T) {
	// A plan JSON that has drift but the single resource can't be absorbed
	// (no matching .tf file in the temp dir) → result=nothing_absorbable.
	planJSON := `{
		"resource_drift": [{
			"address": "aws_instance.web",
			"type": "aws_instance",
			"name": "web",
			"change": {
				"before": {"instance_type": "t3.micro"},
				"after":  {"instance_type": "t3.large"},
				"after_sensitive": false
			}
		}],
		"configuration": {"root_module": {
			"resources": [{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web",
				"expressions":{"instance_type":{"constant_value":"t3.micro"}}}]
		}}
	}`
	tmp := t.TempDir()
	pf := filepath.Join(tmp, "plan.json")
	if err := os.WriteFile(pf, []byte(planJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w

	code, runErr := run(t.Context(), runOpts{
		dir:      tmp,
		planFile: pf,
		jsonOut:  true,
	})

	w.Close()
	os.Stdout = old

	buf := make([]byte, 8192)
	n, _ := r.Read(buf)
	r.Close()

	if runErr != nil {
		t.Fatal(runErr)
	}
	// Drift found → exit 2.
	if code != exitChanges {
		t.Fatalf("want exitChanges(2), got %d — output: %s", code, buf[:n])
	}
	var out JSONResult
	if err := json.Unmarshal(buf[:n], &out); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, buf[:n])
	}
	if out.DriftCount != 1 {
		t.Errorf("want drift_count=1, got %d", out.DriftCount)
	}
}

func TestAbsorbedAddresses(t *testing.T) {
	changes := []absorb.FileChange{
		{Path: "a.tf", Edits: []absorb.ResourceEdit{{Address: "aws_instance.web"}}},
		{Path: "b.tf", Edits: []absorb.ResourceEdit{{Address: "aws_instance.db"}, {Address: "aws_instance.web"}}},
	}
	got := absorbedAddresses(changes)
	if len(got) != 2 || !got["aws_instance.web"] || !got["aws_instance.db"] {
		t.Fatalf("unexpected absorbed set: %v", got)
	}
}
