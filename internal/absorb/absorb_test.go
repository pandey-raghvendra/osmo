package absorb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/raghav/osmo/internal/tfplan"
)

// writeFile writes content to dir/name, creating parent dirs.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRootResourceLiteral(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_instance" "web" {
  instance_type = "t3.micro"
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web",
			"expressions":{"instance_type":{"constant_value":"t3.micro"}}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "aws_instance.web", Type: "aws_instance", Name: "web",
		Before: map[string]interface{}{"instance_type": "t3.micro"},
		After:  map[string]interface{}{"instance_type": "t3.large"},
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %v", unresolved)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change, got %d", len(changes))
	}
	if !strings.Contains(string(changes[0].After), `instance_type = "t3.large"`) {
		t.Errorf("not absorbed:\n%s", changes[0].After)
	}
}

func TestModuleArgChain(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `module "network" {
  source   = "./modules/network"
  location = "eastus"
}
`)
	writeFile(t, dir, "modules/network/main.tf", `variable "location" {}

resource "azurerm_resource_group" "rg" {
  location = var.location
}
`)
	cfg := `{"configuration":{"root_module":{
		"module_calls":{"network":{
			"source":"./modules/network",
			"expressions":{"location":{"constant_value":"eastus"}},
			"module":{
				"variables":{"location":{}},
				"resources":[{"address":"azurerm_resource_group.rg","mode":"managed","type":"azurerm_resource_group","name":"rg",
					"expressions":{"location":{"references":["var.location"]}}}]
			}
		}}
	}}}`
	drifts := []tfplan.Drift{{
		Address: "module.network.azurerm_resource_group.rg",
		Type:    "azurerm_resource_group", Name: "rg",
		Before: map[string]interface{}{"location": "eastus"},
		After:  map[string]interface{}{"location": "westus"},
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %v", unresolved)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change, got %d", len(changes))
	}
	// Edit must land on the ROOT module call argument, not the module source.
	if !strings.HasSuffix(changes[0].Path, "main.tf") || strings.Contains(changes[0].Path, "modules") {
		t.Errorf("edited wrong file: %s", changes[0].Path)
	}
	if !strings.Contains(string(changes[0].After), `location = "westus"`) {
		t.Errorf("module arg not absorbed:\n%s", changes[0].After)
	}
	// Module source must be untouched.
	src := mustRead(t, filepath.Join(dir, "modules/network/main.tf"))
	if !strings.Contains(src, "location = var.location") {
		t.Errorf("module source was modified:\n%s", src)
	}
}

func TestNestedModuleChain(t *testing.T) {
	dir := t.TempDir()
	// root -> module "a" (passes p) -> module "b" (passes q=var.p) -> resource uses var.q
	writeFile(t, dir, "main.tf", `module "a" {
  source = "./a"
  p      = "v1"
}
`)
	writeFile(t, dir, "a/main.tf", `variable "p" {}
module "b" {
  source = "./b"
  q      = var.p
}
`)
	writeFile(t, dir, "a/b/main.tf", `variable "q" {}
resource "aws_s3_bucket" "x" {
  bucket = var.q
}
`)
	cfg := `{"configuration":{"root_module":{
		"module_calls":{"a":{
			"source":"./a",
			"expressions":{"p":{"constant_value":"v1"}},
			"module":{
				"variables":{"p":{}},
				"module_calls":{"b":{
					"source":"./b",
					"expressions":{"q":{"references":["var.p"]}},
					"module":{
						"variables":{"q":{}},
						"resources":[{"address":"aws_s3_bucket.x","mode":"managed","type":"aws_s3_bucket","name":"x",
							"expressions":{"bucket":{"references":["var.q"]}}}]
					}
				}}
			}
		}}
	}}}`
	drifts := []tfplan.Drift{{
		Address: "module.a.module.b.aws_s3_bucket.x",
		Type:    "aws_s3_bucket", Name: "x",
		Before: map[string]interface{}{"bucket": "v1"},
		After:  map[string]interface{}{"bucket": "v2"},
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %v", unresolved)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change, got %d", len(changes))
	}
	// Must edit the ROOT call arg p, since that is where the literal lives.
	if strings.Contains(changes[0].Path, "/a/") {
		t.Errorf("edited a nested file, expected root: %s", changes[0].Path)
	}
	if !strings.Contains(string(changes[0].After), `p      = "v2"`) {
		t.Errorf("root arg not absorbed:\n%s", changes[0].After)
	}
}

func TestUnresolvedLocal(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_instance" "web" {
  instance_type = local.it
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web",
			"expressions":{"instance_type":{"references":["local.it"]}}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "aws_instance.web", Type: "aws_instance", Name: "web",
		Before: map[string]interface{}{"instance_type": "t3.micro"},
		After:  map[string]interface{}{"instance_type": "t3.large"},
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("want 0 changes, got %d", len(changes))
	}
	if len(unresolved) != 1 || !strings.Contains(unresolved[0].Reason, "local") {
		t.Fatalf("want local unresolved, got %v", unresolved)
	}
}

func TestUnresolvedRemoteModule(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `module "vpc" {
  source = "terraform-aws-modules/vpc/aws"
  cidr   = "10.0.0.0/16"
}
`)
	// cidr_block is HARDCODED inside the remote module (constant_value), so the
	// only edit point is inside the uneditable remote source.
	cfg := `{"configuration":{"root_module":{
		"module_calls":{"vpc":{
			"source":"terraform-aws-modules/vpc/aws",
			"module":{
				"resources":[{"address":"aws_vpc.this","mode":"managed","type":"aws_vpc","name":"this",
					"expressions":{"cidr_block":{"constant_value":"10.0.0.0/16"}}}]
			}
		}}
	}}}`
	drifts := []tfplan.Drift{{
		Address: "module.vpc.aws_vpc.this", Type: "aws_vpc", Name: "this",
		Before: map[string]interface{}{"cidr_block": "10.0.0.0/16"},
		After:  map[string]interface{}{"cidr_block": "10.1.0.0/16"},
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("want 0 changes (remote module), got %d", len(changes))
	}
	if len(unresolved) != 1 || !strings.Contains(unresolved[0].Reason, "non-local") {
		t.Fatalf("want non-local unresolved, got %v", unresolved)
	}
}

func TestInstancedConstantUnresolved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `module "n" {
  source = "./n"
}
`)
	writeFile(t, dir, "n/main.tf", `resource "aws_subnet" "s" {
  for_each = toset(["a", "b"])
  cidr     = "10.0.0.0/24"
}
`)
	cfg := `{"configuration":{"root_module":{
		"module_calls":{"n":{
			"source":"./n",
			"module":{
				"resources":[{"address":"aws_subnet.s","mode":"managed","type":"aws_subnet","name":"s",
					"expressions":{"cidr":{"constant_value":"10.0.0.0/24"}}}]
			}
		}}
	}}}`
	drifts := []tfplan.Drift{{
		Address: `module.n.aws_subnet.s["a"]`, Type: "aws_subnet", Name: "s",
		Before: map[string]interface{}{"cidr": "10.0.0.0/24"},
		After:  map[string]interface{}{"cidr": "10.9.0.0/24"},
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("want 0 changes (cannot isolate shared constant), got %d", len(changes))
	}
	if len(unresolved) != 1 || !strings.Contains(unresolved[0].Reason, "isolate") {
		t.Fatalf("want isolate unresolved, got %v", unresolved)
	}
}

// TestSetAttrMapEntry proves instance-scoped editing of one map entry without
// disturbing the rest (the M2 for_each scoping mechanism).
func TestSetAttrMapEntry(t *testing.T) {
	src := `module "net" {
  source = "./net"
  cidrs = {
    app = "10.0.1.0/24"
    db  = "10.0.2.0/24"
  }
}
`
	f, diags := hclwrite.ParseConfig([]byte(src), "test.tf", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		t.Fatal(diags.Error())
	}
	block := f.Body().FirstMatchingBlock("module", []string{"net"})
	key := "app"
	if err := setAttr(block, "cidrs", "10.0.9.0/24", &key); err != nil {
		t.Fatal(err)
	}
	got := string(f.Bytes())
	if !strings.Contains(got, "10.0.9.0/24") {
		t.Errorf("app entry not updated:\n%s", got)
	}
	if !strings.Contains(got, "10.0.2.0/24") {
		t.Errorf("db entry was lost:\n%s", got)
	}
}

// ---- Nested block tests ------------------------------------------------

// TestNestedBlockSingleton: one nested block, attr drifted.
func TestNestedBlockSingleton(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_instance" "web" {
  instance_type = "t3.micro"

  root_block_device {
    volume_size = 20
    volume_type = "gp2"
  }
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web",
			"expressions":{"instance_type":{"constant_value":"t3.micro"}}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "aws_instance.web", Type: "aws_instance", Name: "web",
		Before: map[string]interface{}{
			"instance_type": "t3.micro",
			"root_block_device": []interface{}{
				map[string]interface{}{"volume_size": float64(20), "volume_type": "gp2"},
			},
		},
		After: map[string]interface{}{
			"instance_type": "t3.micro",
			"root_block_device": []interface{}{
				map[string]interface{}{"volume_size": float64(50), "volume_type": "gp2"},
			},
		},
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %v", unresolved)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change, got %d", len(changes))
	}
	got := string(changes[0].After)
	if !strings.Contains(got, "volume_size = 50") {
		t.Errorf("nested block attr not absorbed:\n%s", got)
	}
	// Sibling attr unchanged.
	if !strings.Contains(got, `volume_type = "gp2"`) {
		t.Errorf("sibling attr lost:\n%s", got)
	}
}

// TestNestedBlockMultiInstanceMatchByStableAttr: two ebs_block_device blocks;
// only the one matching device_name="/dev/sda1" should be edited.
func TestNestedBlockMultiInstanceMatchByStableAttr(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_instance" "web" {
  ebs_block_device {
    device_name = "/dev/sda1"
    volume_size = 20
  }

  ebs_block_device {
    device_name = "/dev/sdb1"
    volume_size = 100
  }
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web","expressions":{}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "aws_instance.web", Type: "aws_instance", Name: "web",
		Before: map[string]interface{}{
			"ebs_block_device": []interface{}{
				map[string]interface{}{"device_name": "/dev/sda1", "volume_size": float64(20)},
				map[string]interface{}{"device_name": "/dev/sdb1", "volume_size": float64(100)},
			},
		},
		After: map[string]interface{}{
			"ebs_block_device": []interface{}{
				map[string]interface{}{"device_name": "/dev/sda1", "volume_size": float64(50)}, // drifted
				map[string]interface{}{"device_name": "/dev/sdb1", "volume_size": float64(100)},
			},
		},
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %v", unresolved)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change, got %d", len(changes))
	}
	got := string(changes[0].After)
	// sda1 block must be updated.
	if !strings.Contains(got, "volume_size = 50") {
		t.Errorf("sda1 block not absorbed:\n%s", got)
	}
	// sdb1 block must be untouched.
	if !strings.Contains(got, "volume_size = 100") {
		t.Errorf("sdb1 sibling block was modified:\n%s", got)
	}
}

// TestNestedBlockVarRefUnresolved: nested block attr is a var ref — must be
// reported as unresolved, never hardcoded.
func TestNestedBlockVarRefUnresolved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `variable "vol_size" {}

resource "aws_instance" "web" {
  root_block_device {
    volume_size = var.vol_size
  }
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web","expressions":{}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "aws_instance.web", Type: "aws_instance", Name: "web",
		Before: map[string]interface{}{"root_block_device": []interface{}{map[string]interface{}{"volume_size": float64(20)}}},
		After:  map[string]interface{}{"root_block_device": []interface{}{map[string]interface{}{"volume_size": float64(50)}}},
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("want 0 changes (var ref), got %d", len(changes))
	}
	if len(unresolved) != 1 || !strings.Contains(unresolved[0].Reason, "variable reference") {
		t.Fatalf("want variable-reference unresolved, got %v", unresolved)
	}
}

// TestNestedBlockCountChangeUnresolved: block count changed (add/remove) —
// must be reported, never silently skipped.
func TestNestedBlockCountChangeUnresolved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_security_group" "sg" {
  ingress {
    from_port = 80
    to_port   = 80
    protocol  = "tcp"
  }
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_security_group.sg","mode":"managed","type":"aws_security_group","name":"sg","expressions":{}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "aws_security_group.sg", Type: "aws_security_group", Name: "sg",
		Before: map[string]interface{}{
			"ingress": []interface{}{
				map[string]interface{}{"from_port": float64(80), "to_port": float64(80)},
			},
		},
		After: map[string]interface{}{
			"ingress": []interface{}{
				map[string]interface{}{"from_port": float64(80), "to_port": float64(80)},
				map[string]interface{}{"from_port": float64(443), "to_port": float64(443)}, // added out-of-band
			},
		},
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("want 0 changes (count change), got %d", len(changes))
	}
	if len(unresolved) != 1 || !strings.Contains(unresolved[0].Reason, "count changed") {
		t.Fatalf("want count-change unresolved, got %v", unresolved)
	}
}
