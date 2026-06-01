package absorb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/pandey-raghvendra/osmo/internal/tfplan"
)

// ---- Safety guard tests --------------------------------------------------

// TestSensitiveAttrSkipped: drift attr marked sensitive must never be written
// to plain-text config.
func TestSensitiveAttrSkipped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_db_instance" "db" {
  password = "old-password"
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_db_instance.db","mode":"managed","type":"aws_db_instance","name":"db",
			"expressions":{"password":{"constant_value":"old-password"}}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "aws_db_instance.db", Type: "aws_db_instance", Name: "db",
		Before:         tfplan.TFStateFrom(map[string]interface{}{"password": "old-password"}),
		After:          tfplan.TFStateFrom(map[string]interface{}{"password": "new-password"}),
		AfterSensitive: tfplan.FromGoValue(map[string]interface{}{"password": true}),
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("want 0 changes (sensitive attr), got %d:\n%s", len(changes), changes[0].After)
	}
	if len(unresolved) != 1 || !strings.Contains(unresolved[0].Reason, "sensitive") {
		t.Fatalf("want 1 sensitive unresolved, got %v", unresolved)
	}
}

// TestNullAfterValueRemoved: root-level scalar with null after-value must be
// removed from config (attr deleted from reality → remove the literal).
func TestNullAfterValueRemoved(t *testing.T) {
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
		Before: tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.micro"}),
		After:  tfplan.TFStateFrom(map[string]interface{}{"instance_type": nil}), // null = removed in reality
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %v", unresolved)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change (attr removal), got %d", len(changes))
	}
	got := string(changes[0].After)
	if strings.Contains(got, "instance_type") {
		t.Fatalf("instance_type should have been removed, got:\n%s", got)
	}
}

// TestAbsentAfterValueRemoved: attr present in before but absent from after
// (key not present at all in reality) must also be removed from config.
func TestAbsentAfterValueRemoved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_instance" "web" {
  instance_type = "t3.micro"
  key_name      = "my-key"
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web",
			"expressions":{
				"instance_type":{"constant_value":"t3.micro"},
				"key_name":{"constant_value":"my-key"}
			}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "aws_instance.web", Type: "aws_instance", Name: "web",
		Before: tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.micro", "key_name": "my-key"}),
		After:  tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.micro"}), // key_name absent = deleted from reality
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %v", unresolved)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change (absent key removal), got %d", len(changes))
	}
	got := string(changes[0].After)
	if strings.Contains(got, "key_name") {
		t.Fatalf("key_name should have been removed, got:\n%s", got)
	}
	if !strings.Contains(got, "instance_type") {
		t.Fatalf("instance_type should be preserved, got:\n%s", got)
	}
}

func TestNestedSensitiveAttrSkipped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_instance" "web" {
  root_block_device {
    volume_size = 20
  }
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web","expressions":{}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "aws_instance.web", Type: "aws_instance", Name: "web",
		Before: tfplan.TFStateFrom(map[string]interface{}{
			"root_block_device": []interface{}{map[string]interface{}{"volume_size": float64(20)}},
		}),
		After: tfplan.TFStateFrom(map[string]interface{}{
			"root_block_device": []interface{}{map[string]interface{}{"volume_size": float64(50)}},
		}),
		AfterSensitive: tfplan.FromGoValue(map[string]interface{}{
			"root_block_device": []interface{}{map[string]interface{}{"volume_size": true}},
		}),
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("want 0 changes (nested sensitive attr), got %d:\n%s", len(changes), changes[0].After)
	}
	if len(unresolved) == 0 {
		t.Fatal("want nested sensitive unresolved, got none")
	}
	found := false
	for _, u := range unresolved {
		if u.Attr == "root_block_device.volume_size" && strings.Contains(u.Reason, "sensitive") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want nested sensitive unresolved, got %v", unresolved)
	}
}

// TestNestedNullAfterValueRemoved: nested attr with explicit null after-value
// must be removed from config (same as absent key = removed from reality).
func TestNestedNullAfterValueRemoved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_instance" "web" {
  root_block_device {
    volume_size = 20
  }
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web","expressions":{}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "aws_instance.web", Type: "aws_instance", Name: "web",
		Before: tfplan.TFStateFrom(map[string]interface{}{
			"root_block_device": []interface{}{map[string]interface{}{"volume_size": float64(20)}},
		}),
		After: tfplan.TFStateFrom(map[string]interface{}{
			"root_block_device": []interface{}{map[string]interface{}{"volume_size": nil}},
		}),
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %v", unresolved)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change (nested attr removal), got %d", len(changes))
	}
	got := string(changes[0].After)
	if strings.Contains(got, "volume_size") {
		t.Fatalf("volume_size should have been removed, got:\n%s", got)
	}
}

func TestNestedMissingAfterAttrRemoved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_instance" "web" {
  root_block_device {
    volume_size = 20
    volume_type = "gp2"
  }
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web","expressions":{}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "aws_instance.web", Type: "aws_instance", Name: "web",
		Before: tfplan.TFStateFrom(map[string]interface{}{
			"root_block_device": []interface{}{map[string]interface{}{"volume_size": float64(20), "volume_type": "gp2"}},
		}),
		After: tfplan.TFStateFrom(map[string]interface{}{
			"root_block_device": []interface{}{map[string]interface{}{"volume_type": "gp2"}},
		}),
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %v", unresolved)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 attr-removal change, got %d", len(changes))
	}
	got := string(changes[0].After)
	if strings.Contains(got, "volume_size") {
		t.Fatalf("removed nested attr still present:\n%s", got)
	}
	if !strings.Contains(got, `volume_type = "gp2"`) {
		t.Fatalf("sibling attr lost:\n%s", got)
	}
}

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
		Before: tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.micro"}),
		After:  tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.large"}),
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
		Before: tfplan.TFStateFrom(map[string]interface{}{"location": "eastus"}),
		After:  tfplan.TFStateFrom(map[string]interface{}{"location": "westus"}),
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
		Before: tfplan.TFStateFrom(map[string]interface{}{"bucket": "v1"}),
		After:  tfplan.TFStateFrom(map[string]interface{}{"bucket": "v2"}),
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
		Before: tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.micro"}),
		After:  tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.large"}),
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
		Before: tfplan.TFStateFrom(map[string]interface{}{"cidr_block": "10.0.0.0/16"}),
		After:  tfplan.TFStateFrom(map[string]interface{}{"cidr_block": "10.1.0.0/16"}),
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
		Before: tfplan.TFStateFrom(map[string]interface{}{"cidr": "10.0.0.0/24"}),
		After:  tfplan.TFStateFrom(map[string]interface{}{"cidr": "10.9.0.0/24"}),
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
		Before: tfplan.TFStateFrom(map[string]interface{}{
			"instance_type": "t3.micro",
			"root_block_device": []interface{}{
				map[string]interface{}{"volume_size": float64(20), "volume_type": "gp2"},
			},
		}),
		After: tfplan.TFStateFrom(map[string]interface{}{
			"instance_type": "t3.micro",
			"root_block_device": []interface{}{
				map[string]interface{}{"volume_size": float64(50), "volume_type": "gp2"},
			},
		}),
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
		Before: tfplan.TFStateFrom(map[string]interface{}{
			"ebs_block_device": []interface{}{
				map[string]interface{}{"device_name": "/dev/sda1", "volume_size": float64(20)},
				map[string]interface{}{"device_name": "/dev/sdb1", "volume_size": float64(100)},
			},
		}),
		After: tfplan.TFStateFrom(map[string]interface{}{
			"ebs_block_device": []interface{}{
				map[string]interface{}{"device_name": "/dev/sda1", "volume_size": float64(50)}, // drifted
				map[string]interface{}{"device_name": "/dev/sdb1", "volume_size": float64(100)},
			},
		}),
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

func TestNestedBlockSameCountReplacement(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "azurerm_application_gateway" "agw" {
  backend_http_settings {
    name                  = "old-setting"
    port                  = 80
    protocol              = "Http"
    cookie_based_affinity = "Disabled"
  }

  backend_http_settings {
    name                  = "stable-setting"
    port                  = 8080
    protocol              = "Http"
    cookie_based_affinity = "Disabled"
  }
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"azurerm_application_gateway.agw","mode":"managed","type":"azurerm_application_gateway","name":"agw","expressions":{}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "azurerm_application_gateway.agw", Type: "azurerm_application_gateway", Name: "agw",
		Before: tfplan.TFStateFrom(map[string]interface{}{
			"backend_http_settings": []interface{}{
				map[string]interface{}{"name": "old-setting", "port": float64(80), "protocol": "Http", "cookie_based_affinity": "Disabled"},
				map[string]interface{}{"name": "stable-setting", "port": float64(8080), "protocol": "Http", "cookie_based_affinity": "Disabled"},
			},
		}),
		After: tfplan.TFStateFrom(map[string]interface{}{
			"backend_http_settings": []interface{}{
				map[string]interface{}{"name": "new-setting", "port": float64(80), "protocol": "Http", "cookie_based_affinity": "Disabled"},
				map[string]interface{}{"name": "stable-setting", "port": float64(8080), "protocol": "Http", "cookie_based_affinity": "Disabled"},
			},
		}),
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %v", unresolved)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 same-count replacement change, got %d", len(changes))
	}
	got := string(changes[0].After)
	if strings.Contains(got, "old-setting") {
		t.Errorf("removed setting still present:\n%s", got)
	}
	if !strings.Contains(got, "new-setting") {
		t.Errorf("added setting missing:\n%s", got)
	}
	if !strings.Contains(got, "stable-setting") {
		t.Errorf("stable setting lost:\n%s", got)
	}
}

func TestNamedNestedAttrEditReportsIdentity(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "azurerm_application_gateway" "agw" {
  backend_http_settings {
    name                  = "api-setting"
    port                  = 80
    protocol              = "Http"
    cookie_based_affinity = "Disabled"
  }
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"azurerm_application_gateway.agw","mode":"managed","type":"azurerm_application_gateway","name":"agw","expressions":{}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "azurerm_application_gateway.agw", Type: "azurerm_application_gateway", Name: "agw",
		Before: tfplan.TFStateFrom(map[string]interface{}{
			"backend_http_settings": []interface{}{
				map[string]interface{}{"name": "api-setting", "port": float64(80), "protocol": "Http", "cookie_based_affinity": "Disabled"},
			},
		}),
		After: tfplan.TFStateFrom(map[string]interface{}{
			"backend_http_settings": []interface{}{
				map[string]interface{}{"name": "api-setting", "port": float64(8080), "protocol": "Http", "cookie_based_affinity": "Disabled"},
			},
		}),
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %v", unresolved)
	}
	if len(changes) != 1 || len(changes[0].Edits) != 1 {
		t.Fatalf("want 1 named nested edit, got changes=%d edits=%v", len(changes), changes)
	}
	attrs := changes[0].Edits[0].Attrs
	want := `backend_http_settings["api-setting"].port`
	if len(attrs) != 1 || attrs[0] != want {
		t.Fatalf("attrs = %v, want %q", attrs, want)
	}
}

func TestAppGatewayMissingBackendHTTPSettingsProbeNameAdded(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "azurerm_application_gateway" "agw" {
  backend_http_settings {
    name                                = "bhs-adds-apis"
    cookie_based_affinity               = "Enabled"
    pick_host_name_from_backend_address = true
    port                                = 443
    protocol                            = "Https"
    request_timeout                     = 30
  }
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"azurerm_application_gateway.agw","mode":"managed","type":"azurerm_application_gateway","name":"agw","expressions":{}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "azurerm_application_gateway.agw", Type: "azurerm_application_gateway", Name: "agw",
		Before: tfplan.TFStateFrom(map[string]interface{}{
			"backend_http_settings": []interface{}{
				map[string]interface{}{
					"name": "bhs-adds-apis", "cookie_based_affinity": "Enabled",
					"pick_host_name_from_backend_address": true, "port": float64(443),
					"protocol": "Https", "request_timeout": float64(30), "probe_name": "",
				},
			},
		}),
		After: tfplan.TFStateFrom(map[string]interface{}{
			"backend_http_settings": []interface{}{
				map[string]interface{}{
					"name": "bhs-adds-apis", "cookie_based_affinity": "Enabled",
					"pick_host_name_from_backend_address": true, "port": float64(443),
					"protocol": "Https", "request_timeout": float64(30), "probe_name": "probe-adds-apis-path",
				},
			},
		}),
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %v", unresolved)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 probe_name insertion change, got %d", len(changes))
	}
	got := string(changes[0].After)
	if !strings.Contains(got, `probe_name`) || !strings.Contains(got, `"probe-adds-apis-path"`) {
		t.Fatalf("probe_name was not added:\n%s", got)
	}
	attrs := changes[0].Edits[0].Attrs
	want := `backend_http_settings["bhs-adds-apis"].probe_name`
	if len(attrs) != 1 || attrs[0] != want {
		t.Fatalf("attrs = %v, want %q", attrs, want)
	}
}

func TestNestedBlockCollectionAmbiguousPairingUnresolved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_security_group" "sg" {
  ingress {
    from_port = 80
    protocol  = "tcp"
  }

  ingress {
    from_port = 443
    protocol  = "tcp"
  }
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_security_group.sg","mode":"managed","type":"aws_security_group","name":"sg","expressions":{}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "aws_security_group.sg", Type: "aws_security_group", Name: "sg",
		Before: tfplan.TFStateFrom(map[string]interface{}{
			"ingress": []interface{}{
				map[string]interface{}{"from_port": float64(80), "protocol": "tcp"},
				map[string]interface{}{"from_port": float64(443), "protocol": "tcp"},
			},
		}),
		After: tfplan.TFStateFrom(map[string]interface{}{
			"ingress": []interface{}{
				map[string]interface{}{"from_port": float64(80), "protocol": "udp"},
				map[string]interface{}{"from_port": float64(80), "protocol": "http"},
			},
		}),
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("want 0 changes for ambiguous collection pairing, got %d:\n%s", len(changes), changes[0].After)
	}
	if len(unresolved) != 1 || !strings.Contains(unresolved[0].Reason, "ambiguous nested block collection match") {
		t.Fatalf("want ambiguous collection unresolved, got %v", unresolved)
	}
}

// TestNestedBlockVarRefUnresolved: nested block attr is a var ref but the config
// expressions tree has no entry for the block (empty expressions: {}).
// The attr cannot be traced → reported as unresolved, never hardcoded.
func TestNestedBlockVarRefUnresolved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `variable "vol_size" {}

resource "aws_instance" "web" {
  root_block_device {
    volume_size = var.vol_size
  }
}
`)
	// expressions:{} — no nested block expression present in the config tree.
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web","expressions":{}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "aws_instance.web", Type: "aws_instance", Name: "web",
		Before: tfplan.TFStateFrom(map[string]interface{}{"root_block_device": []interface{}{map[string]interface{}{"volume_size": float64(20)}}}),
		After:  tfplan.TFStateFrom(map[string]interface{}{"root_block_device": []interface{}{map[string]interface{}{"volume_size": float64(50)}}}),
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("want 0 changes (var ref, no config expression), got %d", len(changes))
	}
	// TraceNested fails because the nested block expression is absent from the
	// config tree — expect exactly one unresolved entry.
	if len(unresolved) != 1 {
		t.Fatalf("want 1 unresolved (expression not found), got %v", unresolved)
	}
}

// TestNestedBlockVarRefTraced: nested block attr is var.vol_size that resolves
// through a module argument. The edit must land on the root module call arg,
// not the nested block itself.
func TestNestedBlockVarRefTraced(t *testing.T) {
	dir := t.TempDir()
	// Root passes vol_size = 20 to module "app".
	writeFile(t, dir, "main.tf", `module "app" {
  source   = "./modules/app"
  vol_size = 20
}
`)
	// Module resource uses var.vol_size inside a nested block.
	writeFile(t, dir, "modules/app/main.tf", `variable "vol_size" {}

resource "aws_instance" "web" {
  root_block_device {
    volume_size = var.vol_size
  }
}
`)
	// Plan JSON: nested block expression shows references = ["var.vol_size"].
	cfg := `{"configuration":{"root_module":{
		"module_calls":{"app":{
			"source":"./modules/app",
			"expressions":{"vol_size":{"constant_value":20}},
			"module":{
				"variables":{"vol_size":{}},
				"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web",
					"expressions":{
						"root_block_device":[{"volume_size":{"references":["var.vol_size"]}}]
					}}]
			}
		}}
	}}}`
	drifts := []tfplan.Drift{{
		Address: "module.app.aws_instance.web",
		Type:    "aws_instance", Name: "web",
		Before: tfplan.TFStateFrom(map[string]interface{}{
			"root_block_device": []interface{}{
				map[string]interface{}{"volume_size": float64(20)},
			},
		}),
		After: tfplan.TFStateFrom(map[string]interface{}{
			"root_block_device": []interface{}{
				map[string]interface{}{"volume_size": float64(50)},
			},
		}),
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %v", unresolved)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change (root module call), got %d", len(changes))
	}
	// Edit must land on ROOT main.tf (module call arg), not the module source.
	if strings.Contains(changes[0].Path, "modules") {
		t.Errorf("edited module source instead of root call arg: %s", changes[0].Path)
	}
	if !strings.Contains(string(changes[0].After), "vol_size = 50") {
		t.Errorf("module call arg not absorbed:\n%s", changes[0].After)
	}
	// Module source must be untouched.
	src, _ := os.ReadFile(filepath.Join(dir, "modules/app/main.tf"))
	if !strings.Contains(string(src), "volume_size = var.vol_size") {
		t.Errorf("module source was modified:\n%s", src)
	}
}

// TestNestedBlockAdd: a block was added out-of-band; osmo should append it.
func TestNestedBlockAdd(t *testing.T) {
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
		Before: tfplan.TFStateFrom(map[string]interface{}{
			"ingress": []interface{}{
				map[string]interface{}{"from_port": float64(80), "to_port": float64(80), "protocol": "tcp"},
			},
		}),
		After: tfplan.TFStateFrom(map[string]interface{}{
			"ingress": []interface{}{
				map[string]interface{}{"from_port": float64(80), "to_port": float64(80), "protocol": "tcp"},
				map[string]interface{}{"from_port": float64(443), "to_port": float64(443), "protocol": "tcp"},
			},
		}),
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
	// New block must be appended.
	if !strings.Contains(got, "from_port = 443") {
		t.Errorf("added block not generated:\n%s", got)
	}
	// Original block must be preserved.
	if !strings.Contains(got, "from_port = 80") {
		t.Errorf("original block lost:\n%s", got)
	}
}

// TestNestedBlockRemove: a block was removed out-of-band; osmo should remove it.
func TestNestedBlockRemove(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_security_group" "sg" {
  ingress {
    from_port = 80
    to_port   = 80
    protocol  = "tcp"
  }

  ingress {
    from_port = 443
    to_port   = 443
    protocol  = "tcp"
  }
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_security_group.sg","mode":"managed","type":"aws_security_group","name":"sg","expressions":{}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "aws_security_group.sg", Type: "aws_security_group", Name: "sg",
		Before: tfplan.TFStateFrom(map[string]interface{}{
			"ingress": []interface{}{
				map[string]interface{}{"from_port": float64(80), "to_port": float64(80), "protocol": "tcp"},
				map[string]interface{}{"from_port": float64(443), "to_port": float64(443), "protocol": "tcp"},
			},
		}),
		After: tfplan.TFStateFrom(map[string]interface{}{
			"ingress": []interface{}{
				map[string]interface{}{"from_port": float64(80), "to_port": float64(80), "protocol": "tcp"},
			},
		}),
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
	// 443 block must be gone.
	if strings.Contains(got, "from_port = 443") {
		t.Errorf("removed block still present:\n%s", got)
	}
	// 80 block must remain.
	if !strings.Contains(got, "from_port = 80") {
		t.Errorf("remaining block lost:\n%s", got)
	}
}

// TestDeepNestedBlock: 3-level nesting (server_side_encryption_configuration >
// rule > apply_server_side_encryption_by_default > sse_algorithm).
func TestDeepNestedBlock(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_s3_bucket" "b" {
  bucket = "my-bucket"

  server_side_encryption_configuration {
    rule {
      apply_server_side_encryption_by_default {
        sse_algorithm   = "AES256"
        kms_master_key_id = ""
      }
      bucket_key_enabled = false
    }
  }
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_s3_bucket.b","mode":"managed","type":"aws_s3_bucket","name":"b",
			"expressions":{"bucket":{"constant_value":"my-bucket"}}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "aws_s3_bucket.b", Type: "aws_s3_bucket", Name: "b",
		Before: tfplan.TFStateFrom(map[string]interface{}{
			"bucket": "my-bucket",
			"server_side_encryption_configuration": []interface{}{
				map[string]interface{}{
					"rule": []interface{}{
						map[string]interface{}{
							"apply_server_side_encryption_by_default": []interface{}{
								map[string]interface{}{
									"sse_algorithm":     "AES256",
									"kms_master_key_id": "",
								},
							},
							"bucket_key_enabled": false,
						},
					},
				},
			},
		}),
		After: tfplan.TFStateFrom(map[string]interface{}{
			"bucket": "my-bucket",
			"server_side_encryption_configuration": []interface{}{
				map[string]interface{}{
					"rule": []interface{}{
						map[string]interface{}{
							"apply_server_side_encryption_by_default": []interface{}{
								map[string]interface{}{
									"sse_algorithm":     "aws:kms", // drifted
									"kms_master_key_id": "arn:aws:kms:us-east-1:123:key/abc",
								},
							},
							"bucket_key_enabled": true, // also drifted
						},
					},
				},
			},
		}),
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
	// hclwrite normalizes spacing on rewrite; check value only.
	if !strings.Contains(got, `"aws:kms"`) {
		t.Errorf("deep nested sse_algorithm not absorbed:\n%s", got)
	}
	if !strings.Contains(got, "bucket_key_enabled = true") {
		t.Errorf("mid-level bucket_key_enabled not absorbed:\n%s", got)
	}
	// kms_master_key_id was not in config → skipped silently (computed).
}

// ---- Dynamic block tests ------------------------------------------------

// TestDynamicBlockCollectionUpdate: resource uses `dynamic "ingress"` driven by
// var.rules; a new ingress block appears in drift. The edit must update the
// root module call arg (rules = [...]) to include the new block, not append a
// literal ingress {} block to the HCL.
func TestDynamicBlockCollectionUpdate(t *testing.T) {
	dir := t.TempDir()
	// Root passes rules to module "sg".
	writeFile(t, dir, "main.tf", `module "sg" {
  source = "./modules/sg"
  rules  = [{ from_port = 80, to_port = 80, protocol = "tcp" }]
}
`)
	// Module uses a dynamic block driven by var.rules.
	writeFile(t, dir, "modules/sg/main.tf", `variable "rules" {}

resource "aws_security_group" "sg" {
  dynamic "ingress" {
    for_each = var.rules
    content {
      from_port = ingress.value.from_port
      to_port   = ingress.value.to_port
      protocol  = ingress.value.protocol
    }
  }
}
`)
	// Plan JSON: for_each references var.rules in the nested block expression,
	// and the root module call passes it as a constant_value list.
	cfg := `{"configuration":{"root_module":{
		"module_calls":{"sg":{
			"source":"./modules/sg",
			"expressions":{"rules":{"constant_value":[{"from_port":80,"to_port":80,"protocol":"tcp"}]}},
			"module":{
				"variables":{"rules":{}},
				"resources":[{"address":"aws_security_group.sg","mode":"managed","type":"aws_security_group","name":"sg",
					"expressions":{}}]
			}
		}}
	}}}`
	// Drift: 443 ingress block added out-of-band.
	drifts := []tfplan.Drift{{
		Address: "module.sg.aws_security_group.sg",
		Type:    "aws_security_group", Name: "sg",
		Before: tfplan.TFStateFrom(map[string]interface{}{
			"ingress": []interface{}{
				map[string]interface{}{"from_port": float64(80), "to_port": float64(80), "protocol": "tcp"},
			},
		}),
		After: tfplan.TFStateFrom(map[string]interface{}{
			"ingress": []interface{}{
				map[string]interface{}{"from_port": float64(80), "to_port": float64(80), "protocol": "tcp"},
				map[string]interface{}{"from_port": float64(443), "to_port": float64(443), "protocol": "tcp"},
			},
		}),
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %v", unresolved)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change (root module call), got %d", len(changes))
	}
	// Edit must land on ROOT main.tf, not the module source.
	if strings.Contains(changes[0].Path, "modules") {
		t.Errorf("edited module source instead of root call: %s", changes[0].Path)
	}
	got := string(changes[0].After)
	// New block values must appear in the rules collection.
	if !strings.Contains(got, "443") {
		t.Errorf("new ingress rule not absorbed into rules collection:\n%s", got)
	}
	// Original 80 values must be preserved.
	if !strings.Contains(got, "80") {
		t.Errorf("original ingress rule lost:\n%s", got)
	}
	// Module source must be untouched (no literal ingress {} added).
	src, _ := os.ReadFile(filepath.Join(dir, "modules/sg/main.tf"))
	if strings.Contains(string(src), "from_port = 443") {
		t.Errorf("literal ingress block was incorrectly added to module source:\n%s", src)
	}
	if !strings.Contains(string(src), `dynamic "ingress"`) {
		t.Errorf("dynamic block was modified or removed from module source:\n%s", src)
	}
}

// TestDynamicBlockNonVarForEach: dynamic block's for_each derives from a local —
// cannot trace; must be reported as unresolved.
func TestDynamicBlockNonVarForEach(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `locals {
  rules = [{ from_port = 80, to_port = 80, protocol = "tcp" }]
}

resource "aws_security_group" "sg" {
  dynamic "ingress" {
    for_each = local.rules
    content {
      from_port = ingress.value.from_port
      to_port   = ingress.value.to_port
      protocol  = ingress.value.protocol
    }
  }
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_security_group.sg","mode":"managed","type":"aws_security_group","name":"sg","expressions":{}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "aws_security_group.sg", Type: "aws_security_group", Name: "sg",
		Before: tfplan.TFStateFrom(map[string]interface{}{
			"ingress": []interface{}{
				map[string]interface{}{"from_port": float64(80), "to_port": float64(80), "protocol": "tcp"},
			},
		}),
		After: tfplan.TFStateFrom(map[string]interface{}{
			"ingress": []interface{}{
				map[string]interface{}{"from_port": float64(80), "to_port": float64(80), "protocol": "tcp"},
				map[string]interface{}{"from_port": float64(443), "to_port": float64(443), "protocol": "tcp"},
			},
		}),
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("want 0 changes (local.rules not traceable), got %d", len(changes))
	}
	if len(unresolved) != 1 || !strings.Contains(unresolved[0].Reason, "local") {
		t.Fatalf("want 1 local-unresolvable unresolved, got %v", unresolved)
	}
}

func TestForEachMapEntrySingleInstanceDrift(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_instance" "web" {
  for_each = {
    a = { instance_type = "t3.micro" }
    b = { instance_type = "t3.small" }
  }
  instance_type = each.value.instance_type
}
`)
	// Configuration tree: instance_type references each.value.instance_type.
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web",
			"expressions":{"instance_type":{"references":["each.value.instance_type","each.value","each"]}}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: `aws_instance.web["a"]`, Type: "aws_instance", Name: "web",
		Before: tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.micro"}),
		After:  tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.large"}),
	}}

	changes, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %v", unresolved)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change, got %d: %v", len(changes), changes)
	}
	got := string(changes[0].After)
	// Instance "a" must be updated.
	if !strings.Contains(got, `"t3.large"`) {
		t.Errorf("instance a not updated:\n%s", got)
	}
	// Instance "b" must be untouched.
	if !strings.Contains(got, `"t3.small"`) {
		t.Errorf("instance b value lost:\n%s", got)
	}
	// Instance "a"'s old value must be gone.
	if strings.Contains(got, `"t3.micro"`) {
		t.Errorf("old value t3.micro still present after patch:\n%s", got)
	}
}

func TestForEachMapEntryOtherInstanceUntouched(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_instance" "web" {
  for_each = {
    prod = { instance_type = "t3.large" }
    dev  = { instance_type = "t3.micro" }
  }
  instance_type = each.value.instance_type
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web",
			"expressions":{"instance_type":{"references":["each.value.instance_type","each.value","each"]}}}]
	}}}`
	// Only "prod" drifted — "dev" untouched.
	drifts := []tfplan.Drift{{
		Address: `aws_instance.web["prod"]`, Type: "aws_instance", Name: "web",
		Before: tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.large"}),
		After:  tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.xlarge"}),
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
	if !strings.Contains(got, `"t3.xlarge"`) {
		t.Errorf("prod not updated:\n%s", got)
	}
	if !strings.Contains(got, `"t3.micro"`) {
		t.Errorf("dev instance_type lost:\n%s", got)
	}
}

func TestForEachScalarDirectValue(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_instance" "web" {
  for_each      = { a = "t3.micro", b = "t3.small" }
  instance_type = each.value
}
`)
	// References contain each.value directly (not each.value.X).
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web",
			"expressions":{"instance_type":{"references":["each.value","each"]}}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: `aws_instance.web["a"]`, Type: "aws_instance", Name: "web",
		Before: tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.micro"}),
		After:  tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.large"}),
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
	if !strings.Contains(got, `"t3.large"`) {
		t.Errorf("instance a not updated:\n%s", got)
	}
	if !strings.Contains(got, `"t3.small"`) {
		t.Errorf("instance b value lost:\n%s", got)
	}
	if strings.Contains(got, `"t3.micro"`) {
		t.Errorf("old value still present:\n%s", got)
	}
}

func TestForEachMapEntryMissingKeyReportsUnresolved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_instance" "web" {
  for_each      = { a = { instance_type = "t3.micro" } }
  instance_type = each.value.instance_type
}
`)
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web",
			"expressions":{"instance_type":{"references":["each.value.instance_type","each.value","each"]}}}]
	}}}`
	// Drift for key "z" which is not in the for_each map.
	drifts := []tfplan.Drift{{
		Address: `aws_instance.web["z"]`, Type: "aws_instance", Name: "web",
		Before: tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.micro"}),
		After:  tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.large"}),
	}}

	_, unresolved, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) == 0 {
		t.Fatal("expected unresolved for missing key, got none")
	}
}

func TestCRLFPreserved(t *testing.T) {
	dir := t.TempDir()
	// Write a .tf file with Windows CRLF line endings.
	crlfContent := "resource \"aws_instance\" \"web\" {\r\n  instance_type = \"t3.micro\"\r\n}\r\n"
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(crlfContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := `{"configuration":{"root_module":{
		"resources":[{"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web",
			"expressions":{"instance_type":{"constant_value":"t3.micro"}}}]
	}}}`
	drifts := []tfplan.Drift{{
		Address: "aws_instance.web", Type: "aws_instance", Name: "web",
		Before: tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.micro"}),
		After:  tfplan.TFStateFrom(map[string]interface{}{"instance_type": "t3.large"}),
	}}

	changes, _, err := Plan(dir, drifts, []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change, got %d", len(changes))
	}
	after := changes[0].After
	if !strings.Contains(string(after), "t3.large") {
		t.Errorf("drift not absorbed:\n%s", after)
	}
	if !strings.Contains(string(after), "\r\n") {
		t.Errorf("CRLF line endings not preserved in output:\n%q", after)
	}
}
