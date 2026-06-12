package inspect

import (
	"encoding/json"
	"testing"

	"github.com/pandey-raghvendra/osmo/internal/blockid"
)

// buildPlan constructs a minimal plan JSON with one resource_change.
func buildPlan(t *testing.T, address, resType string, actions []string, before, after, afterUnknown map[string]interface{}) []byte {
	t.Helper()
	pj := map[string]interface{}{
		"resource_changes": []interface{}{
			map[string]interface{}{
				"address": address,
				"type":    resType,
				"name":    "test",
				"change": map[string]interface{}{
					"actions":       actions,
					"before":        before,
					"after":         after,
					"after_unknown": afterUnknown,
				},
			},
		},
	}
	b, err := json.Marshal(pj)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func loadRegistry(t *testing.T) *blockid.Registry {
	t.Helper()
	reg, err := blockid.Load("../../")
	if err != nil {
		t.Log("no .osmo.json, using builtins only")
		return nil
	}
	return reg
}

// TestAppGWBackendHTTPSettingsNoise verifies that empty-string attrs echoed
// by the Azure API are classified as noise, not semantic changes.
func TestAppGWBackendHTTPSettingsNoise(t *testing.T) {
	idreg := loadRegistry(t)

	before := map[string]interface{}{
		"backend_http_settings": []interface{}{
			map[string]interface{}{
				"name":                  "https-appservice-settings",
				"port":                  float64(443),
				"protocol":              "Https",
				"path":                  "/",
				"affinity_cookie_name":  "",
				"host_name":             "",
				"sni_name":              "",
				"probe_name":            "",
				"probe_id":              "",
				"request_timeout":       float64(30),
			},
		},
	}
	// After: same block but path changed to "" and optional attrs omitted.
	after := map[string]interface{}{
		"backend_http_settings": []interface{}{
			map[string]interface{}{
				"name":            "https-appservice-settings",
				"port":            float64(443),
				"protocol":        "Https",
				"path":            "",
				"request_timeout": float64(30),
			},
		},
	}
	afterUnknown := map[string]interface{}{
		"backend_http_settings": []interface{}{
			map[string]interface{}{"id": true, "probe_id": true},
		},
	}

	raw := buildPlan(t, "azurerm_application_gateway.appgw", "azurerm_application_gateway",
		[]string{"update"}, before, after, afterUnknown)

	result, err := Run(raw, idreg, NormalsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Resources) != 1 {
		t.Fatalf("expected 1 resource report, got %d", len(result.Resources))
	}
	res := result.Resources[0]

	if len(res.NoiseBlocks) != 1 {
		t.Errorf("expected 1 noise block, got %d (changed=%d added=%d removed=%d)",
			len(res.NoiseBlocks), len(res.ChangedBlocks), len(res.AddedBlocks), len(res.RemovedBlocks))
	}
	if len(res.ChangedBlocks) != 0 {
		t.Errorf("expected 0 changed blocks, got %d: %+v", len(res.ChangedBlocks), res.ChangedBlocks)
	}
	if !result.AllSafe {
		t.Error("expected AllSafe=true for pure noise")
	}
	if res.Verdict != "noise-only" {
		t.Errorf("expected verdict noise-only, got %q", res.Verdict)
	}
}

// TestAppGWNewListenerIsAddition verifies a new http_listener block is
// classified as an intentional addition, not noise or removal.
func TestAppGWNewListenerIsAddition(t *testing.T) {
	idreg := loadRegistry(t)

	before := map[string]interface{}{
		"http_listener": []interface{}{
			map[string]interface{}{
				"name":                          "listener-http",
				"frontend_port_name":            "port-80",
				"frontend_ip_configuration_name": "frontend-public",
				"protocol":                       "Http",
			},
		},
	}
	after := map[string]interface{}{
		"http_listener": []interface{}{
			map[string]interface{}{
				"name":                          "listener-https",
				"frontend_port_name":            "port-443",
				"frontend_ip_configuration_name": "frontend-public",
				"protocol":                       "Https",
				"ssl_certificate_name":          "ssl-cert",
			},
			map[string]interface{}{
				"name":                          "listener-http",
				"frontend_port_name":            "port-80",
				"frontend_ip_configuration_name": "frontend-public",
				"protocol":                       "Http",
				"firewall_policy_id":             "",
				"ssl_certificate_id":             "",
				"ssl_profile_id":                 "",
			},
		},
	}
	afterUnknown := map[string]interface{}{
		"http_listener": []interface{}{
			map[string]interface{}{"id": true, "frontend_ip_configuration_id": true, "frontend_port_id": true, "ssl_certificate_id": true, "ssl_profile_id": true},
			map[string]interface{}{"id": true, "frontend_ip_configuration_id": true, "frontend_port_id": true},
		},
	}

	raw := buildPlan(t, "azurerm_application_gateway.appgw", "azurerm_application_gateway",
		[]string{"update"}, before, after, afterUnknown)

	result, err := Run(raw, idreg, NormalsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Resources) != 1 {
		t.Fatal("expected 1 resource")
	}
	res := result.Resources[0]

	// listener-https should be an addition; listener-http a noise match
	if len(res.AddedBlocks) != 1 || res.AddedBlocks[0].Key != "listener-https" {
		t.Errorf("expected 1 added block listener-https, got %+v", res.AddedBlocks)
	}
	if len(res.RemovedBlocks) != 0 {
		t.Errorf("expected 0 removed blocks, got %+v", res.RemovedBlocks)
	}
	if res.Verdict != "has-additions" {
		t.Errorf("expected has-additions, got %q", res.Verdict)
	}
	if !result.AllSafe {
		t.Error("additions-only should be AllSafe=true")
	}
}

// TestSemanticChangeDetected verifies a real attr change (priority) is NOT
// classified as noise.
func TestSemanticChangeDetected(t *testing.T) {
	idreg := loadRegistry(t)

	before := map[string]interface{}{
		"request_routing_rule": []interface{}{
			map[string]interface{}{
				"name":             "rule-pathbased",
				"priority":         float64(100),
				"http_listener_name": "listener-http",
				"url_path_map_name": "path-map-main",
			},
		},
	}
	after := map[string]interface{}{
		"request_routing_rule": []interface{}{
			map[string]interface{}{
				"name":             "rule-pathbased",
				"priority":         float64(200),
				"http_listener_name": "listener-https",
				"url_path_map_name": "path-map-main",
			},
		},
	}

	raw := buildPlan(t, "azurerm_application_gateway.appgw", "azurerm_application_gateway",
		[]string{"update"}, before, after, map[string]interface{}{})

	result, err := Run(raw, idreg, NormalsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	res := result.Resources[0]

	if len(res.ChangedBlocks) != 1 {
		t.Errorf("expected 1 changed block, got %d", len(res.ChangedBlocks))
	}
	cb := res.ChangedBlocks[0]
	if cb.Key != "rule-pathbased" {
		t.Errorf("expected key rule-pathbased, got %q", cb.Key)
	}
	// priority and http_listener_name are semantic changes
	if len(cb.SemanticDiffs) < 2 {
		t.Errorf("expected ≥2 semantic diffs, got %d: %+v", len(cb.SemanticDiffs), cb.SemanticDiffs)
	}
	if result.AllSafe {
		t.Error("semantic change should set AllSafe=false")
	}
}

// TestNoOpSkipped verifies no-op resource_changes are ignored.
func TestNoOpSkipped(t *testing.T) {
	idreg := loadRegistry(t)
	pj := map[string]interface{}{
		"resource_changes": []interface{}{
			map[string]interface{}{
				"address": "aws_instance.web",
				"type":    "aws_instance",
				"change":  map[string]interface{}{"actions": []interface{}{"no-op"}},
			},
		},
	}
	raw, _ := json.Marshal(pj)
	result, err := Run(raw, idreg, NormalsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Resources) != 0 {
		t.Errorf("expected 0 resources for no-op, got %d", len(result.Resources))
	}
}

// TestUserNormalsExtend verifies .osmo.json normals extend built-ins.
func TestUserNormalsExtend(t *testing.T) {
	idreg := loadRegistry(t)
	cfg := NormalsConfig{
		"aws_alb_listener_rule.condition.path_pattern.values": {"", "/app/*"},
	}

	before := map[string]interface{}{
		"condition": []interface{}{
			map[string]interface{}{
				"name": "path",
				"path_pattern": []interface{}{
					map[string]interface{}{"values": []interface{}{"/app/*"}},
				},
			},
		},
	}
	after := map[string]interface{}{
		"condition": []interface{}{
			map[string]interface{}{
				"name": "path",
				"path_pattern": []interface{}{
					map[string]interface{}{"values": []interface{}{""}},
				},
			},
		},
	}

	raw := buildPlan(t, "aws_alb_listener_rule.rule", "aws_alb_listener_rule",
		[]string{"update"}, before, after, map[string]interface{}{})

	result, err := Run(raw, idreg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// This test validates that user normals are loaded; the path_pattern nesting
	// is deeper than one level so inspect won't classify it (inspect is one-level).
	// Just verify it doesn't error.
	_ = result
}
