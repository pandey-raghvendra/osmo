// Package blockid provides the identity-key registry used to match set-typed
// nested blocks by a stable attribute (e.g. "name") rather than by position.
//
// Built-in entries cover common providers. Projects can extend or override via
// .osmo.json in the Terraform working directory:
//
//	{
//	  "block_identity": {
//	    "google_compute_firewall.allow": ["protocol"],
//	    "my_custom_resource.my_block": ["id"]
//	  }
//	}
//
// The map key is "<resource_type>.<block_type>" (last element of the nesting
// path). User entries take precedence over built-ins.
package blockid

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ConfigFile is the name of the optional per-project config file.
const ConfigFile = ".osmo.json"

// osmoJSON is the schema of the optional project config file.
type osmoJSON struct {
	BlockIdentity map[string][]string `json:"block_identity"`
}

// builtins maps "resource_type.block_type" → identity key names.
// These cover set-typed blocks in commonly used providers where positional
// matching produces false diffs.
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

	// Azure Load Balancer — pools, rules, probes are named sets.
	"azurerm_lb.frontend_ip_configuration": {"name"},

	// GCP Compute — firewall allow/deny keyed by protocol.
	"google_compute_firewall.allow": {"protocol"},
	"google_compute_firewall.deny":  {"protocol"},

	// GCP Compute Backend Service — backends keyed by the instance group URL.
	"google_compute_backend_service.backend": {"group"},

	// GCP Container Cluster — node pools keyed by name.
	"google_container_cluster.node_pool": {"name"},
}

// Registry is an immutable lookup of identity keys per resource+block type.
type Registry struct {
	m map[string][]string
}

// Load reads .osmo.json from dir (if present) and merges it with the built-in
// defaults. User entries override built-ins for the same key.
// An absent config file is not an error.
func Load(dir string) (*Registry, error) {
	merged := make(map[string][]string, len(builtins))
	for k, v := range builtins {
		merged[k] = v
	}

	path := filepath.Join(dir, ConfigFile)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Registry{m: merged}, nil
	}
	if err != nil {
		return nil, err
	}

	var cfg osmoJSON
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	for k, v := range cfg.BlockIdentity {
		merged[k] = v
	}
	return &Registry{m: merged}, nil
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
