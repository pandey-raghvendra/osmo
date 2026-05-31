// Package absorb rewrites Terraform HCL so that configuration follows
// real-world drift (the "absorb" direction).
package absorb

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/raghav/osmo/internal/tfplan"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// FileChange is a proposed rewrite of one .tf file.
type FileChange struct {
	Path    string   // absolute path to the .tf file
	Before  []byte   // original bytes
	After   []byte   // rewritten bytes
	Address string   // resource address that triggered the change
	Attrs   []string // attribute names rewritten
}

// Plan walks dir's .tf files and computes the HCL rewrites needed to absorb the
// given drifts. v1 only rewrites attributes that already exist as literals in
// the resource block AND changed between Before and After. Computed/read-only
// attributes never in config are left untouched.
func Plan(dir string, drifts []tfplan.Drift) ([]FileChange, error) {
	tfFiles, err := filepath.Glob(filepath.Join(dir, "*.tf"))
	if err != nil {
		return nil, fmt.Errorf("glob tf files: %w", err)
	}

	var changes []FileChange
	for _, d := range drifts {
		changed := changedAttrs(d.Before, d.After)
		if len(changed) == 0 {
			continue
		}
		fc, err := absorbResource(tfFiles, d, changed)
		if err != nil {
			return nil, err
		}
		if fc != nil {
			changes = append(changes, *fc)
		}
	}
	return changes, nil
}

// absorbResource finds the HCL block for drift d across tfFiles and rewrites the
// changed attributes that are present in the block. Returns nil if the block or
// no matching attributes are found.
func absorbResource(tfFiles []string, d tfplan.Drift, changed map[string]interface{}) (*FileChange, error) {
	for _, path := range tfFiles {
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		f, diags := hclwrite.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return nil, fmt.Errorf("parse %s: %s", path, diags.Error())
		}

		block := f.Body().FirstMatchingBlock("resource", []string{d.Type, d.Name})
		if block == nil {
			continue
		}

		body := block.Body()
		var rewritten []string
		for name, val := range changed {
			// Only touch attrs already written literally in config.
			if body.GetAttribute(name) == nil {
				continue
			}
			ctyVal, err := toCty(val)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", d.Address, name, err)
			}
			body.SetAttributeValue(name, ctyVal)
			rewritten = append(rewritten, name)
		}
		if len(rewritten) == 0 {
			return nil, nil
		}
		sort.Strings(rewritten)
		return &FileChange{
			Path:    path,
			Before:  src,
			After:   f.Bytes(),
			Address: d.Address,
			Attrs:   rewritten,
		}, nil
	}
	return nil, nil
}

// changedAttrs returns top-level attributes whose value differs between before
// and after. Nested-block drift is out of scope for v1.
func changedAttrs(before, after map[string]interface{}) map[string]interface{} {
	changed := make(map[string]interface{})
	for k, av := range after {
		if bv, ok := before[k]; !ok || !reflect.DeepEqual(bv, av) {
			changed[k] = av
		}
	}
	return changed
}

// toCty converts an arbitrary JSON-decoded value into a cty.Value via its
// implied type, so hclwrite can emit it as an HCL literal.
func toCty(v interface{}) (cty.Value, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return cty.NilVal, err
	}
	t, err := ctyjson.ImpliedType(b)
	if err != nil {
		return cty.NilVal, err
	}
	return ctyjson.Unmarshal(b, t)
}
