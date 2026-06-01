// Package blockid provides the identity-key registry used to match set-typed
// nested blocks by a stable attribute (e.g. "name") rather than by position,
// and loads the optional per-project .osmo.json configuration file.
//
// Full .osmo.json schema:
//
//	{
//	  "block_identity": {
//	    "google_compute_firewall.allow": ["protocol"],
//	    "my_custom_resource.my_block":  ["id"]
//	  },
//	  "defaults": {
//	    "dir":       "./infra",
//	    "terraform": "/usr/local/bin/terraform",
//	    "targets":   ["module.app"],
//	    "excludes":  ["aws_instance.bastion"],
//	    "write":     false,
//	    "verify":    false,
//	    "json":      false
//	  }
//	}
//
// block_identity keys are "<resource_type>.<block_type>". User entries override
// built-ins. defaults are applied only for CLI flags the user did not set.
package blockid

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ConfigFile is the name of the optional per-project config file.
const ConfigFile = ".osmo.json"

// Defaults holds per-project CLI flag defaults. A nil pointer field means
// "not set in config" and will not override the compiled-in flag default.
type Defaults struct {
	Dir       string   `json:"dir"`
	Terraform string   `json:"terraform"`
	Targets   []string `json:"targets"`
	Excludes  []string `json:"excludes"`
	Write     *bool    `json:"write"`
	Verify    *bool    `json:"verify"`
	JSON      *bool    `json:"json"`
}

// Config is the full contents of .osmo.json.
type Config struct {
	BlockIdentity map[string][]string `json:"block_identity"`
	Defaults      Defaults            `json:"defaults"`
}

// Registry is an immutable lookup of identity keys per resource+block type.
type Registry struct {
	m map[string][]string
}

// builtins maps "resource_type.block_type" → identity key names.
var builtins = map[string][]string{
	// Azure Application Gateway — all named sub-resources are sets keyed by name.
	"azurerm_application_gateway.backend_address_pool":      {"name"},
	"azurerm_application_gateway.backend_http_settings":     {"name"},
	"azurerm_application_gateway.frontend_ip_configuration": {"name"},
	"azurerm_application_gateway.frontend_port":             {"name"},
	"azurerm_application_gateway.gateway_ip_configuration":  {"name"},
	"azurerm_application_gateway.http_listener":             {"name"},
	"azurerm_application_gateway.probe":                     {"name"},
	"azurerm_application_gateway.redirect_configuration":    {"name"},
	"azurerm_application_gateway.request_routing_rule":      {"name"},
	"azurerm_application_gateway.rewrite_rule_set":          {"name"},
	"azurerm_application_gateway.ssl_certificate":           {"name"},
	"azurerm_application_gateway.trusted_root_certificate":  {"name"},
	"azurerm_application_gateway.url_path_map":              {"name"},

	// Azure Load Balancer.
	"azurerm_lb.frontend_ip_configuration": {"name"},

	// GCP Compute — firewall allow/deny keyed by protocol.
	"google_compute_firewall.allow": {"protocol"},
	"google_compute_firewall.deny":  {"protocol"},

	// GCP Compute Backend Service — backends keyed by the instance group URL.
	"google_compute_backend_service.backend": {"group"},

	// GCP Container Cluster — node pools keyed by name.
	"google_container_cluster.node_pool": {"name"},
}

// LoadConfig reads .osmo.json from dir and returns the full Config.
// An absent config file returns a zero Config (not an error).
func LoadConfig(dir string) (*Config, error) {
	path := filepath.Join(dir, ConfigFile)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Load reads .osmo.json from dir and returns the identity-key Registry.
// An absent config file returns a Registry with built-in defaults only.
func Load(dir string) (*Registry, error) {
	cfg, err := LoadConfig(dir)
	if err != nil {
		return nil, err
	}
	return cfg.registry(), nil
}

func (c *Config) registry() *Registry {
	merged := make(map[string][]string, len(builtins))
	for k, v := range builtins {
		merged[k] = v
	}
	for k, v := range c.BlockIdentity {
		merged[k] = v
	}
	return &Registry{m: merged}
}

// Keys returns the identity key names for the given resource type and nesting
// path, or nil if no entry is registered (caller falls back to scoring).
func (r *Registry) Keys(resourceType string, path []string) []string {
	if len(path) == 0 {
		return nil
	}
	key := resourceType + "." + path[len(path)-1]
	return r.m[key]
}
