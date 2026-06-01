package tfc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectBackend_Local(t *testing.T) {
	dir := t.TempDir()
	// No .terraform directory → nil, nil.
	b, err := DetectBackend(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b != nil {
		t.Fatalf("expected nil backend for local dir, got %+v", b)
	}
}

func TestDetectBackend_LocalBackendType(t *testing.T) {
	dir := t.TempDir()
	writeTFState(t, dir, `{
		"backend": {
			"type": "local",
			"config": {}
		}
	}`)
	b, err := DetectBackend(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b != nil {
		t.Fatalf("expected nil for local backend, got %+v", b)
	}
}

func TestDetectBackend_TFCNoToken(t *testing.T) {
	dir := t.TempDir()
	writeTFState(t, dir, `{
		"backend": {
			"type": "remote",
			"config": {
				"hostname": "app.terraform.io",
				"organization": "my-org",
				"workspaces": {"name": "my-ws"}
			}
		}
	}`)
	t.Setenv("TFE_TOKEN", "") // ensure unset
	_, err := DetectBackend(dir)
	if err == nil {
		t.Fatal("expected error when TFE_TOKEN is unset")
	}
}

func TestDetectBackend_TFCWithToken(t *testing.T) {
	dir := t.TempDir()
	writeTFState(t, dir, `{
		"backend": {
			"type": "remote",
			"config": {
				"hostname": "tfe.example.com",
				"organization": "acme",
				"workspaces": {"name": "prod"}
			}
		}
	}`)
	t.Setenv("TFE_TOKEN", "test-token-abc")
	b, err := DetectBackend(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil backend")
	}
	if b.Host != "tfe.example.com" {
		t.Errorf("host: got %q, want %q", b.Host, "tfe.example.com")
	}
	if b.Organization != "acme" {
		t.Errorf("org: got %q, want %q", b.Organization, "acme")
	}
	if b.Workspace != "prod" {
		t.Errorf("workspace: got %q, want %q", b.Workspace, "prod")
	}
	if b.Token != "test-token-abc" {
		t.Errorf("token not set from env")
	}
}

func TestDetectBackend_WorkspaceEnvOverride(t *testing.T) {
	dir := t.TempDir()
	writeTFState(t, dir, `{
		"backend": {
			"type": "cloud",
			"config": {
				"organization": "acme",
				"workspaces": {"name": "base-ws"}
			}
		}
	}`)
	// Write .terraform/environment to override workspace name.
	tfDir := filepath.Join(dir, ".terraform")
	if err := os.WriteFile(filepath.Join(tfDir, "environment"), []byte("feature-branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TFE_TOKEN", "tok")
	b, err := DetectBackend(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.Workspace != "feature-branch" {
		t.Errorf("workspace override: got %q, want %q", b.Workspace, "feature-branch")
	}
}

func TestBuildTarball(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_instance" "web" {}`)
	writeFile(t, dir, "vars.tfvars", `region = "us-east-1"`)
	writeFile(t, dir, "ignored.txt", "not terraform")

	tarball, err := buildTarball(dir)
	if err != nil {
		t.Fatalf("buildTarball: %v", err)
	}

	names := tarballNames(t, tarball)
	if !contains(names, "main.tf") {
		t.Errorf("tarball missing main.tf; got %v", names)
	}
	if !contains(names, "vars.tfvars") {
		t.Errorf("tarball missing vars.tfvars; got %v", names)
	}
	if contains(names, "ignored.txt") {
		t.Errorf("tarball should not contain ignored.txt; got %v", names)
	}
}

func TestParsePlanJSON(t *testing.T) {
	raw := []byte(`{
		"resource_changes": [
			{"address": "aws_instance.web", "change": {"actions": ["update"]}},
			{"address": "aws_s3_bucket.logs", "change": {"actions": ["no-op"]}},
			{"address": "data.aws_ami.latest", "change": {"actions": ["read"]}}
		]
	}`)
	addrs, err := parsePlanJSON(raw)
	if err != nil {
		t.Fatalf("parsePlanJSON: %v", err)
	}
	if len(addrs) != 1 || addrs[0] != "aws_instance.web" {
		t.Errorf("got %v, want [aws_instance.web]", addrs)
	}
}

func TestWorkspaceURL(t *testing.T) {
	b := &Backend{
		Host:         "app.terraform.io",
		Organization: "acme",
		Workspace:    "prod",
	}
	want := "https://app.terraform.io/app/acme/workspaces/prod"
	if got := b.WorkspaceURL(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ---- helpers ----------------------------------------------------------------

func writeTFState(t *testing.T, dir, content string) {
	t.Helper()
	tfDir := filepath.Join(dir, ".terraform")
	if err := os.MkdirAll(tfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tfDir, "terraform.tfstate"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func tarballNames(t *testing.T, data []byte) []string {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		names = append(names, hdr.Name)
	}
	return names
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
