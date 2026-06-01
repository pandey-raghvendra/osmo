package provenance

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pandey-raghvendra/osmo/internal/address"
	"github.com/pandey-raghvendra/osmo/internal/config"
)

// ---- helpers ---------------------------------------------------------------

func parseModule(t *testing.T, raw string) *config.Module {
	t.Helper()
	m, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	return m
}

func mustAddr(t *testing.T, s string) address.Addr {
	t.Helper()
	a, err := address.Parse(s)
	if err != nil {
		t.Fatalf("address.Parse(%q): %v", s, err)
	}
	return a
}

// rootWith builds a minimal plan JSON for one resource in the root module.
func rootWith(resType, resName string, expressions map[string]interface{}) string {
	exprs, _ := json.Marshal(expressions)
	return `{"configuration":{"root_module":{"resources":[` +
		`{"address":"` + resType + `.` + resName + `","mode":"managed","type":"` + resType + `","name":"` + resName + `",` +
		`"expressions":` + string(exprs) + `}]}}}`
}

// ---- Trace -----------------------------------------------------------------

func TestTrace_ConstantLiteral(t *testing.T) {
	root := parseModule(t, rootWith("aws_instance", "web",
		map[string]interface{}{
			"instance_type": map[string]interface{}{"constant_value": "t3.micro"},
		}))

	addr := mustAddr(t, "aws_instance.web")
	tgt, u := Trace(root, addr, "instance_type", "t3.large")

	if u != nil {
		t.Fatalf("unexpected unresolved: %v", u)
	}
	if tgt.Kind != ResourceAttr {
		t.Fatalf("want ResourceAttr, got %v", tgt.Kind)
	}
	if tgt.Attr != "instance_type" {
		t.Errorf("want attr=instance_type, got %q", tgt.Attr)
	}
	if tgt.Value != "t3.large" {
		t.Errorf("want value=t3.large, got %v", tgt.Value)
	}
	if tgt.ResourceType != "aws_instance" || tgt.ResourceName != "web" {
		t.Errorf("unexpected resource %s.%s", tgt.ResourceType, tgt.ResourceName)
	}
}

func TestTrace_ConstantLiteral_MultiInstance_Unresolvable(t *testing.T) {
	root := parseModule(t, rootWith("aws_instance", "web",
		map[string]interface{}{
			"instance_type": map[string]interface{}{"constant_value": "t3.micro"},
		}))

	// Indexed instance: shared constant → cannot isolate.
	addr := mustAddr(t, `aws_instance.web["a"]`)
	_, u := Trace(root, addr, "instance_type", "t3.large")

	if u == nil {
		t.Fatal("want unresolved for multi-instance constant, got nil")
	}
	if !strings.Contains(u.Reason, "cannot isolate") {
		t.Errorf("unexpected reason: %q", u.Reason)
	}
}

func TestTrace_VarReference_RootDefault(t *testing.T) {
	raw := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web",
			"expressions":{"instance_type":{"references":["var.size"]}}}],
		"variables":{"size":{"default":"t3.micro"}}
	}}}`
	root := parseModule(t, raw)
	addr := mustAddr(t, "aws_instance.web")
	tgt, u := Trace(root, addr, "instance_type", "t3.large")

	if u != nil {
		t.Fatalf("unexpected unresolved: %v", u)
	}
	if tgt.Kind != VariableDefault {
		t.Fatalf("want VariableDefault, got %v", tgt.Kind)
	}
	if tgt.VarName != "size" {
		t.Errorf("want VarName=size, got %q", tgt.VarName)
	}
}

func TestTrace_VarReference_NoDefault_Unresolvable(t *testing.T) {
	raw := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web",
			"expressions":{"instance_type":{"references":["var.size"]}}}],
		"variables":{"size":{}}
	}}}`
	root := parseModule(t, raw)
	addr := mustAddr(t, "aws_instance.web")
	_, u := Trace(root, addr, "instance_type", "t3.large")

	if u == nil {
		t.Fatal("want unresolved when root var has no default")
	}
}

func TestTrace_ModuleArg(t *testing.T) {
	raw := `{"configuration":{"root_module":{
		"module_calls":{
			"app":{
				"source":"./modules/app",
				"expressions":{"size":{"constant_value":"t3.micro"}},
				"module":{
					"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web",
						"expressions":{"instance_type":{"references":["var.size"]}}}],
					"variables":{"size":{"default":"t3.small"}}
				}
			}
		}
	}}}`
	root := parseModule(t, raw)
	addr := mustAddr(t, "module.app.aws_instance.web")
	tgt, u := Trace(root, addr, "instance_type", "t3.large")

	if u != nil {
		t.Fatalf("unexpected unresolved: %v", u)
	}
	if tgt.Kind != ModuleArg {
		t.Fatalf("want ModuleArg, got %v", tgt.Kind)
	}
	if tgt.CallName != "app" || tgt.Attr != "size" {
		t.Errorf("want call=app attr=size, got call=%q attr=%q", tgt.CallName, tgt.Attr)
	}
}

func TestTrace_ModuleArg_MultiHop(t *testing.T) {
	// root → module.outer(var.sz) → module.inner(var.sz) → resource(var.sz)
	raw := `{"configuration":{"root_module":{
		"module_calls":{
			"outer":{
				"source":"./modules/outer",
				"expressions":{"sz":{"constant_value":"t3.nano"}},
				"module":{
					"module_calls":{
						"inner":{
							"source":"./modules/inner",
							"expressions":{"sz":{"references":["var.sz"]}},
							"module":{
								"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web",
									"expressions":{"instance_type":{"references":["var.sz"]}}}],
								"variables":{"sz":{}}
							}
						}
					},
					"variables":{"sz":{}}
				}
			}
		}
	}}}`
	root := parseModule(t, raw)
	addr := mustAddr(t, "module.outer.module.inner.aws_instance.web")
	tgt, u := Trace(root, addr, "instance_type", "t3.large")

	if u != nil {
		t.Fatalf("unexpected unresolved: %v", u)
	}
	if tgt.Kind != ModuleArg {
		t.Fatalf("want ModuleArg at root call, got %v", tgt.Kind)
	}
	if tgt.CallName != "outer" {
		t.Errorf("want call=outer (root module arg), got %q", tgt.CallName)
	}
}

func TestTrace_ForEachMapEntry_ObjectAttr(t *testing.T) {
	raw := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web",
			"expressions":{"instance_type":{"references":["each.value.size"]}}}]
	}}}`
	root := parseModule(t, raw)
	addr := mustAddr(t, `aws_instance.web["prod"]`)
	tgt, u := Trace(root, addr, "instance_type", "t3.large")

	if u != nil {
		t.Fatalf("unexpected unresolved: %v", u)
	}
	if tgt.Kind != ForEachMapEntry {
		t.Fatalf("want ForEachMapEntry, got %v", tgt.Kind)
	}
	if tgt.Attr != "size" {
		t.Errorf("want attr=size (map key), got %q", tgt.Attr)
	}
	if tgt.InstanceKey == nil || *tgt.InstanceKey != "prod" {
		t.Errorf("want InstanceKey=prod, got %v", tgt.InstanceKey)
	}
}

func TestTrace_ForEachMapEntry_Scalar(t *testing.T) {
	raw := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web",
			"expressions":{"instance_type":{"references":["each.value"]}}}]
	}}}`
	root := parseModule(t, raw)
	addr := mustAddr(t, `aws_instance.web["prod"]`)
	tgt, u := Trace(root, addr, "instance_type", "t3.large")

	if u != nil {
		t.Fatalf("unexpected unresolved: %v", u)
	}
	if tgt.Kind != ForEachMapEntry {
		t.Fatalf("want ForEachMapEntry, got %v", tgt.Kind)
	}
	if tgt.Attr != "" {
		t.Errorf("want empty Attr (scalar entry), got %q", tgt.Attr)
	}
}

func TestTrace_ComposedExpression_Unresolvable(t *testing.T) {
	root := parseModule(t, rootWith("aws_instance", "web",
		map[string]interface{}{
			"tags": map[string]interface{}{"references": []string{"var.a", "var.b"}},
		}))
	addr := mustAddr(t, "aws_instance.web")
	_, u := Trace(root, addr, "tags", "x")
	if u == nil {
		t.Fatal("want unresolved for composed expression")
	}
	if !strings.Contains(u.Reason, "multiple references") {
		t.Errorf("unexpected reason: %q", u.Reason)
	}
}

func TestTrace_LocalReference_Unresolvable(t *testing.T) {
	root := parseModule(t, rootWith("aws_instance", "web",
		map[string]interface{}{
			"tags": map[string]interface{}{"references": []string{"local.common_tags"}},
		}))
	addr := mustAddr(t, "aws_instance.web")
	_, u := Trace(root, addr, "tags", "x")
	if u == nil {
		t.Fatal("want unresolved for local reference")
	}
}

func TestTrace_ResourceNotFound_Unresolvable(t *testing.T) {
	root := parseModule(t, `{"configuration":{"root_module":{"resources":[]}}}`)
	addr := mustAddr(t, "aws_instance.web")
	_, u := Trace(root, addr, "instance_type", "t3.large")
	if u == nil {
		t.Fatal("want unresolved when resource missing from config")
	}
}

func TestTrace_AttrNotInConfig_Unresolvable(t *testing.T) {
	root := parseModule(t, rootWith("aws_instance", "web",
		map[string]interface{}{
			"instance_type": map[string]interface{}{"constant_value": "t3.micro"},
		}))
	addr := mustAddr(t, "aws_instance.web")
	_, u := Trace(root, addr, "ami", "ami-12345")
	if u == nil {
		t.Fatal("want unresolved when attr missing from config expressions")
	}
}

// ---- TraceForEach ----------------------------------------------------------

func TestTraceForEach_ResolvesToVarDefault(t *testing.T) {
	raw := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_security_group.sg","mode":"managed","type":"aws_security_group","name":"sg",
			"expressions":{}}],
		"variables":{"rules":{"default":[{"port":80}]}}
	}}}`
	root := parseModule(t, raw)
	addr := mustAddr(t, "aws_security_group.sg")
	after := []interface{}{map[string]interface{}{"port": 443}}
	tgt, u := TraceForEach(root, addr, "rules", after)

	if u != nil {
		t.Fatalf("unexpected unresolved: %v", u)
	}
	if tgt.Kind != VariableDefault {
		t.Fatalf("want VariableDefault, got %v", tgt.Kind)
	}
}

// ---- singleVarRef ----------------------------------------------------------

func TestSingleVarRef_Simple(t *testing.T) {
	name, uerr := singleVarRef([]string{"var.size"})
	if uerr != "" {
		t.Fatalf("unexpected error: %q", uerr)
	}
	if name != "size" {
		t.Errorf("want name=size, got %q", name)
	}
}

func TestSingleVarRef_Empty(t *testing.T) {
	_, uerr := singleVarRef([]string{})
	if uerr == "" {
		t.Fatal("want error for empty refs")
	}
}

func TestSingleVarRef_Multiple(t *testing.T) {
	_, uerr := singleVarRef([]string{"var.a", "var.b"})
	if uerr == "" {
		t.Fatal("want error for multiple refs")
	}
}

func TestSingleVarRef_Local(t *testing.T) {
	_, uerr := singleVarRef([]string{"local.foo"})
	if uerr == "" || !strings.Contains(uerr, "local") {
		t.Fatalf("want local error, got %q", uerr)
	}
}

func TestSingleVarRef_Each(t *testing.T) {
	_, uerr := singleVarRef([]string{"each.key"})
	if uerr == "" {
		t.Fatal("want error for each reference")
	}
}

func TestSingleVarRef_Count(t *testing.T) {
	_, uerr := singleVarRef([]string{"count.index"})
	if uerr == "" {
		t.Fatal("want error for count reference")
	}
}

func TestSingleVarRef_ResourceRef(t *testing.T) {
	_, uerr := singleVarRef([]string{"aws_instance.other.id"})
	if uerr == "" {
		t.Fatal("want error for resource output reference")
	}
}

func TestSingleVarRef_SubAttr(t *testing.T) {
	_, uerr := singleVarRef([]string{"var.obj.field"})
	if uerr == "" || !strings.Contains(uerr, "sub-attribute") {
		t.Fatalf("want sub-attribute error, got %q", uerr)
	}
}

// ---- eachValueMapAttr ------------------------------------------------------

func TestEachValueMapAttr_ObjectAttr(t *testing.T) {
	attr, ok := eachValueMapAttr([]string{"each.value.instance_type"})
	if !ok || attr != "instance_type" {
		t.Errorf("want (instance_type, true), got (%q, %v)", attr, ok)
	}
}

func TestEachValueMapAttr_Scalar(t *testing.T) {
	attr, ok := eachValueMapAttr([]string{"each.value"})
	if !ok || attr != "" {
		t.Errorf("want (\"\", true), got (%q, %v)", attr, ok)
	}
}

func TestEachValueMapAttr_None(t *testing.T) {
	_, ok := eachValueMapAttr([]string{"var.x"})
	if ok {
		t.Error("want false when no each.value ref present")
	}
}

func TestEachValueMapAttr_NestedSubAttr_Ignored(t *testing.T) {
	// each.value.obj.field has a dot inside — not a simple attr, ignored.
	_, ok := eachValueMapAttr([]string{"each.value.obj.field"})
	if ok {
		t.Error("want false for deep sub-attr each.value.obj.field")
	}
}

// ---- isCountKey ------------------------------------------------------------

func TestIsCountKey(t *testing.T) {
	cases := []struct {
		k    string
		want bool
	}{
		{"0", true},
		{"42", true},
		{"prod", false},
		{"", false},
		{"1a", false},
	}
	for _, c := range cases {
		if got := isCountKey(c.k); got != c.want {
			t.Errorf("isCountKey(%q) = %v, want %v", c.k, got, c.want)
		}
	}
}

// ---- quoteKey --------------------------------------------------------------

func TestQuoteKey(t *testing.T) {
	cases := []struct {
		k    string
		want string
	}{
		{"0", "0"},           // numeric → bare
		{"prod", `"prod"`},   // string → quoted
		{"", ""},             // empty → empty
		{"123", "123"},       // all digits → bare
	}
	for _, c := range cases {
		if got := quoteKey(c.k); got != c.want {
			t.Errorf("quoteKey(%q) = %q, want %q", c.k, got, c.want)
		}
	}
}

// ---- fullAddr --------------------------------------------------------------

func TestFullAddr_Simple(t *testing.T) {
	addr := mustAddr(t, "aws_instance.web")
	got := fullAddr(addr)
	if got != "aws_instance.web" {
		t.Errorf("got %q", got)
	}
}

func TestFullAddr_WithModule(t *testing.T) {
	addr := mustAddr(t, "module.app.aws_instance.web")
	got := fullAddr(addr)
	if got != "module.app.aws_instance.web" {
		t.Errorf("got %q", got)
	}
}

func TestFullAddr_WithIndex(t *testing.T) {
	addr := mustAddr(t, `aws_instance.web["prod"]`)
	got := fullAddr(addr)
	if got != `aws_instance.web["prod"]` {
		t.Errorf("got %q", got)
	}
}

func TestFullAddr_Data(t *testing.T) {
	addr := mustAddr(t, "data.aws_ami.latest")
	got := fullAddr(addr)
	if got != "data.aws_ami.latest" {
		t.Errorf("got %q", got)
	}
}
