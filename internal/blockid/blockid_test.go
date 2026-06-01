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
