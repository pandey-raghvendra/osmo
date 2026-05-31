// Package absorb rewrites Terraform HCL so that configuration follows
// real-world drift (the "absorb" direction).
//
// It is provenance-driven: each drifted attribute is traced through the plan's
// configuration tree (see internal/provenance) to the single literal that
// controls it — a resource attribute, a module-call argument, or a variable
// default — which may live in the root module or in a local child module's
// source directory. Anything that cannot be traced to a literal is reported as
// Unresolved and left untouched.
package absorb

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/raghav/osmo/internal/address"
	"github.com/raghav/osmo/internal/config"
	"github.com/raghav/osmo/internal/provenance"
	"github.com/raghav/osmo/internal/tfplan"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// ResourceEdit records which attributes of one drifted resource were rewritten
// (the edit may physically land on a module argument or variable default).
type ResourceEdit struct {
	Address string   // the drifted resource address
	Attrs   []string // attribute names absorbed (sorted)
}

// FileChange is a proposed rewrite of one .tf file.
type FileChange struct {
	Path   string
	Before []byte
	After  []byte
	Edits  []ResourceEdit
}

// Plan computes the HCL rewrites needed to absorb drifts. baseDir is the root
// Terraform working directory; raw is the `terraform show -json` output (for
// the configuration tree). It returns the proposed file changes and a list of
// drifts that could not be absorbed automatically.
func Plan(baseDir string, drifts []tfplan.Drift, raw []byte) ([]FileChange, []provenance.Unresolved, error) {
	root, err := config.Parse(raw)
	if err != nil {
		return nil, nil, err
	}

	var unresolved []provenance.Unresolved

	// scalarOp: a top-level attr routed through provenance tracing.
	type scalarOp struct {
		target    *provenance.Target
		driftAddr string
		attr      string
	}
	// nestedOp: a nested-block attr drift, handled via direct HCL edit.
	type nestedOp struct {
		nbd  NestedBlockDrift
		addr address.Addr
	}
	var scalarOps []scalarOp
	var nestedOps []nestedOp

	for _, d := range drifts {
		addr, err := address.Parse(d.Address)
		if err != nil {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: d.Address, Attr: "*", Reason: "unparseable address: " + err.Error()})
			continue
		}
		for name, av := range d.After {
			bv := d.Before[name]
			if reflect.DeepEqual(bv, av) {
				continue
			}
			afterSlice, isSlice := av.([]interface{})
			beforeSlice, _ := bv.([]interface{})
			if !isSlice {
				// Scalar attr: route through provenance.
				t, u := provenance.Trace(root, addr, name, av)
				if u != nil {
					unresolved = append(unresolved, *u)
					continue
				}
				scalarOps = append(scalarOps, scalarOp{t, d.Address, name})
			} else {
				// Nested block: count change = add/remove (out of scope).
				if len(beforeSlice) != len(afterSlice) {
					unresolved = append(unresolved, provenance.Unresolved{
						Address: d.Address, Attr: name,
						Reason: fmt.Sprintf("nested block count changed (%d → %d); block add/remove not supported", len(beforeSlice), len(afterSlice)),
					})
					continue
				}
				for _, nbd := range diffNestedBlocks(addr, d.Address, name, beforeSlice, afterSlice) {
					nestedOps = append(nestedOps, nestedOp{nbd, addr})
				}
			}
		}
	}

	// Group ops by the source directory whose .tf files hold the construct.
	editors := map[string]*dirEditor{}
	// editsByPath accumulates, per file, drift-address -> attrs absorbed.
	editsByPath := map[string]map[string]map[string]bool{}

	record := func(path, driftAddr, attr string) {
		if editsByPath[path] == nil {
			editsByPath[path] = map[string]map[string]bool{}
		}
		if editsByPath[path][driftAddr] == nil {
			editsByPath[path][driftAddr] = map[string]bool{}
		}
		editsByPath[path][driftAddr][attr] = true
	}

	getEditor := func(dir string) (*dirEditor, error) {
		de := editors[dir]
		if de != nil {
			return de, nil
		}
		de, err := newDirEditor(dir)
		if err != nil {
			return nil, err
		}
		editors[dir] = de
		return de, nil
	}

	for _, o := range scalarOps {
		dir, err := resolveDir(root, baseDir, o.target.SourceModulePath)
		if err != nil {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: o.driftAddr, Attr: o.attr, Reason: err.Error()})
			continue
		}
		de, err := getEditor(dir)
		if err != nil {
			return nil, nil, err
		}
		path, err := de.apply(o.target)
		if err != nil {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: o.driftAddr, Attr: o.attr, Reason: err.Error()})
			continue
		}
		record(path, o.driftAddr, o.attr)
	}

	for _, o := range nestedOps {
		dir, err := resolveDir(root, baseDir, o.addr.Modules)
		if err != nil {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: o.nbd.DriftAddr, Attr: o.nbd.BlockType + ".*", Reason: err.Error()})
			continue
		}
		de, err := getEditor(dir)
		if err != nil {
			return nil, nil, err
		}
		path, absorbed, urs := de.applyNested(o.nbd)
		unresolved = append(unresolved, urs...)
		for _, attr := range absorbed {
			record(path, o.nbd.DriftAddr, attr)
		}
	}

	// Emit one FileChange per dirty file.
	var changes []FileChange
	for _, de := range editors {
		for path, ff := range de.files {
			if !ff.dirty {
				continue
			}
			changes = append(changes, FileChange{
				Path:   path,
				Before: ff.src,
				After:  ff.f.Bytes(),
				Edits:  buildEdits(editsByPath[path]),
			})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, unresolved, nil
}

func buildEdits(byAddr map[string]map[string]bool) []ResourceEdit {
	var edits []ResourceEdit
	for addr, attrs := range byAddr {
		names := make([]string, 0, len(attrs))
		for a := range attrs {
			names = append(names, a)
		}
		sort.Strings(names)
		edits = append(edits, ResourceEdit{Address: addr, Attrs: names})
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].Address < edits[j].Address })
	return edits
}

// ---- Nested block support -----------------------------------------------

// NestedBlockDrift holds drift detected within one nested block instance.
type NestedBlockDrift struct {
	ResourceType string
	ResourceName string
	ResourceMode string
	DriftAddr    string
	BlockType    string
	Before       map[string]interface{} // all attrs of this block (for matching)
	DriftedAttrs map[string]interface{} // only changed attrs -> new values
}

// diffNestedBlocks compares element-matched before/after arrays and returns one
// NestedBlockDrift per element that has attribute changes. Caller guarantees
// len(before) == len(after).
func diffNestedBlocks(addr address.Addr, driftAddr, blockType string, before, after []interface{}) []NestedBlockDrift {
	var drifts []NestedBlockDrift
	for i := range after {
		bm, _ := before[i].(map[string]interface{})
		am, _ := after[i].(map[string]interface{})
		if bm == nil || am == nil {
			continue
		}
		changed := changedAttrs(bm, am)
		if len(changed) == 0 {
			continue
		}
		drifts = append(drifts, NestedBlockDrift{
			ResourceType: addr.Type,
			ResourceName: addr.Name,
			ResourceMode: addr.Mode,
			DriftAddr:    driftAddr,
			BlockType:    blockType,
			Before:       bm,
			DriftedAttrs: changed,
		})
	}
	return drifts
}

// applyNested locates the matching nested block in the dirEditor's files and
// rewrites the drifted attributes that are present as literals. Returns the
// file path edited, the absorbed attr names (blockType.attr form), and any
// unresolved attrs (var refs, missing, etc.).
func (de *dirEditor) applyNested(nbd NestedBlockDrift) (path string, absorbed []string, unresolved []provenance.Unresolved) {
	outerType := "resource"
	if nbd.ResourceMode == "data" {
		outerType = "data"
	}
	for p, ff := range de.files {
		resBlock := ff.f.Body().FirstMatchingBlock(outerType, []string{nbd.ResourceType, nbd.ResourceName})
		if resBlock == nil {
			continue
		}
		nested := findMatchingNestedBlock(resBlock.Body(), nbd.BlockType, nbd.Before, nbd.DriftedAttrs)
		if nested == nil {
			continue // resource found but nested block not here; try next file
		}
		body := nested.Body()
		for attr, val := range nbd.DriftedAttrs {
			qualAttr := nbd.BlockType + "." + attr
			if body.GetAttribute(attr) == nil {
				// Attr absent from config (computed/read-only) — skip silently.
				continue
			}
			if _, evalErr := evalAttr(nested, attr); evalErr != nil {
				// Attr is a variable reference, not a literal.
				unresolved = append(unresolved, provenance.Unresolved{
					Address: nbd.DriftAddr, Attr: qualAttr,
					Reason: "nested block attr is a variable reference; trace through var chains not yet supported for nested blocks",
				})
				continue
			}
			newVal, ctyErr := toCty(val)
			if ctyErr != nil {
				unresolved = append(unresolved, provenance.Unresolved{
					Address: nbd.DriftAddr, Attr: qualAttr, Reason: ctyErr.Error()})
				continue
			}
			body.SetAttributeValue(attr, newVal)
			absorbed = append(absorbed, qualAttr)
			ff.dirty = true
		}
		sort.Strings(absorbed)
		return p, absorbed, unresolved
	}
	// Resource block not found at all.
	return "", nil, unresolved
}

// findMatchingNestedBlock returns the nested block of blockType within body
// whose stable (non-drifted) attributes best match the before values. For
// singleton blocks (only one candidate) it returns immediately. For multiple
// candidates it scores each by how many stable attrs match literal values.
func findMatchingNestedBlock(body *hclwrite.Body, blockType string, before, drifted map[string]interface{}) *hclwrite.Block {
	var candidates []*hclwrite.Block
	for _, b := range body.Blocks() {
		if b.Type() == blockType {
			candidates = append(candidates, b)
		}
	}
	switch len(candidates) {
	case 0:
		return nil
	case 1:
		return candidates[0]
	default:
		// Stable attrs: in before but not in drifted — use as identity signals.
		stable := make(map[string]interface{}, len(before))
		for k, v := range before {
			if _, isDrifted := drifted[k]; !isDrifted {
				stable[k] = v
			}
		}
		best, bestScore := candidates[0], -1
		for _, c := range candidates {
			if s := blockMatchScore(c, stable); s > bestScore {
				bestScore = s
				best = c
			}
		}
		return best
	}
}

// blockMatchScore counts how many stable attrs have matching literal values in
// the block. Comparison is done by marshalling both sides to JSON.
func blockMatchScore(b *hclwrite.Block, stableAttrs map[string]interface{}) int {
	score := 0
	for attr, expected := range stableAttrs {
		if b.Body().GetAttribute(attr) == nil {
			continue
		}
		curCty, err := evalAttr(b, attr)
		if err != nil {
			continue // var ref — can't compare
		}
		expCty, err := toCty(expected)
		if err != nil {
			continue
		}
		curJSON, e1 := ctyjson.Marshal(curCty, curCty.Type())
		expJSON, e2 := ctyjson.Marshal(expCty, expCty.Type())
		if e1 == nil && e2 == nil && string(curJSON) == string(expJSON) {
			score++
		}
	}
	return score
}

// changedAttrs returns top-level attributes whose value differs between before
// and after (used both for top-level scalar drift and within diffNestedBlocks).
func changedAttrs(before, after map[string]interface{}) map[string]interface{} {
	changed := map[string]interface{}{}
	for k, av := range after {
		if bv, ok := before[k]; !ok || !reflect.DeepEqual(bv, av) {
			changed[k] = av
		}
	}
	return changed
}

// resolveDir walks the module path from baseDir, following each local module
// source. It errors if any source on the path is non-local (registry/git),
// since those cannot be edited in place.
func resolveDir(root *config.Module, baseDir string, steps []address.Step) (string, error) {
	dir := baseDir
	cur := root
	for _, s := range steps {
		call, ok := cur.ModuleCalls[s.Name]
		if !ok {
			return "", fmt.Errorf("module %q not found in configuration", s.Name)
		}
		if !isLocalSource(call.Source) {
			return "", fmt.Errorf("module %q has non-local source %q (cannot edit in place)", s.Name, call.Source)
		}
		if filepath.IsAbs(call.Source) {
			dir = call.Source
		} else {
			dir = filepath.Join(dir, call.Source)
		}
		cur = &call.Module
	}
	return dir, nil
}

func isLocalSource(src string) bool {
	return strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../") || filepath.IsAbs(src)
}

// ---- dirEditor: parse a directory's .tf files once and edit them in place ----

type tfFile struct {
	src   []byte
	f     *hclwrite.File
	dirty bool
}

type dirEditor struct {
	dir   string
	files map[string]*tfFile // path -> file
}

func newDirEditor(dir string) (*dirEditor, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.tf"))
	if err != nil {
		return nil, err
	}
	de := &dirEditor{dir: dir, files: map[string]*tfFile{}}
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		f, diags := hclwrite.ParseConfig(src, p, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return nil, fmt.Errorf("parse %s: %s", p, diags.Error())
		}
		de.files[p] = &tfFile{src: src, f: f}
	}
	return de, nil
}

// apply finds the target's block among the dir's files and sets the attribute.
// Returns the path of the file edited.
func (de *dirEditor) apply(t *provenance.Target) (string, error) {
	blockType, labels, attr := blockSelector(t)
	for path, ff := range de.files {
		block := ff.f.Body().FirstMatchingBlock(blockType, labels)
		if block == nil {
			continue
		}
		if err := setAttr(block, attr, t.Value, t.InstanceKey); err != nil {
			return "", err
		}
		ff.dirty = true
		return path, nil
	}
	return "", fmt.Errorf("could not locate %s %v in %s", blockType, labels, de.dir)
}

// blockSelector maps a Target to the hclwrite block type/labels and attribute.
func blockSelector(t *provenance.Target) (blockType string, labels []string, attr string) {
	switch t.Kind {
	case provenance.ResourceAttr:
		bt := "resource"
		if t.ResourceMode == "data" {
			bt = "data"
		}
		return bt, []string{t.ResourceType, t.ResourceName}, t.Attr
	case provenance.ModuleArg:
		return "module", []string{t.CallName}, t.Attr
	case provenance.VariableDefault:
		return "variable", []string{t.VarName}, "default"
	default:
		return "", nil, ""
	}
}

// setAttr sets attr on the block to value. When key != nil, it sets value into
// the attribute's existing map literal at that key instead of replacing it.
func setAttr(block *hclwrite.Block, attr string, value interface{}, key *string) error {
	body := block.Body()
	if body.GetAttribute(attr) == nil {
		return fmt.Errorf("attribute %q not present to absorb into", attr)
	}
	newVal, err := toCty(value)
	if err != nil {
		return err
	}
	if key == nil {
		body.SetAttributeValue(attr, newVal)
		return nil
	}

	// Instance-scoped: edit one entry of the attribute's map/object literal.
	cur, err := evalAttr(block, attr)
	if err != nil {
		return fmt.Errorf("cannot scope %q to instance %q: %w", attr, *key, err)
	}
	if !cur.Type().IsObjectType() && !cur.Type().IsMapType() {
		return fmt.Errorf("argument %q is not a per-instance map; editing it would affect all instances", attr)
	}
	m := cur.AsValueMap()
	if m == nil {
		m = map[string]cty.Value{}
	}
	m[*key] = newVal
	body.SetAttributeValue(attr, cty.ObjectVal(m))
	return nil
}

// evalAttr evaluates a block attribute's expression to a literal cty value.
// Fails if the expression is not a static literal.
func evalAttr(block *hclwrite.Block, attr string) (cty.Value, error) {
	a := block.Body().GetAttribute(attr)
	exprBytes := a.Expr().BuildTokens(nil).Bytes()
	expr, diags := hclsyntax.ParseExpression(exprBytes, "<attr>", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return cty.NilVal, fmt.Errorf("parse expression: %s", diags.Error())
	}
	v, diags := expr.Value(nil)
	if diags.HasErrors() {
		return cty.NilVal, fmt.Errorf("expression is not a static literal")
	}
	return v, nil
}

// toCty converts a JSON-decoded value into a cty.Value via its implied type.
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
