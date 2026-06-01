package triage

import (
	"testing"

	"github.com/pandey-raghvendra/osmo/internal/tfplan"
)

// ---- changedAttrs ----------------------------------------------------------

func TestChangedAttrs_SimpleScalar(t *testing.T) {
	before := tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.micro"})
	after := tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.large"})
	got := changedAttrs(before, after)
	if len(got) != 1 || got[0] != "instance_type" {
		t.Errorf("want [instance_type], got %v", got)
	}
}

func TestChangedAttrs_NewAttr(t *testing.T) {
	before := tfplan.TFStateFrom(map[string]interface{}{})
	after := tfplan.TFStateFrom(map[string]interface{}{"ingress": []interface{}{}})
	got := changedAttrs(before, after)
	if len(got) != 1 || got[0] != "ingress" {
		t.Errorf("want [ingress], got %v", got)
	}
}

func TestChangedAttrs_RemovedAttr(t *testing.T) {
	before := tfplan.TFStateFrom(map[string]interface{}{"description": "old"})
	after := tfplan.TFStateFrom(map[string]interface{}{})
	got := changedAttrs(before, after)
	if len(got) != 1 || got[0] != "description" {
		t.Errorf("want [description], got %v", got)
	}
}

func TestChangedAttrs_UnchangedAttr(t *testing.T) {
	before := tfplan.TFStateFrom(map[string]interface{}{"ami": "ami-123", "instance_type": "t3.micro"})
	after := tfplan.TFStateFrom(map[string]interface{}{"ami": "ami-123", "instance_type": "t3.large"})
	got := changedAttrs(before, after)
	if len(got) != 1 || got[0] != "instance_type" {
		t.Errorf("want [instance_type], got %v", got)
	}
}

func TestChangedAttrs_Sorted(t *testing.T) {
	before := tfplan.TFStateFrom(map[string]interface{}{"z": "1", "a": "1", "m": "1"})
	after := tfplan.TFStateFrom(map[string]interface{}{"z": "2", "a": "2", "m": "2"})
	got := changedAttrs(before, after)
	if len(got) != 3 || got[0] != "a" || got[1] != "m" || got[2] != "z" {
		t.Errorf("want [a m z] sorted, got %v", got)
	}
}

// ---- classify: Safe cases --------------------------------------------------

func TestClassify_TagChange_Safe(t *testing.T) {
	d := drift("aws_instance", "web",
		map[string]interface{}{"tags": map[string]interface{}{"env": "dev"}},
		map[string]interface{}{"tags": map[string]interface{}{"env": "prod"}},
	)
	v := classify(d, buildRegistry(Config{}))
	if v.Severity != Safe {
		t.Errorf("tag change should be Safe, got %s (reasons: %v)", v.Severity, v.Reasons)
	}
}

func TestClassify_InstanceTypeChange_Safe(t *testing.T) {
	d := drift("aws_instance", "web",
		map[string]interface{}{"instance_type": "t3.micro"},
		map[string]interface{}{"instance_type": "t3.large"},
	)
	v := classify(d, buildRegistry(Config{}))
	if v.Severity != Safe {
		t.Errorf("instance_type change should be Safe, got %s", v.Severity)
	}
}

func TestClassify_DescriptionChange_Safe(t *testing.T) {
	d := drift("aws_security_group", "sg",
		map[string]interface{}{"description": "old desc"},
		map[string]interface{}{"description": "new desc"},
	)
	// description pattern overrides the security_group resource-level flag
	// for the attributes that match safeAttrPatterns.
	// BUT: aws_security_group is a flagged RESOURCE TYPE — whole resource is flagged.
	v := classify(d, buildRegistry(Config{}))
	// Even if only description changed, resource-level flag applies.
	if v.Severity != Flag {
		t.Errorf("security_group resource should be Flag regardless of attr, got %s", v.Severity)
	}
}

// ---- classify: Review cases ------------------------------------------------

func TestClassify_DesiredCapacity_Review(t *testing.T) {
	d := drift("aws_autoscaling_group", "asg",
		map[string]interface{}{"desired_capacity": float64(3)},
		map[string]interface{}{"desired_capacity": float64(7)},
	)
	v := classify(d, buildRegistry(Config{}))
	if v.Severity != Review {
		t.Errorf("desired_capacity should be Review, got %s", v.Severity)
	}
	if len(v.FlaggedAttrs) == 0 {
		t.Error("FlaggedAttrs should be set for Review")
	}
	if v.Suggestion == "" {
		t.Error("Suggestion should be set for Review")
	}
}

func TestClassify_ReplicaCount_Review(t *testing.T) {
	d := drift("kubernetes_deployment", "api",
		map[string]interface{}{"replica_count": float64(2)},
		map[string]interface{}{"replica_count": float64(5)},
	)
	v := classify(d, buildRegistry(Config{}))
	if v.Severity != Review {
		t.Errorf("replica_count should be Review, got %s", v.Severity)
	}
}

// ---- classify: Flag cases --------------------------------------------------

func TestClassify_SecurityGroupResource_Flag(t *testing.T) {
	d := drift("aws_security_group", "public",
		map[string]interface{}{"ingress": []interface{}{}},
		map[string]interface{}{"ingress": []interface{}{map[string]interface{}{"from_port": float64(8080)}}},
	)
	v := classify(d, buildRegistry(Config{}))
	if v.Severity != Flag {
		t.Errorf("aws_security_group should always be Flag, got %s", v.Severity)
	}
	if v.Suggestion == "" {
		t.Error("Suggestion should be set for Flag")
	}
}

func TestClassify_IAMPolicy_Flag(t *testing.T) {
	d := drift("aws_iam_policy", "admin",
		map[string]interface{}{"policy": `{"Version":"2012-10-17"}`},
		map[string]interface{}{"policy": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*"}]}`},
	)
	v := classify(d, buildRegistry(Config{}))
	if v.Severity != Flag {
		t.Errorf("aws_iam_policy should be Flag, got %s", v.Severity)
	}
}

func TestClassify_CIDRAttr_Flag(t *testing.T) {
	d := drift("aws_instance", "web",
		map[string]interface{}{"cidr_block": "10.0.0.0/8"},
		map[string]interface{}{"cidr_block": "0.0.0.0/0"},
	)
	v := classify(d, buildRegistry(Config{}))
	if v.Severity != Flag {
		t.Errorf("cidr_block attr should be Flag, got %s", v.Severity)
	}
}

func TestClassify_GCPFirewall_Flag(t *testing.T) {
	d := drift("google_compute_firewall", "allow-http",
		map[string]interface{}{"allow": []interface{}{}},
		map[string]interface{}{"allow": []interface{}{map[string]interface{}{"ports": []interface{}{"80"}}}},
	)
	v := classify(d, buildRegistry(Config{}))
	if v.Severity != Flag {
		t.Errorf("google_compute_firewall should be Flag, got %s", v.Severity)
	}
}

func TestClassify_AzureNSG_Flag(t *testing.T) {
	d := drift("azurerm_network_security_group", "nsg",
		map[string]interface{}{"security_rule": []interface{}{}},
		map[string]interface{}{"security_rule": []interface{}{map[string]interface{}{"priority": float64(100)}}},
	)
	v := classify(d, buildRegistry(Config{}))
	if v.Severity != Flag {
		t.Errorf("azurerm_network_security_group should be Flag, got %s", v.Severity)
	}
}

// ---- classify: Config overrides --------------------------------------------

func TestClassify_CustomFlagResource(t *testing.T) {
	cfg := Config{FlagResources: []string{"my_custom_firewall"}}
	d := drift("my_custom_firewall", "fw",
		map[string]interface{}{"rule": "old"},
		map[string]interface{}{"rule": "new"},
	)
	v := classify(d, buildRegistry(cfg))
	if v.Severity != Flag {
		t.Errorf("custom flagged resource should be Flag, got %s", v.Severity)
	}
}

func TestClassify_CustomSafeAttr(t *testing.T) {
	cfg := Config{SafeAttrs: []string{"my_annotation"}}
	d := drift("aws_instance", "web",
		map[string]interface{}{"my_annotation": "v1"},
		map[string]interface{}{"my_annotation": "v2"},
	)
	v := classify(d, buildRegistry(cfg))
	if v.Severity != Safe {
		t.Errorf("custom safe attr should be Safe, got %s", v.Severity)
	}
}

func TestClassify_CustomFlagAttr(t *testing.T) {
	cfg := Config{FlagAttrs: []string{"secret_arn"}}
	d := drift("aws_instance", "web",
		map[string]interface{}{"secret_arn": "arn:aws:old"},
		map[string]interface{}{"secret_arn": "arn:aws:new"},
	)
	v := classify(d, buildRegistry(cfg))
	if v.Severity != Flag {
		t.Errorf("custom flagged attr should be Flag, got %s", v.Severity)
	}
}

// ---- Run / Summary ---------------------------------------------------------

func TestRun_MixedDrifts(t *testing.T) {
	drifts := []tfplan.Drift{
		drift("aws_instance", "web",
			map[string]interface{}{"instance_type": "t3.micro"},
			map[string]interface{}{"instance_type": "t3.large"},
		),
		drift("aws_security_group", "public",
			map[string]interface{}{"ingress": nil},
			map[string]interface{}{"ingress": []interface{}{}},
		),
		drift("aws_autoscaling_group", "asg",
			map[string]interface{}{"desired_capacity": float64(3)},
			map[string]interface{}{"desired_capacity": float64(7)},
		),
	}
	result := Run(drifts, ".", Config{})
	if result.Summary.Safe != 1 {
		t.Errorf("want 1 safe, got %d", result.Summary.Safe)
	}
	if result.Summary.Flag != 1 {
		t.Errorf("want 1 flag, got %d", result.Summary.Flag)
	}
	if result.Summary.Review != 1 {
		t.Errorf("want 1 review, got %d", result.Summary.Review)
	}
}

func TestRun_NoDrift(t *testing.T) {
	result := Run(nil, ".", Config{})
	if result.Summary.Safe+result.Summary.Review+result.Summary.Flag != 0 {
		t.Error("empty drift should produce empty summary")
	}
	if result.SuggestedCommand != "" {
		t.Error("no drift → no suggested command")
	}
}

// ---- suggestCmd ------------------------------------------------------------

func TestSuggestCmd_AllSafe(t *testing.T) {
	verdicts := []Verdict{
		{Address: "aws_instance.web", Severity: Safe},
		{Address: "aws_s3_bucket.logs", Severity: Safe},
	}
	got := suggestCmd("./infra", verdicts)
	if got != "osmo -dir ./infra -write" {
		t.Errorf("all safe → simple command, got %q", got)
	}
}

func TestSuggestCmd_MixedSafeAndFlag(t *testing.T) {
	verdicts := []Verdict{
		{Address: "aws_instance.web", Severity: Safe},
		{Address: "aws_security_group.pub", Severity: Flag},
	}
	got := suggestCmd("./infra", verdicts)
	if got == "" {
		t.Error("should produce a command when some are safe")
	}
	if !contains(got, "-target aws_instance.web") {
		t.Errorf("safe resource should be in -target: %q", got)
	}
	if !contains(got, "-exclude aws_security_group.pub") {
		t.Errorf("flagged resource should be in -exclude: %q", got)
	}
}

func TestSuggestCmd_NoSafe(t *testing.T) {
	verdicts := []Verdict{
		{Address: "aws_security_group.pub", Severity: Flag},
	}
	got := suggestCmd(".", verdicts)
	if got != "" {
		t.Errorf("no safe resources → no suggested command, got %q", got)
	}
}

func TestSuggestCmd_ReviewNotIncluded(t *testing.T) {
	verdicts := []Verdict{
		{Address: "aws_instance.web", Severity: Safe},
		{Address: "aws_autoscaling_group.asg", Severity: Review},
	}
	got := suggestCmd(".", verdicts)
	if contains(got, "aws_autoscaling_group") {
		t.Errorf("review resource should not appear in suggested command: %q", got)
	}
}

// ---- Severity.String -------------------------------------------------------

func TestSeverityString(t *testing.T) {
	cases := []struct {
		s    Severity
		want string
	}{
		{Safe, "safe"},
		{Review, "review"},
		{Flag, "flag"},
	}
	for _, c := range cases {
		if c.s.String() != c.want {
			t.Errorf("Severity(%d).String() = %q, want %q", c.s, c.s.String(), c.want)
		}
	}
}

// ---- helpers ---------------------------------------------------------------

func drift(resType, resName string, before, after map[string]interface{}) tfplan.Drift {
	return tfplan.Drift{
		Address: resType + "." + resName,
		Type:    resType,
		Name:    resName,
		Before:  tfplan.TFStateFrom(before),
		After:   tfplan.TFStateFrom(after),
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
