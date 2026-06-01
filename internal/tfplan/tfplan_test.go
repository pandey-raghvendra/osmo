package tfplan

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// ---- ParseDrift ------------------------------------------------------------

func TestParseDrift_Empty(t *testing.T) {
	raw := `{"resource_drift":[],"configuration":{"root_module":{}}}`
	drifts, err := ParseDrift([]byte(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(drifts) != 0 {
		t.Fatalf("want 0 drifts, got %d", len(drifts))
	}
}

func TestParseDrift_NoDriftKey(t *testing.T) {
	raw := `{"configuration":{"root_module":{}}}`
	drifts, err := ParseDrift([]byte(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(drifts) != 0 {
		t.Fatalf("want 0 drifts, got %d", len(drifts))
	}
}

func TestParseDrift_SingleResource(t *testing.T) {
	raw := `{
		"resource_drift": [{
			"address": "aws_instance.web",
			"type": "aws_instance",
			"name": "web",
			"change": {
				"before": {"instance_type": "t3.micro"},
				"after":  {"instance_type": "t3.large"},
				"after_sensitive": false
			}
		}]
	}`
	drifts, err := ParseDrift([]byte(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(drifts) != 1 {
		t.Fatalf("want 1 drift, got %d", len(drifts))
	}
	d := drifts[0]
	if d.Address != "aws_instance.web" {
		t.Errorf("address: got %q", d.Address)
	}
	if d.Type != "aws_instance" || d.Name != "web" {
		t.Errorf("type/name: got %q/%q", d.Type, d.Name)
	}
	before, ok := d.Before["instance_type"].AsString()
	if !ok || before != "t3.micro" {
		t.Errorf("before instance_type: got %v", d.Before["instance_type"])
	}
	after, ok := d.After["instance_type"].AsString()
	if !ok || after != "t3.large" {
		t.Errorf("after instance_type: got %v", d.After["instance_type"])
	}
}

func TestParseDrift_MultipleResources(t *testing.T) {
	raw := `{
		"resource_drift": [
			{"address":"aws_instance.web","type":"aws_instance","name":"web",
			 "change":{"before":{"instance_type":"t3.micro"},"after":{"instance_type":"t3.large"},"after_sensitive":false}},
			{"address":"aws_s3_bucket.data","type":"aws_s3_bucket","name":"data",
			 "change":{"before":{"tags":{"env":"dev"}},"after":{"tags":{"env":"prod"}},"after_sensitive":false}}
		]
	}`
	drifts, err := ParseDrift([]byte(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(drifts) != 2 {
		t.Fatalf("want 2 drifts, got %d", len(drifts))
	}
	if drifts[0].Address != "aws_instance.web" {
		t.Errorf("first drift address: %q", drifts[0].Address)
	}
	if drifts[1].Address != "aws_s3_bucket.data" {
		t.Errorf("second drift address: %q", drifts[1].Address)
	}
}

func TestParseDrift_WithSensitiveAttr(t *testing.T) {
	raw := `{
		"resource_drift": [{
			"address": "aws_db_instance.db",
			"type": "aws_db_instance",
			"name": "db",
			"change": {
				"before": {"password": "old"},
				"after":  {"password": "new"},
				"after_sensitive": {"password": true}
			}
		}]
	}`
	drifts, err := ParseDrift([]byte(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(drifts) != 1 {
		t.Fatal("want 1 drift")
	}
	d := drifts[0]
	// after_sensitive must be present and carry the sensitivity flag.
	obj, ok := d.AfterSensitive.AsObject()
	if !ok {
		t.Fatalf("AfterSensitive should be an object, got kind=%v", d.AfterSensitive.Kind())
	}
	b, ok := obj["password"].AsBool()
	if !ok || !b {
		t.Errorf("password should be sensitive, got %v", obj["password"])
	}
}

func TestParseDrift_ModuleAddress(t *testing.T) {
	raw := `{
		"resource_drift": [{
			"address": "module.app.aws_instance.web",
			"type": "aws_instance",
			"name": "web",
			"change": {
				"before": {"instance_type": "t3.micro"},
				"after":  {"instance_type": "t3.large"},
				"after_sensitive": false
			}
		}]
	}`
	drifts, err := ParseDrift([]byte(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(drifts) != 1 {
		t.Fatal("want 1 drift")
	}
	if drifts[0].Address != "module.app.aws_instance.web" {
		t.Errorf("address: %q", drifts[0].Address)
	}
}

func TestParseDrift_InvalidJSON(t *testing.T) {
	_, err := ParseDrift([]byte(`{not valid json`))
	if err == nil {
		t.Fatal("want error for invalid JSON")
	}
}

func fmtCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestFmt(t *testing.T) {
	ugly := []byte(`resource "aws_instance" "web" {
instance_type="t3.micro"
  ami="ami-12345"
}
`)
	got, err := Fmt(fmtCtx(t), "terraform", ugly)
	if err != nil {
		t.Skipf("terraform fmt unavailable: %v", err)
	}
	if bytes.Equal(got, ugly) {
		t.Fatal("fmt should have changed the formatting")
	}
	if !strings.Contains(string(got), `instance_type = "t3.micro"`) {
		t.Fatalf("formatted output missing expected content:\n%s", got)
	}
}

func TestFmtUnchanged(t *testing.T) {
	clean := []byte(`resource "aws_instance" "web" {
  instance_type = "t3.micro"
  ami           = "ami-12345"
}
`)
	got, err := Fmt(fmtCtx(t), "terraform", clean)
	if err != nil {
		t.Skipf("terraform fmt unavailable: %v", err)
	}
	if !bytes.Equal(got, clean) {
		t.Fatalf("fmt changed already-clean HCL:\nbefore: %s\nafter:  %s", clean, got)
	}
}

func TestFmtFallbackOnInvalidBin(t *testing.T) {
	src := []byte(`resource "aws_instance" "web" { ami = "x" }`)
	_, err := Fmt(context.Background(), "no-such-terraform-binary-xyz", src)
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestIsActionable(t *testing.T) {
	cases := []struct {
		actions []string
		want    bool
	}{
		{nil, false},
		{[]string{"no-op"}, false},
		{[]string{"read"}, false},
		{[]string{"update"}, true},
		{[]string{"create"}, true},
		{[]string{"delete"}, true},
		{[]string{"delete", "create"}, true}, // replace
		{[]string{"no-op", "read"}, false},
	}
	for _, c := range cases {
		if got := isActionable(c.actions); got != c.want {
			t.Errorf("isActionable(%v) = %v, want %v", c.actions, got, c.want)
		}
	}
}
