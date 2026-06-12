package inspect

import "encoding/json"

// NormalsConfig holds user-supplied normals loaded from .osmo.json.
// Keys follow the format "resource_type.block_type.attr".
// Values are lists of equivalent normal values — changes between any two values
// in the same list are classified as provider noise.
type NormalsConfig map[string][]interface{}

// normalsRegistry is the compiled normal-value lookup for one inspect run.
type normalsRegistry struct {
	// m: "resource_type.block_type.attr" → set of JSON-marshaled normal values
	m map[string]map[string]bool
}

// builtinNormals maps attribute keys to their equivalent/normal values.
// A change between any two values in the slice is provider noise (safe to apply).
var builtinNormals = map[string][]interface{}{
	// Azure Application Gateway — optional attrs echoed back by the Azure API
	// as empty strings even when not set in Terraform config.
	"azurerm_application_gateway.backend_http_settings.affinity_cookie_name": {""},
	"azurerm_application_gateway.backend_http_settings.host_name":            {""},
	"azurerm_application_gateway.backend_http_settings.sni_name":             {""},
	"azurerm_application_gateway.backend_http_settings.probe_id":             {""},
	"azurerm_application_gateway.backend_http_settings.probe_name":           {""},
	// Azure treats empty path and "/" as equivalent (both = no path prefix).
	"azurerm_application_gateway.backend_http_settings.path": {"", "/"},

	"azurerm_application_gateway.http_listener.firewall_policy_id":   {""},
	"azurerm_application_gateway.http_listener.ssl_certificate_id":   {""},
	"azurerm_application_gateway.http_listener.ssl_certificate_name": {""},
	"azurerm_application_gateway.http_listener.ssl_profile_id":       {""},
	"azurerm_application_gateway.http_listener.ssl_profile_name":     {""},
	"azurerm_application_gateway.http_listener.host_name":            {""},

	"azurerm_application_gateway.request_routing_rule.backend_address_pool_id":    {""},
	"azurerm_application_gateway.request_routing_rule.backend_address_pool_name":  {""},
	"azurerm_application_gateway.request_routing_rule.backend_http_settings_id":   {""},
	"azurerm_application_gateway.request_routing_rule.backend_http_settings_name": {""},
	"azurerm_application_gateway.request_routing_rule.redirect_configuration_id":  {""},
	"azurerm_application_gateway.request_routing_rule.redirect_configuration_name": {""},
	"azurerm_application_gateway.request_routing_rule.rewrite_rule_set_id":        {""},
	"azurerm_application_gateway.request_routing_rule.rewrite_rule_set_name":      {""},
	"azurerm_application_gateway.request_routing_rule.url_path_map_id":            {""},
	"azurerm_application_gateway.request_routing_rule.url_path_map_name":          {""},

	// Azure Load Balancer
	"azurerm_lb.frontend_ip_configuration.private_ip_address":            {""},
	"azurerm_lb.frontend_ip_configuration.public_ip_prefix_id":           {""},
	"azurerm_lb.frontend_ip_configuration.subnet_id":                     {""},
}

func buildNormalsRegistry(cfg NormalsConfig) *normalsRegistry {
	r := &normalsRegistry{m: make(map[string]map[string]bool)}
	add := func(key string, vals []interface{}) {
		if r.m[key] == nil {
			r.m[key] = make(map[string]bool)
		}
		for _, v := range vals {
			b, _ := json.Marshal(v)
			r.m[key][string(b)] = true
		}
	}
	for k, vs := range builtinNormals {
		add(k, vs)
	}
	for k, vs := range cfg {
		add(k, vs)
	}
	return r
}

// isKnownNormal returns true when v is a registered normal value for this attr.
func (r *normalsRegistry) isKnownNormal(resType, blockType, attr string, v interface{}) bool {
	key := resType + "." + blockType + "." + attr
	normals, ok := r.m[key]
	if !ok {
		return false
	}
	b, _ := json.Marshal(v)
	return normals[string(b)]
}

// isAbsentNormal returns true when attr is absent from the new config (after==nil)
// but its before value is a known provider default — safe to remove from config.
func (r *normalsRegistry) isAbsentNormal(resType, blockType, attr string, before interface{}) bool {
	return r.isKnownNormal(resType, blockType, attr, before)
}

// isBothNormal returns true when both before and after values are in the normal set.
func (r *normalsRegistry) isBothNormal(resType, blockType, attr string, before, after interface{}) bool {
	key := resType + "." + blockType + "." + attr
	normals, ok := r.m[key]
	if !ok {
		return false
	}
	bb, _ := json.Marshal(before)
	ab, _ := json.Marshal(after)
	return normals[string(bb)] && normals[string(ab)]
}
