package tfplan

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

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
