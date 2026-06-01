package blockid

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuiltins(t *testing.T) {
	reg, err := Load(t.TempDir()) // no .osmo.json → pure builtins
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		resType string
		path    []string
		want    []string
	}{
		{"azurerm_application_gateway", []string{"backend_http_settings"}, []string{"name"}},
		{"azurerm_application_gateway", []string{"http_listener"}, []string{"name"}},
		{"azurerm_application_gateway", []string{"probe"}, []string{"name"}},
		{"azurerm_lb", []string{"frontend_ip_configuration"}, []string{"name"}},
		{"google_compute_firewall", []string{"allow"}, []string{"protocol"}},
		{"google_compute_firewall", []string{"deny"}, []string{"protocol"}},
		{"google_compute_backend_service", []string{"backend"}, []string{"group"}},
		{"google_container_cluster", []string{"node_pool"}, []string{"name"}},
		{"aws_instance", []string{"root_block_device"}, nil}, // no built-in
		{"azurerm_application_gateway", nil, nil},            // no path → nil
	}
	for _, c := range cases {
		got := reg.Keys(c.resType, c.path)
		if len(got) != len(c.want) {
			t.Errorf("Keys(%q, %v) = %v, want %v", c.resType, c.path, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("Keys(%q, %v)[%d] = %q, want %q", c.resType, c.path, i, got[i], c.want[i])
			}
		}
	}
}

func TestUserOverride(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"block_identity": {"aws_instance.root_block_device": ["volume_type"], "google_compute_firewall.allow": ["protocol","ports"]}}`
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	// User-defined key for previously uncovered resource.
	got := reg.Keys("aws_instance", []string{"root_block_device"})
	if len(got) != 1 || got[0] != "volume_type" {
		t.Errorf("user override: got %v, want [volume_type]", got)
	}

	// User override replaces built-in.
	got = reg.Keys("google_compute_firewall", []string{"allow"})
	if len(got) != 2 || got[0] != "protocol" || got[1] != "ports" {
		t.Errorf("user override of built-in: got %v, want [protocol ports]", got)
	}
}

func TestMissingConfigFile(t *testing.T) {
	reg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal("missing .osmo.json should not error:", err)
	}
	if reg == nil {
		t.Fatal("registry should not be nil")
	}
}

func TestMalformedConfigFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("malformed JSON should return an error")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	trueVal := true
	_ = trueVal
	cfg := `{
		"defaults": {
			"dir": "./infra",
			"terraform": "/usr/bin/terraform",
			"targets": ["module.app", "aws_instance.web"],
			"excludes": ["aws_instance.bastion"],
			"write": true,
			"verify": true,
			"json": false
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Defaults.Dir != "./infra" {
		t.Errorf("dir: got %q", c.Defaults.Dir)
	}
	if c.Defaults.Terraform != "/usr/bin/terraform" {
		t.Errorf("terraform: got %q", c.Defaults.Terraform)
	}
	if len(c.Defaults.Targets) != 2 || c.Defaults.Targets[0] != "module.app" {
		t.Errorf("targets: got %v", c.Defaults.Targets)
	}
	if len(c.Defaults.Excludes) != 1 || c.Defaults.Excludes[0] != "aws_instance.bastion" {
		t.Errorf("excludes: got %v", c.Defaults.Excludes)
	}
	if c.Defaults.Write == nil || !*c.Defaults.Write {
		t.Errorf("write: got %v", c.Defaults.Write)
	}
	if c.Defaults.Verify == nil || !*c.Defaults.Verify {
		t.Errorf("verify: got %v", c.Defaults.Verify)
	}
	if c.Defaults.JSON == nil || *c.Defaults.JSON {
		t.Errorf("json: got %v", c.Defaults.JSON)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	c, err := LoadConfig(t.TempDir())
	if err != nil {
		t.Fatal("missing file should not error:", err)
	}
	if c.Defaults.Dir != "" || len(c.Defaults.Targets) != 0 {
		t.Errorf("empty config should have zero defaults, got %+v", c.Defaults)
	}
}
