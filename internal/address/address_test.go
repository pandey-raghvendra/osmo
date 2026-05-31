package address

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		in       string
		modules  []Step
		mode     string
		typ      string
		name     string
		index    string
		hasIndex bool
		relAddr  string
	}{
		{
			in:   "aws_instance.web",
			mode: "managed", typ: "aws_instance", name: "web",
			relAddr: "aws_instance.web",
		},
		{
			in:   "data.aws_ami.ubuntu",
			mode: "data", typ: "aws_ami", name: "ubuntu",
			relAddr: "data.aws_ami.ubuntu",
		},
		{
			in:      "module.network.azurerm_subnet.this",
			modules: []Step{{Name: "network"}},
			mode:    "managed", typ: "azurerm_subnet", name: "this",
			relAddr: "azurerm_subnet.this",
		},
		{
			in:      "module.network.azurerm_subnet.this[\"app\"]",
			modules: []Step{{Name: "network"}},
			mode:    "managed", typ: "azurerm_subnet", name: "this",
			index: "app", hasIndex: true,
			relAddr: "azurerm_subnet.this",
		},
		{
			in:      "module.net[\"app\"].module.sub.aws_s3_bucket.b",
			modules: []Step{{Name: "net", Index: "app", HasIndex: true}, {Name: "sub"}},
			mode:    "managed", typ: "aws_s3_bucket", name: "b",
			relAddr: "aws_s3_bucket.b",
		},
		{
			in:   "aws_instance.web[0]",
			mode: "managed", typ: "aws_instance", name: "web",
			index: "0", hasIndex: true,
			relAddr: "aws_instance.web",
		},
		{
			in:      `module.m.aws_x.y["a.b"]`, // dotted for_each key
			modules: []Step{{Name: "m"}},
			mode:    "managed", typ: "aws_x", name: "y",
			index: "a.b", hasIndex: true,
			relAddr: "aws_x.y",
		},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			a, err := Parse(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if len(a.Modules) != len(tc.modules) {
				t.Fatalf("modules = %+v, want %+v", a.Modules, tc.modules)
			}
			for i := range tc.modules {
				if a.Modules[i] != tc.modules[i] {
					t.Errorf("module[%d] = %+v, want %+v", i, a.Modules[i], tc.modules[i])
				}
			}
			if a.Mode != tc.mode || a.Type != tc.typ || a.Name != tc.name {
				t.Errorf("got mode=%s type=%s name=%s", a.Mode, a.Type, a.Name)
			}
			if a.Index != tc.index || a.HasIndex != tc.hasIndex {
				t.Errorf("index = %q has=%v, want %q %v", a.Index, a.HasIndex, tc.index, tc.hasIndex)
			}
			if a.RelAddr() != tc.relAddr {
				t.Errorf("RelAddr = %q, want %q", a.RelAddr(), tc.relAddr)
			}
		})
	}
}
