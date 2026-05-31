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

// ResourceEdit records which attributes of one resource were rewritten.
type ResourceEdit struct {
	Address string   // resource address, e.g. "aws_instance.web"
	Attrs   []string // attribute names rewritten (sorted)
}

// FileChange is a proposed rewrite of one .tf file. A single file may absorb
// drift from multiple resources, recorded in Edits.
type FileChange struct {
	Path   string         // absolute path to the .tf file
	Before []byte         // original bytes
	After  []byte         // rewritten bytes
	Edits  []ResourceEdit // per-resource edits applied to this file
}

// Plan walks dir's .tf files and computes the HCL rewrites needed to absorb the
// given drifts. v1 only rewrites attributes that already exist as literals in
// the resource block AND changed between Before and After. Computed/read-only
// attributes never in config are left untouched.
//
// Each file is parsed once and all matching drifts are applied to that single
// in-memory AST before emitting one FileChange, so multiple drifted resources
// sharing a file do not clobber each other's edits.
func Plan(dir string, drifts []tfplan.Drift) ([]FileChange, error) {
	tfFiles, err := filepath.Glob(filepath.Join(dir, "*.tf"))
	if err != nil {
		return nil, fmt.Errorf("glob tf files: %w", err)
	}

	// Precompute changed attrs once per drift.
	type pending struct {
		drift   tfplan.Drift
		changed map[string]interface{}
	}
	var todo []pending
	for _, d := range drifts {
		changed := changedAttrs(d.Before, d.After)
		if len(changed) > 0 {
			todo = append(todo, pending{d, changed})
		}
	}

	var changes []FileChange
	for _, path := range tfFiles {
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		f, diags := hclwrite.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return nil, fmt.Errorf("parse %s: %s", path, diags.Error())
		}

		var edits []ResourceEdit
		for _, p := range todo {
			block := f.Body().FirstMatchingBlock("resource", []string{p.drift.Type, p.drift.Name})
			if block == nil {
				continue // resource not defined in this file
			}
			rewritten, err := applyAttrs(block, p.drift, p.changed)
			if err != nil {
				return nil, err
			}
			if len(rewritten) > 0 {
				edits = append(edits, ResourceEdit{Address: p.drift.Address, Attrs: rewritten})
			}
		}

		if len(edits) > 0 {
			changes = append(changes, FileChange{
				Path:   path,
				Before: src,
				After:  f.Bytes(),
				Edits:  edits,
			})
		}
	}
	return changes, nil
}

// applyAttrs rewrites, in place, the changed attributes that are present as
// literals in the block. Returns the sorted names of attributes rewritten.
func applyAttrs(block *hclwrite.Block, d tfplan.Drift, changed map[string]interface{}) ([]string, error) {
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
	sort.Strings(rewritten)
	return rewritten, nil
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
