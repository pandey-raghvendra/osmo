// Package absorb rewrites Terraform HCL so that configuration follows
// real-world drift (the "absorb" direction).
//
// Scalar top-level attributes are traced through the plan's configuration
// tree (see internal/provenance) to the single literal that controls them.
//
// Nested block attributes — at any depth — are handled by a separate
// provenance-free engine that navigates the live HCL AST: it recursively
// diffs before/after maps, matches block instances by scoring stable
// attributes, edits leaf literals in place, and generates or removes blocks
// for structural (count-change) drift.
package absorb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/pandey-raghvendra/osmo/internal/address"
	"github.com/pandey-raghvendra/osmo/internal/blockid"
	"github.com/pandey-raghvendra/osmo/internal/config"
	"github.com/pandey-raghvendra/osmo/internal/provenance"
	"github.com/pandey-raghvendra/osmo/internal/tfplan"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// ---- Public output types ------------------------------------------------

// ResourceEdit records which attributes of one drifted resource were rewritten.
type ResourceEdit struct {
	Address string   // drifted resource address
	Attrs   []string // attribute names absorbed (sorted; nested use "block.attr" notation)
}

// FileChange is a proposed rewrite of one .tf file.
type FileChange struct {
	Path   string
	Before []byte
	After  []byte
	Edits  []ResourceEdit
}

// ---- Nested block path types --------------------------------------------

// BlockStep is one step when navigating into nested blocks. Before is the full
// snapshot of this specific block instance (for identity matching); Drifted
// contains only the keys that changed at this level (to exclude from stable-
// attr scoring).
type BlockStep struct {
	BlockType string
	Before    tfplan.TFState
	Drifted   tfplan.TFState
}

// BlockAttrEdit is a leaf-level scalar attribute change inside a nested block
// at any depth. Steps is the path from the resource body to the containing
// block; an empty Steps slice means the edit is at the resource's own body
// (this path is unused for the scalar/provenance flow but used in tests).
type BlockAttrEdit struct {
	DriftAddr string
	ResType   string
	ResName   string
	ResMode   string
	Steps     []BlockStep
	Attr      string
	Value     tfplan.TFValue
}

// BlockAttrRemove is a leaf-level scalar attribute removal inside a nested
// block. It is emitted when refreshed reality no longer has an attribute that
// prior state had.
type BlockAttrRemove struct {
	DriftAddr string
	ResType   string
	ResName   string
	ResMode   string
	Steps     []BlockStep
	Attr      string
}

// BlockStructuralChange is a nested block count change (add or remove) at some
// nesting depth. Steps is the path to the parent body; Added and Removed are
// the after- and before-attribute maps of the affected block instances.
type BlockStructuralChange struct {
	DriftAddr string
	ResType   string
	ResName   string
	ResMode   string
	Steps     []BlockStep
	BlockType string
	Added     []tfplan.TFState
	Removed   []tfplan.TFState
}

// DynamicBlockUpdate is emitted for every slice (nested-block) attribute in a
// drift. During the apply phase Plan() checks whether the block type is
// implemented with a Terraform `dynamic` block; if so, the for_each collection
// variable is updated to AfterFull instead of manipulating individual blocks.
// For literal blocks this op is a no-op (regular BlockAttrEdit / BlockStructural-
// Change ops handle them).
type DynamicBlockUpdate struct {
	DriftAddr string
	ResType   string
	ResName   string
	ResMode   string
	Steps     []BlockStep      // path to the body containing the dynamic block
	BlockType string           // the dynamic block's label, e.g. "ingress"
	AfterFull []tfplan.TFState // complete after-state of this block type
}

// ---- Plan ---------------------------------------------------------------

// Plan computes HCL rewrites for baseDir that absorb the given drifts.
// raw is the full `terraform show -json` output used for the configuration
// tree (scalar attr provenance). It returns proposed file changes and a list
// of drifts that could not be absorbed automatically.
func Plan(baseDir string, drifts []tfplan.Drift, raw []byte) ([]FileChange, []provenance.Unresolved, error) {
	root, err := config.Parse(raw)
	if err != nil {
		return nil, nil, err
	}

	idreg, err := blockid.Load(baseDir)
	if err != nil {
		return nil, nil, fmt.Errorf("load %s: %w", blockid.ConfigFile, err)
	}

	var unresolved []provenance.Unresolved

	type scalarOp struct {
		addr      address.Addr
		target    *provenance.Target
		driftAddr string
		attr      string
	}
	type scalarRemoveOp struct {
		addr      address.Addr
		driftAddr string
		attr      string
	}
	type nestedAttrOp struct {
		addr address.Addr
		edit BlockAttrEdit
	}
	type nestedAttrRemoveOp struct {
		addr   address.Addr
		remove BlockAttrRemove
	}
	type nestedStructOp struct {
		addr   address.Addr
		change BlockStructuralChange
	}

	var scalarOps []scalarOp
	var scalarRemoveOps []scalarRemoveOp
	var nestedAttrOps []nestedAttrOp
	var nestedAttrRemoveOps []nestedAttrRemoveOp
	var nestedStructOps []nestedStructOp

	type dynUpdateOp struct {
		addr   address.Addr
		update DynamicBlockUpdate
	}
	var dynUpdateOps []dynUpdateOp

	for _, d := range drifts {
		addr, err := address.Parse(d.Address)
		if err != nil {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: d.Address, Attr: "*",
				Reason: "unparseable address: " + err.Error(),
			})
			continue
		}

		// Scalar top-level attrs → provenance trace.
		for k, av := range d.After {
			bv := d.Before[k]
			if bv.Equal(av) {
				continue
			}
			if av.IsList() {
				continue // handled by walkDriftMap below
			}
			// Null after-value: attr removed from reality → remove from config.
			if av.IsNull() {
				scalarRemoveOps = append(scalarRemoveOps, scalarRemoveOp{addr, d.Address, k})
				continue
			}
			// Guard: sensitive after-value must not be written to plain-text config.
			if isSensitiveAttr(d.AfterSensitive, k) {
				unresolved = append(unresolved, provenance.Unresolved{
					Address: d.Address, Attr: k,
					Reason: "sensitive value — cannot absorb into plain-text config",
				})
				continue
			}
			t, u := provenance.Trace(root, addr, k, av.GoValue())
			if u != nil {
				unresolved = append(unresolved, *u)
				continue
			}
			scalarOps = append(scalarOps, scalarOp{addr, t, d.Address, k})
		}
		// Attrs present in before but absent from after: also removed from reality.
		for k, bv := range d.Before {
			if _, inAfter := d.After[k]; inAfter {
				continue
			}
			if bv.IsNull() {
				continue
			}
			if bv.IsList() {
				continue // structural removal handled by walkDriftMap
			}
			scalarRemoveOps = append(scalarRemoveOps, scalarRemoveOp{addr, d.Address, k})
		}

		// Nested block attrs + structural changes → recursive walk.
		// walkDriftMap skips root-level scalars (handled above) and emits
		// BlockAttrEdits for nested literals, BlockStructuralChanges for
		// count-change events, and DynamicBlockUpdates for every slice attr
		// (used if the block type is implemented with a `dynamic` block).
		blockEdits, blockRemoves, blockStructs, blockDynUpdates, blockUnresolved := walkDriftMap(idreg, addr, d.Address, nil, d.Before, d.After, d.AfterSensitive)
		unresolved = append(unresolved, blockUnresolved...)
		for _, e := range blockEdits {
			nestedAttrOps = append(nestedAttrOps, nestedAttrOp{addr, e})
		}
		for _, r := range blockRemoves {
			nestedAttrRemoveOps = append(nestedAttrRemoveOps, nestedAttrRemoveOp{addr, r})
		}
		for _, s := range blockStructs {
			nestedStructOps = append(nestedStructOps, nestedStructOp{addr, s})
		}
		for _, u := range blockDynUpdates {
			dynUpdateOps = append(dynUpdateOps, dynUpdateOp{addr, u})
		}
	}

	// ---- Apply phase ----

	editors := map[string]*dirEditor{}
	editsByPath := map[string]map[string]map[string]bool{} // path → addr → attr → true

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
		if de := editors[dir]; de != nil {
			return de, nil
		}
		de, err := newDirEditor(dir)
		if err != nil {
			return nil, err
		}
		editors[dir] = de
		return de, nil
	}

	// Scalar ops: provenance may redirect to a different (parent) module dir.
	for _, o := range scalarOps {
		// for_each map entry patching is handled locally (always in the resource's
		// own module dir); no provenance redirect is needed.
		if o.target.Kind == provenance.ForEachMapEntry {
			dir, err := resolveDir(root, baseDir, o.addr.Modules)
			if err != nil {
				unresolved = append(unresolved, provenance.Unresolved{
					Address: o.driftAddr, Attr: o.attr, Reason: err.Error()})
				continue
			}
			de, err := getEditor(dir)
			if err != nil {
				return nil, nil, err
			}
			path, u := de.applyForEachMapPatch(o.addr, *o.target.InstanceKey, o.target.Attr, tfplan.FromGoValue(o.target.Value))
			if u != nil {
				unresolved = append(unresolved, *u)
				continue
			}
			if path != "" {
				record(path, o.driftAddr, o.attr)
			}
			continue
		}

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

	// Scalar remove ops: remove root-level attrs deleted from reality.
	for _, o := range scalarRemoveOps {
		dir, err := resolveDir(root, baseDir, o.addr.Modules)
		if err != nil {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: o.driftAddr, Attr: o.attr, Reason: err.Error()})
			continue
		}
		de, err := getEditor(dir)
		if err != nil {
			return nil, nil, err
		}
		path, u := de.applyScalarRemove(o.addr, o.attr)
		if u != nil {
			unresolved = append(unresolved, *u)
			continue
		}
		if path != "" {
			record(path, o.driftAddr, o.attr+"-removed")
		}
	}

	// Nested attr ops: attempt literal edit in the resource's own dir; fall back
	// to provenance redirect if the attr is a var ref.
	for _, o := range nestedAttrOps {
		dir, err := resolveDir(root, baseDir, o.addr.Modules)
		if err != nil {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: o.edit.DriftAddr,
				Attr:    qualifiedAttr(o.edit.Steps, o.edit.Attr),
				Reason:  err.Error(),
			})
			continue
		}
		de, err := getEditor(dir)
		if err != nil {
			return nil, nil, err
		}
		path, qualAttr, redirect, u := de.applyBlockAttrEdit(o.edit, root)
		if u != nil {
			unresolved = append(unresolved, *u)
			continue
		}
		if redirect != nil {
			// Var ref traced to a different (parent) location — apply there.
			redirectDir, dirErr := resolveDir(root, baseDir, redirect.SourceModulePath)
			if dirErr != nil {
				unresolved = append(unresolved, provenance.Unresolved{
					Address: o.edit.DriftAddr, Attr: qualAttr, Reason: dirErr.Error(),
				})
				continue
			}
			rde, deErr := getEditor(redirectDir)
			if deErr != nil {
				return nil, nil, deErr
			}
			rpath, applyErr := rde.apply(redirect)
			if applyErr != nil {
				unresolved = append(unresolved, provenance.Unresolved{
					Address: o.edit.DriftAddr, Attr: qualAttr, Reason: applyErr.Error(),
				})
				continue
			}
			if rpath != "" {
				record(rpath, o.edit.DriftAddr, qualAttr)
			}
			continue
		}
		if path != "" {
			record(path, o.edit.DriftAddr, qualAttr)
		}
	}

	// Nested attr removals: remove literal/configured attrs that disappeared
	// from refreshed reality.
	for _, o := range nestedAttrRemoveOps {
		dir, err := resolveDir(root, baseDir, o.addr.Modules)
		if err != nil {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: o.remove.DriftAddr,
				Attr:    qualifiedAttr(o.remove.Steps, o.remove.Attr),
				Reason:  err.Error(),
			})
			continue
		}
		de, err := getEditor(dir)
		if err != nil {
			return nil, nil, err
		}
		path, qualAttr, u := de.applyBlockAttrRemove(o.remove)
		if u != nil {
			unresolved = append(unresolved, *u)
			continue
		}
		if path != "" {
			record(path, o.remove.DriftAddr, qualAttr)
		}
	}

	// Nested structural ops: add/remove blocks in the resource's source dir.
	for _, o := range nestedStructOps {
		dir, err := resolveDir(root, baseDir, o.addr.Modules)
		if err != nil {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: o.change.DriftAddr,
				Attr:    o.change.BlockType,
				Reason:  err.Error(),
			})
			continue
		}
		de, err := getEditor(dir)
		if err != nil {
			return nil, nil, err
		}
		path, absorbed, urs := de.applyBlockStructChange(o.change)
		unresolved = append(unresolved, urs...)
		for _, attr := range absorbed {
			record(path, o.change.DriftAddr, attr)
		}
	}

	// Dynamic block update ops: update the for_each collection variable when
	// drift involves a `dynamic` block rather than literal nested blocks.
	for _, o := range dynUpdateOps {
		dir, err := resolveDir(root, baseDir, o.addr.Modules)
		if err != nil {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: o.update.DriftAddr,
				Attr:    o.update.BlockType + ".for_each",
				Reason:  err.Error(),
			})
			continue
		}
		de, err := getEditor(dir)
		if err != nil {
			return nil, nil, err
		}
		qualAttr, redirect, u := de.applyDynamicBlockUpdate(o.update, o.addr, root)
		if u != nil {
			unresolved = append(unresolved, *u)
			continue
		}
		if redirect == nil {
			// No dynamic block found for this block type → regular ops handled it.
			continue
		}
		redirectDir, dirErr := resolveDir(root, baseDir, redirect.SourceModulePath)
		if dirErr != nil {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: o.update.DriftAddr, Attr: qualAttr, Reason: dirErr.Error(),
			})
			continue
		}
		rde, deErr := getEditor(redirectDir)
		if deErr != nil {
			return nil, nil, deErr
		}
		rpath, applyErr := rde.apply(redirect)
		if applyErr != nil {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: o.update.DriftAddr, Attr: qualAttr, Reason: applyErr.Error(),
			})
			continue
		}
		if rpath != "" {
			record(rpath, o.update.DriftAddr, qualAttr)
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
				After:  ff.bytes(),
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

// changedAttrs returns keys whose value differs between before and after.
func changedAttrs(before, after tfplan.TFState) tfplan.TFState {
	changed := tfplan.TFState{}
	for k, av := range after {
		if bv, ok := before[k]; !ok || !bv.Equal(av) {
			changed[k] = av
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			changed[k] = tfplan.TFValue{} // TFNull — key removed
		}
	}
	return changed
}

// resolveDir walks the module path from baseDir following local module sources.
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

// ---- dirEditor ----------------------------------------------------------

type tfFile struct {
	src   []byte
	f     *hclwrite.File
	dirty bool
	crlf  bool // original file used CRLF line endings
}

// bytes returns the formatted HCL, restoring CRLF line endings if the
// original file used them (hclwrite always emits LF).
func (tf *tfFile) bytes() []byte {
	out := tf.f.Bytes()
	if tf.crlf {
		out = bytes.ReplaceAll(out, []byte("\r\n"), []byte("\n")) // normalise first
		out = bytes.ReplaceAll(out, []byte("\n"), []byte("\r\n"))
	}
	return out
}

type dirEditor struct {
	dir   string
	files map[string]*tfFile
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
		crlf := bytes.Contains(src, []byte("\r\n"))
		parseSrc := src
		if crlf {
			parseSrc = bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
		}
		f, diags := hclwrite.ParseConfig(parseSrc, p, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return nil, fmt.Errorf("parse %s: %s", p, diags.Error())
		}
		de.files[p] = &tfFile{src: src, f: f, crlf: crlf}
	}
	return de, nil
}

// apply locates the Target's block and sets the attribute (scalar/provenance path).
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

// applyScalarRemove removes a root-level resource attribute that disappeared from
// refreshed reality. Only literal attributes inside the resource block itself are
// removed; if the attribute traces through a module arg or variable default, removal
// is unsafe and an Unresolved is returned so the caller can report it.
func (de *dirEditor) applyScalarRemove(addr address.Addr, attr string) (string, *provenance.Unresolved) {
	outerType := "resource"
	if addr.Mode == "data" {
		outerType = "data"
	}
	for path, ff := range de.files {
		resBlock := ff.f.Body().FirstMatchingBlock(outerType, []string{addr.Type, addr.Name})
		if resBlock == nil {
			continue
		}
		if resBlock.Body().GetAttribute(attr) == nil {
			// Not a literal in config (computed or not set) — nothing to remove.
			return path, nil
		}
		resBlock.Body().RemoveAttribute(attr)
		ff.dirty = true
		return path, nil
	}
	return "", nil
}

// applyForEachMapPatch patches a single attribute inside the for_each map
// literal for the given instanceKey. The for_each expression must be a fully
// literal object; any non-literal value in the map causes the patch to report
// Unresolved so callers can surface it cleanly.
//
//	resource "aws_instance" "web" {
//	  for_each = { a = { instance_type = "t3.micro" } }
//	  instance_type = each.value.instance_type
//	}
//
// For drift on aws_instance.web["a"], instanceKey="a", mapAttr="instance_type".
func (de *dirEditor) applyForEachMapPatch(addr address.Addr, instanceKey, mapAttr string, value tfplan.TFValue) (string, *provenance.Unresolved) { //nolint:unparam
	outerType := "resource"
	if addr.Mode == "data" {
		outerType = "data"
	}
	driftAddr := addr.Type + "." + addr.Name + `["` + instanceKey + `"]`

	unres := func(reason string) (string, *provenance.Unresolved) {
		return "", &provenance.Unresolved{Address: driftAddr, Attr: mapAttr, Reason: reason}
	}

	for path, ff := range de.files {
		resBlock := ff.f.Body().FirstMatchingBlock(outerType, []string{addr.Type, addr.Name})
		if resBlock == nil {
			continue
		}
		feAttr := resBlock.Body().GetAttribute("for_each")
		if feAttr == nil {
			return unres("resource has no for_each attribute")
		}

		// Extract current expression bytes from the hclwrite token stream.
		exprBytes := feAttr.Expr().BuildTokens(nil).Bytes()

		expr, diags := hclsyntax.ParseExpression(exprBytes, "<for_each>", hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return unres("cannot parse for_each expression: " + diags.Error())
		}
		obj, ok := expr.(*hclsyntax.ObjectConsExpr)
		if !ok {
			return unres("for_each is not a literal object expression — cannot patch automatically")
		}

		for _, item := range obj.Items {
			key, ok := hclObjectKey(item.KeyExpr)
			if !ok || key != instanceKey {
				continue
			}

			// mapAttr == "" means each.value direct: for_each = {a = "scalar"}.
			// Patch the entry value itself, not a nested attribute.
			if mapAttr == "" {
				if !isHCLLiteral(item.ValueExpr) {
					return unres(fmt.Sprintf(
						"for_each[%q] value is not a literal; cannot patch automatically", instanceKey))
				}
				newValBytes, err := renderTFValue(value)
				if err != nil {
					return unres(err.Error())
				}
				vr := item.ValueExpr.Range()
				start, end := vr.Start.Byte, vr.End.Byte
				newExpr := make([]byte, 0, len(exprBytes)+len(newValBytes))
				newExpr = append(newExpr, exprBytes[:start]...)
				newExpr = append(newExpr, newValBytes...)
				newExpr = append(newExpr, exprBytes[end:]...)
				tmpSrc := append([]byte("x = "), append(bytes.TrimRight(newExpr, "\n"), '\n')...)
				tmpFile, tmpDiags := hclwrite.ParseConfig(tmpSrc, "<tmp>", hcl.Pos{Line: 1, Column: 1})
				if tmpDiags.HasErrors() {
					return unres("rebuild for_each after scalar patch: " + tmpDiags.Error())
				}
				resBlock.Body().SetAttributeRaw("for_each", tmpFile.Body().GetAttribute("x").Expr().BuildTokens(nil))
				ff.dirty = true
				return path, nil
			}

			// mapAttr != "" means each.value.X: for_each = {a = {attr = "val"}}.
			innerObj, ok := item.ValueExpr.(*hclsyntax.ObjectConsExpr)
			if !ok {
				return unres(fmt.Sprintf("for_each value for key %q is not an object literal", instanceKey))
			}
			for _, inner := range innerObj.Items {
				attr, ok := hclObjectKey(inner.KeyExpr)
				if !ok || attr != mapAttr {
					continue
				}
				if !isHCLLiteral(inner.ValueExpr) {
					return unres(fmt.Sprintf(
						"for_each[%q].%s is not a literal value; cannot patch automatically", instanceKey, mapAttr))
				}
				newValBytes, err := renderTFValue(value)
				if err != nil {
					return unres(err.Error())
				}
				vr := inner.ValueExpr.Range()
				start, end := vr.Start.Byte, vr.End.Byte
				newExpr := make([]byte, 0, len(exprBytes)+len(newValBytes))
				newExpr = append(newExpr, exprBytes[:start]...)
				newExpr = append(newExpr, newValBytes...)
				newExpr = append(newExpr, exprBytes[end:]...)
				tmpSrc := append([]byte("x = "), append(bytes.TrimRight(newExpr, "\n"), '\n')...)
				tmpFile, tmpDiags := hclwrite.ParseConfig(tmpSrc, "<tmp>", hcl.Pos{Line: 1, Column: 1})
				if tmpDiags.HasErrors() {
					return unres("rebuild for_each after patch: " + tmpDiags.Error())
				}
				resBlock.Body().SetAttributeRaw("for_each", tmpFile.Body().GetAttribute("x").Expr().BuildTokens(nil))
				ff.dirty = true
				return path, nil
			}
			return unres(fmt.Sprintf("attribute %q not found in for_each[%q]", mapAttr, instanceKey))
		}
		return unres(fmt.Sprintf("key %q not found in for_each map literal", instanceKey))
	}
	return "", nil // resource not in this directory's files
}

// hclObjectKey extracts a string key from an HCL object expression item key.
// Handles bare identifiers (a = ...) and quoted string literals ("a" = ...).
// Object keys are always wrapped in ObjectConsKeyExpr; we unwrap first.
func hclObjectKey(expr hclsyntax.Expression) (string, bool) {
	// Unwrap the outer ObjectConsKeyExpr that the parser always adds.
	if wk, ok := expr.(*hclsyntax.ObjectConsKeyExpr); ok {
		expr = wk.Wrapped
	}
	switch e := expr.(type) {
	case *hclsyntax.LiteralValueExpr:
		if e.Val.Type() == cty.String {
			return e.Val.AsString(), true
		}
	case *hclsyntax.TemplateWrapExpr:
		if lit, ok := e.Wrapped.(*hclsyntax.LiteralValueExpr); ok && lit.Val.Type() == cty.String {
			return lit.Val.AsString(), true
		}
	case *hclsyntax.ScopeTraversalExpr:
		// Bare identifier key: `a = ...`
		if len(e.Traversal) == 1 {
			if root, ok := e.Traversal[0].(hcl.TraverseRoot); ok {
				return root.Name, true
			}
		}
	}
	return "", false
}

// isHCLLiteral reports whether expr contains no variable references and can
// therefore be safely replaced with a new literal value.
func isHCLLiteral(expr hclsyntax.Expression) bool {
	return len(expr.Variables()) == 0
}

// renderTFValue serialises a scalar TFValue to its HCL token representation.
func renderTFValue(v tfplan.TFValue) ([]byte, error) {
	if s, ok := v.AsString(); ok {
		return []byte(`"` + hclEscapeString(s) + `"`), nil
	}
	if n, ok := v.AsNumber(); ok {
		return []byte(strconv.FormatFloat(n, 'f', -1, 64)), nil
	}
	if b, ok := v.AsBool(); ok {
		if b {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	}
	return nil, fmt.Errorf("cannot render non-scalar TFValue as HCL literal")
}

// hclEscapeString escapes characters that are special inside HCL string literals.
func hclEscapeString(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

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
	cur, err := evalBodyAttr(body, attr)
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

// ---- Nested block engine ------------------------------------------------

// walkDriftMap recursively walks a before/after map pair and emits:
//   - BlockAttrEdits for scalar attr changes INSIDE nested blocks (path != nil)
//   - BlockAttrRemoves for scalar attrs removed from refreshed nested blocks
//   - BlockStructuralChanges for nested block count changes at any depth
//   - DynamicBlockUpdates for every slice attr (used if the block type is a
//     Terraform `dynamic` block; no-op otherwise)
//
// Root-level scalar attrs (path == nil) are skipped because those go through
// the provenance path in Plan.
func walkDriftMap(
	idreg *blockid.Registry,
	addr address.Addr,
	driftAddr string,
	path []BlockStep,
	before, after tfplan.TFState,
	afterSensitive tfplan.TFValue,
) (edits []BlockAttrEdit, removes []BlockAttrRemove, structs []BlockStructuralChange, dynUpdates []DynamicBlockUpdate, unresolved []provenance.Unresolved) {
	for k, av := range after {
		bv := before[k]
		if bv.Equal(av) {
			continue
		}
		qualAttr := qualifiedAttr(path, k)
		afterList, isList := av.AsList()
		if !isList {
			if len(path) == 0 {
				// Root-level scalar: handled via provenance in Plan.
				continue
			}
			if av.IsNull() {
				// Explicit null in after = attr removed from reality; treat same as absent key.
				removes = append(removes, BlockAttrRemove{
					DriftAddr: driftAddr,
					ResType:   addr.Type,
					ResName:   addr.Name,
					ResMode:   addr.Mode,
					Steps:     path,
					Attr:      k,
				})
				continue
			}
			if isSensitiveNested(afterSensitive, k) {
				unresolved = append(unresolved, provenance.Unresolved{
					Address: driftAddr, Attr: qualAttr,
					Reason: "sensitive value — cannot absorb into plain-text config",
				})
				continue
			}
			// Nested scalar attr change.
			edits = append(edits, BlockAttrEdit{
				DriftAddr: driftAddr,
				ResType:   addr.Type,
				ResName:   addr.Name,
				ResMode:   addr.Mode,
				Steps:     path,
				Attr:      k,
				Value:     av,
			})
			continue
		}

		// Nested block (list value).
		var beforeList []tfplan.TFValue
		if bl, ok := bv.AsList(); ok {
			beforeList = bl
		}
		bMaps := toStateSlice(beforeList)
		aMaps := toStateSlice(afterList)
		sensitiveElems := sensitiveSlice(afterSensitive, k)

		if hasSensitiveValue(sensitiveElems) {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: driftAddr, Attr: qualAttr + ".for_each",
				Reason: "sensitive value — cannot absorb dynamic block collection into plain-text config",
			})
		} else {
			dynUpdates = append(dynUpdates, DynamicBlockUpdate{
				DriftAddr: driftAddr,
				ResType:   addr.Type,
				ResName:   addr.Name,
				ResMode:   addr.Mode,
				Steps:     path,
				BlockType: k,
				AfterFull: aMaps,
			})
		}

		matches, addedIdx, removedIdx, matchErr := matchBlockElements(idreg, addr.Type, blockPath(path, k), bMaps, aMaps)
		if matchErr != nil {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: driftAddr, Attr: qualAttr,
				Reason: matchErr.Error(),
			})
			continue
		}
		if len(addedIdx) > 0 || len(removedIdx) > 0 {
			var added, removed []tfplan.TFState
			for _, i := range addedIdx {
				if hasSensitiveValue(sensitiveAt(sensitiveElems, i)) {
					unresolved = append(unresolved, provenance.Unresolved{
						Address: driftAddr, Attr: qualAttr,
						Reason: "sensitive value in added block — cannot absorb into plain-text config",
					})
					continue
				}
				added = append(added, aMaps[i])
			}
			for _, i := range removedIdx {
				removed = append(removed, bMaps[i])
			}
			structs = append(structs, BlockStructuralChange{
				DriftAddr: driftAddr,
				ResType:   addr.Type,
				ResName:   addr.Name,
				ResMode:   addr.Mode,
				Steps:     path,
				BlockType: k,
				Added:     added,
				Removed:   removed,
			})
		}
		for _, pair := range matches {
			bm, am := bMaps[pair[0]], aMaps[pair[1]]
			drifted := changedAttrs(bm, am)
			if len(drifted) == 0 {
				continue
			}
			newStep := BlockStep{BlockType: k, Before: bm, Drifted: drifted}
			newPath := append(append([]BlockStep(nil), path...), newStep)
			subEdits, subRemoves, subStructs, subDyn, subUnresolved := walkDriftMap(idreg, addr, driftAddr, newPath, bm, am, sensitiveAt(sensitiveElems, pair[1]))
			edits = append(edits, subEdits...)
			removes = append(removes, subRemoves...)
			structs = append(structs, subStructs...)
			dynUpdates = append(dynUpdates, subDyn...)
			unresolved = append(unresolved, subUnresolved...)
		}
	}
	for k, bv := range before {
		if _, ok := after[k]; ok {
			continue
		}
		if len(path) == 0 {
			// Root-level removals are not edited automatically.
			continue
		}
		beforeList, isList := bv.AsList()
		if !isList {
			removes = append(removes, BlockAttrRemove{
				DriftAddr: driftAddr,
				ResType:   addr.Type,
				ResName:   addr.Name,
				ResMode:   addr.Mode,
				Steps:     path,
				Attr:      k,
			})
			continue
		}
		bMaps := toStateSlice(beforeList)
		dynUpdates = append(dynUpdates, DynamicBlockUpdate{
			DriftAddr: driftAddr,
			ResType:   addr.Type,
			ResName:   addr.Mode,
			Steps:     path,
			BlockType: k,
			AfterFull: nil,
		})
		structs = append(structs, BlockStructuralChange{
			DriftAddr: driftAddr,
			ResType:   addr.Type,
			ResName:   addr.Name,
			ResMode:   addr.Mode,
			Steps:     path,
			BlockType: k,
			Removed:   bMaps,
		})
	}
	return
}

// matchBlockElements greedily matches before elements to after elements by
// attribute similarity, returning matched index pairs, unmatched after indices
// (added), and unmatched before indices (removed).
func matchBlockElements(idreg *blockid.Registry, resourceType string, path []string, before, after []tfplan.TFState) (matches [][2]int, added, removed []int, err error) {
	if len(before) == 1 && len(after) == 1 && !hasDifferentNameIdentity(before[0], after[0]) {
		return [][2]int{{0, 0}}, nil, nil, nil
	}
	identity := idreg.Keys(resourceType, path)
	used := make([]bool, len(after))
	for bi, bm := range before {
		bestScore, bestAi := -1, -1
		ties := 0
		for ai, am := range after {
			if used[ai] {
				continue
			}
			s := mapMatchScore(bm, am, identity)
			switch {
			case s > bestScore:
				bestScore, bestAi = s, ai
				ties = 1
			case s == bestScore:
				ties++
			}
		}
		if bestAi >= 0 && bestScore > 0 {
			if ties > 1 {
				return nil, nil, nil, fmt.Errorf("ambiguous nested block collection match: before element %d matched multiple after elements equally", bi)
			}
			matches = append(matches, [2]int{bi, bestAi})
			used[bestAi] = true
		} else {
			removed = append(removed, bi)
		}
	}
	for ai := range after {
		if !used[ai] {
			added = append(added, ai)
		}
	}
	return
}

func mapMatchScore(a, b tfplan.TFState, identity []string) int {
	if len(identity) > 0 {
		return identityMatchScore(a, b, identity)
	}
	if hasDifferentNameIdentity(a, b) {
		return 0
	}
	score := 0
	for k, av := range a {
		if bv, ok := b[k]; ok && bv.Equal(av) {
			score++
		}
	}
	return score
}

func identityMatchScore(a, b tfplan.TFState, keys []string) int {
	score := 0
	for _, k := range keys {
		av, aOK := a[k]
		bv, bOK := b[k]
		if !aOK || !bOK {
			continue
		}
		if !bv.Equal(av) {
			return 0
		}
		score += 100
	}
	if score == 0 {
		return mapMatchScore(a, b, nil)
	}
	return score
}

func hasDifferentNameIdentity(a, b tfplan.TFState) bool {
	av, aOK := a["name"]
	bv, bOK := b["name"]
	return aOK && bOK && !av.Equal(bv)
}

func canAddMissingNestedAttr(edit BlockAttrEdit) bool {
	path := blockPath(edit.Steps, "")
	if len(path) > 0 && path[len(path)-1] == "" {
		path = path[:len(path)-1]
	}
	if edit.ResType == "azurerm_application_gateway" && len(path) > 0 {
		switch path[len(path)-1] {
		case "backend_http_settings":
			return edit.Attr == "probe_name"
		case "url_path_map", "path_rule":
			switch edit.Attr {
			case "backend_address_pool_name", "backend_http_settings_name", "redirect_configuration_name", "rewrite_rule_set_name":
				return true
			}
		}
	}
	return false
}

func blockPath(steps []BlockStep, next string) []string {
	path := make([]string, 0, len(steps)+1)
	for _, s := range steps {
		path = append(path, s.BlockType)
	}
	return append(path, next)
}

func toStateSlice(list []tfplan.TFValue) []tfplan.TFState {
	result := make([]tfplan.TFState, 0, len(list))
	for _, v := range list {
		if obj, ok := v.AsObject(); ok {
			result = append(result, obj)
		}
	}
	return result
}

func (de *dirEditor) applyBlockAttrEdit(edit BlockAttrEdit, root *config.Module) (path, qualAttr string, redirect *provenance.Target, u *provenance.Unresolved) {
	outerType := "resource"
	if edit.ResMode == "data" {
		outerType = "data"
	}
	qualAttr = qualifiedAttr(edit.Steps, edit.Attr)

	for p, ff := range de.files {
		resBlock := ff.f.Body().FirstMatchingBlock(outerType, []string{edit.ResType, edit.ResName})
		if resBlock == nil {
			continue
		}
		targetBody, err := navigateNestedPath(resBlock.Body(), edit.Steps)
		if err != nil {
			if isAmbiguousNestedMatch(err) {
				return p, qualAttr, nil, &provenance.Unresolved{
					Address: edit.DriftAddr, Attr: qualAttr,
					Reason: err.Error(),
				}
			}
			// Block path not found in this file; try the next.
			continue
		}
		if targetBody.GetAttribute(edit.Attr) == nil {
			if canAddMissingNestedAttr(edit) {
				newVal, ctyErr := toCty(edit.Value.GoValue())
				if ctyErr != nil {
					return p, qualAttr, nil, &provenance.Unresolved{
						Address: edit.DriftAddr, Attr: qualAttr, Reason: ctyErr.Error(),
					}
				}
				targetBody.SetAttributeValue(edit.Attr, newVal)
				ff.dirty = true
				return p, qualAttr, nil, nil
			}
			// Computed / not in config: skip silently (same rule as top-level).
			return p, qualAttr, nil, nil
		}
		if _, evalErr := evalBodyAttr(targetBody, edit.Attr); evalErr != nil {
			// Attribute is a variable reference — try to trace via config tree.
			if root != nil {
				addr, parseErr := address.Parse(edit.DriftAddr)
				if parseErr == nil {
					blockPath := make([]string, len(edit.Steps))
					for i, s := range edit.Steps {
						blockPath[i] = s.BlockType
					}
					tgt, tu := provenance.TraceNested(root, addr, blockPath, edit.Attr, edit.Value.GoValue())
					if tu != nil {
						return p, qualAttr, nil, tu
					}
					return p, qualAttr, tgt, nil
				}
			}
			return p, qualAttr, nil, &provenance.Unresolved{
				Address: edit.DriftAddr, Attr: qualAttr,
				Reason: "nested block attr is a variable reference; cannot parse drift address for tracing",
			}
		}
		newVal, ctyErr := toCty(edit.Value.GoValue())
		if ctyErr != nil {
			return p, qualAttr, nil, &provenance.Unresolved{
				Address: edit.DriftAddr, Attr: qualAttr, Reason: ctyErr.Error(),
			}
		}
		targetBody.SetAttributeValue(edit.Attr, newVal)
		ff.dirty = true
		return p, qualAttr, nil, nil
	}
	return "", qualAttr, nil, nil
}

// applyBlockAttrRemove navigates to a nested block and removes an attribute
// that disappeared from refreshed reality.
func (de *dirEditor) applyBlockAttrRemove(remove BlockAttrRemove) (path, qualAttr string, u *provenance.Unresolved) {
	outerType := "resource"
	if remove.ResMode == "data" {
		outerType = "data"
	}
	qualAttr = qualifiedAttr(remove.Steps, remove.Attr)

	for p, ff := range de.files {
		resBlock := ff.f.Body().FirstMatchingBlock(outerType, []string{remove.ResType, remove.ResName})
		if resBlock == nil {
			continue
		}
		targetBody, err := navigateNestedPath(resBlock.Body(), remove.Steps)
		if err != nil {
			if isAmbiguousNestedMatch(err) {
				return p, qualAttr, &provenance.Unresolved{
					Address: remove.DriftAddr, Attr: qualAttr,
					Reason: err.Error(),
				}
			}
			continue
		}
		if targetBody.GetAttribute(remove.Attr) == nil {
			return p, qualAttr, nil
		}
		targetBody.RemoveAttribute(remove.Attr)
		ff.dirty = true
		return p, qualAttr + "-removed", nil
	}
	return "", qualAttr, nil
}

// applyBlockStructChange adds and removes nested blocks in the parent body
// described by sc.Steps, within the resource block in de's files.
func (de *dirEditor) applyBlockStructChange(sc BlockStructuralChange) (path string, absorbed []string, unresolved []provenance.Unresolved) {
	outerType := "resource"
	if sc.ResMode == "data" {
		outerType = "data"
	}

	for p, ff := range de.files {
		resBlock := ff.f.Body().FirstMatchingBlock(outerType, []string{sc.ResType, sc.ResName})
		if resBlock == nil {
			continue
		}
		parentBody, err := navigateNestedPath(resBlock.Body(), sc.Steps)
		if err != nil {
			// Navigation may fail because an intermediate step is a dynamic block.
			// In that case DynamicBlockUpdate ops handle the change; skip silently.
			if isDynamicAtPath(resBlock.Body(), sc.Steps) {
				return p, nil, nil
			}
			unresolved = append(unresolved, provenance.Unresolved{
				Address: sc.DriftAddr, Attr: sc.BlockType,
				Reason: "could not navigate to parent block: " + err.Error(),
			})
			return p, nil, unresolved
		}

		// If the block type is managed by a dynamic block, skip direct block
		// manipulation — DynamicBlockUpdate ops handle the for_each update.
		if findDynamicBlock(parentBody, sc.BlockType) != nil {
			return p, nil, nil
		}

		// Remove blocks that disappeared out-of-band.
		for _, bm := range sc.Removed {
			block, matchErr := findMatchingNestedBlock(parentBody, sc.BlockType, bm, tfplan.TFState{})
			if matchErr != nil {
				unresolved = append(unresolved, provenance.Unresolved{
					Address: sc.DriftAddr, Attr: sc.BlockType,
					Reason: "could not find matching block to remove: " + matchErr.Error(),
				})
				continue
			}
			if block == nil {
				unresolved = append(unresolved, provenance.Unresolved{
					Address: sc.DriftAddr, Attr: sc.BlockType,
					Reason: "could not find matching block to remove",
				})
				continue
			}
			parentBody.RemoveBlock(block)
			absorbed = append(absorbed, sc.BlockType+"-removed")
			ff.dirty = true
		}

		// Append blocks that were added out-of-band.
		for _, am := range sc.Added {
			newBlock := generateHCLBlock(sc.BlockType, am)
			parentBody.AppendBlock(newBlock)
			absorbed = append(absorbed, sc.BlockType+"+added")
			ff.dirty = true
		}

		return p, absorbed, unresolved
	}
	return "", nil, unresolved
}

// navigateNestedPath walks steps from root, following each step's block type
// by matching. Returns the body of the deepest block reached.
func navigateNestedPath(root *hclwrite.Body, steps []BlockStep) (*hclwrite.Body, error) {
	cur := root
	for _, step := range steps {
		block, err := findMatchingNestedBlock(cur, step.BlockType, step.Before, step.Drifted)
		if err != nil {
			return nil, err
		}
		if block == nil {
			return nil, fmt.Errorf("nested block %q not found", step.BlockType)
		}
		cur = block.Body()
	}
	return cur, nil
}

// findMatchingNestedBlock returns the first nested block of blockType that
// best matches the before snapshot. For a single candidate it returns
// immediately. For multiple candidates it scores by how many stable
// (non-drifted) before attributes have matching literal values.
func findMatchingNestedBlock(
	body *hclwrite.Body,
	blockType string,
	before, drifted tfplan.TFState,
) (*hclwrite.Block, error) {
	var candidates []*hclwrite.Block
	for _, b := range body.Blocks() {
		if b.Type() == blockType {
			candidates = append(candidates, b)
		}
	}
	switch len(candidates) {
	case 0:
		return nil, nil
	case 1:
		return candidates[0], nil
	default:
		stable := make(tfplan.TFState, len(before))
		for k, v := range before {
			if _, isDrifted := drifted[k]; !isDrifted {
				stable[k] = v
			}
		}
		best, bestScore := candidates[0], -1
		ties := 0
		for _, c := range candidates {
			s := bodyMatchScore(c.Body(), stable)
			switch {
			case s > bestScore:
				bestScore = s
				best = c
				ties = 1
			case s == bestScore:
				ties++
			}
		}
		if bestScore <= 0 {
			return nil, fmt.Errorf("ambiguous nested block %q match: no stable literal attributes matched", blockType)
		}
		if ties > 1 {
			return nil, fmt.Errorf("ambiguous nested block %q match: multiple blocks matched equally", blockType)
		}
		return best, nil
	}
}

// bodyMatchScore counts how many stable attrs have matching literal values in
// the block body. Comparison is JSON-marshal-based for type safety.
func bodyMatchScore(body *hclwrite.Body, stableAttrs tfplan.TFState) int {
	score := 0
	for attr, expected := range stableAttrs {
		if body.GetAttribute(attr) == nil {
			continue
		}
		curCty, err := evalBodyAttr(body, attr)
		if err != nil {
			continue
		}
		expCty, err := toCty(expected.GoValue())
		if err != nil {
			continue
		}
		curJ, e1 := ctyjson.Marshal(curCty, curCty.Type())
		expJ, e2 := ctyjson.Marshal(expCty, expCty.Type())
		if e1 == nil && e2 == nil && string(curJ) == string(expJ) {
			score++
		}
	}
	return score
}

func isAmbiguousNestedMatch(err error) bool {
	return err != nil && strings.Contains(err.Error(), "ambiguous nested block")
}

// generateHCLBlock creates an hclwrite.Block from a TFState attribute map.
// List values are treated as nested blocks (recursed); all others are scalars.
func generateHCLBlock(blockType string, attrs tfplan.TFState) *hclwrite.Block {
	b := hclwrite.NewBlock(blockType, nil)
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := attrs[k]
		if v.IsNull() {
			continue
		}
		if list, ok := v.AsList(); ok {
			for _, elem := range list {
				if obj, ok := elem.AsObject(); ok {
					b.Body().AppendBlock(generateHCLBlock(k, obj))
				}
			}
			continue
		}
		if ctyVal, err := toCty(v.GoValue()); err == nil {
			b.Body().SetAttributeValue(k, ctyVal)
		}
	}
	return b
}

// ---- Dynamic block engine -----------------------------------------------

// applyDynamicBlockUpdate looks for a `dynamic "blockType"` block in the
// resource body navigated by u.Steps. If found, it extracts the for_each
// variable name and returns a provenance Target that updates the collection
// variable to u.AfterFull. Returns (qualAttr, nil, nil) when no dynamic block
// exists (regular ops cover literal blocks).
func (de *dirEditor) applyDynamicBlockUpdate(u DynamicBlockUpdate, addr address.Addr, root *config.Module) (qualAttr string, redirect *provenance.Target, ur *provenance.Unresolved) {
	outerType := "resource"
	if u.ResMode == "data" {
		outerType = "data"
	}
	qualAttr = u.BlockType + ".for_each"

	for _, ff := range de.files {
		resBlock := ff.f.Body().FirstMatchingBlock(outerType, []string{u.ResType, u.ResName})
		if resBlock == nil {
			continue
		}
		parentBody, err := navigateNestedPath(resBlock.Body(), u.Steps)
		if err != nil {
			continue // intermediate step not found — not our file
		}
		dynBlock := findDynamicBlock(parentBody, u.BlockType)
		if dynBlock == nil {
			// No dynamic block — literal blocks handled by regular ops.
			return qualAttr, nil, nil
		}
		varName, extractErr := extractForEachVarName(dynBlock.Body())
		if extractErr != nil {
			return qualAttr, nil, &provenance.Unresolved{
				Address: u.DriftAddr, Attr: qualAttr, Reason: extractErr.Error(),
			}
		}
		// Validate the current for_each value is a list/set (not a map) —
		// map-keyed for_each would need different reconstruction.
		if forEachIsMap(dynBlock.Body()) {
			return qualAttr, nil, &provenance.Unresolved{
				Address: u.DriftAddr, Attr: qualAttr,
				Reason: "dynamic block uses a map for_each; collection reconstruction from drift not supported — update " + varName + " manually",
			}
		}
		afterCollection := mapsToInterfaceSlice(u.AfterFull)
		tgt, tu := provenance.TraceForEach(root, addr, varName, afterCollection)
		if tu != nil {
			return qualAttr, nil, tu
		}
		return qualAttr, tgt, nil
	}
	return qualAttr, nil, nil
}

// findDynamicBlock returns the first `dynamic "blockType"` block in body, or nil.
func findDynamicBlock(body *hclwrite.Body, blockType string) *hclwrite.Block {
	for _, b := range body.Blocks() {
		if b.Type() == "dynamic" && len(b.Labels()) == 1 && b.Labels()[0] == blockType {
			return b
		}
	}
	return nil
}

// extractForEachVarName reads the for_each attribute from a dynamic block body
// and returns the variable name if it is a direct `var.x` reference.
func extractForEachVarName(body *hclwrite.Body) (string, error) {
	attr := body.GetAttribute("for_each")
	if attr == nil {
		return "", fmt.Errorf("dynamic block has no for_each attribute")
	}
	exprBytes := bytes.TrimSpace(attr.Expr().BuildTokens(nil).Bytes())
	expr, diags := hclsyntax.ParseExpression(exprBytes, "<for_each>", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return "", fmt.Errorf("parse for_each expression: %s", diags.Error())
	}
	tv, ok := expr.(*hclsyntax.ScopeTraversalExpr)
	if !ok {
		return "", fmt.Errorf("for_each is a composed expression; cannot trace automatically")
	}
	traversal := tv.Traversal
	switch traversal.RootName() {
	case "var":
		if len(traversal) != 2 {
			return "", fmt.Errorf("for_each references a sub-attribute of a variable (unsupported)")
		}
		ta, ok := traversal[1].(hcl.TraverseAttr)
		if !ok {
			return "", fmt.Errorf("unexpected traversal step type in for_each var reference")
		}
		return ta.Name, nil
	case "local":
		return "", fmt.Errorf("for_each derives from a local; locals are not in plan JSON")
	default:
		return "", fmt.Errorf("for_each references %q (not a direct var.x reference)", traversal.RootName())
	}
}

// forEachIsMap returns true when the dynamic block's for_each expression
// evaluates to a map/object type. Heuristic: if the for_each literal evaluates
// without variable errors to a map/object cty value.
func forEachIsMap(body *hclwrite.Body) bool {
	v, err := evalBodyAttr(body, "for_each")
	if err != nil {
		// Variable ref — can't tell statically. Conservative: assume list.
		return false
	}
	return v.Type().IsObjectType() || v.Type().IsMapType()
}

// isDynamicAtPath walks steps from root to check whether any step's block type
// is absent as a literal block but present as a dynamic block. Used to
// distinguish "navigation failed because of dynamic block" from other errors.
func isDynamicAtPath(root *hclwrite.Body, steps []BlockStep) bool {
	cur := root
	for _, step := range steps {
		block, _ := findMatchingNestedBlock(cur, step.BlockType, step.Before, step.Drifted)
		if block == nil {
			return findDynamicBlock(cur, step.BlockType) != nil
		}
		cur = block.Body()
	}
	return false
}

// mapsToInterfaceSlice converts []TFState to []interface{} for provenance.TraceForEach.
func mapsToInterfaceSlice(maps []tfplan.TFState) []interface{} {
	result := make([]interface{}, len(maps))
	for i, m := range maps {
		obj := make(map[string]interface{}, len(m))
		for k, v := range m {
			obj[k] = v.GoValue()
		}
		result[i] = obj
	}
	return result
}

// qualifiedAttr returns "blockA.blockB.attr" notation for a nested attr.
func qualifiedAttr(steps []BlockStep, attr string) string {
	if len(steps) == 0 {
		return attr
	}
	parts := make([]string, 0, len(steps)+1)
	for _, s := range steps {
		parts = append(parts, blockStepLabel(s))
	}
	parts = append(parts, attr)
	return strings.Join(parts, ".")
}

func blockStepLabel(step BlockStep) string {
	if name, ok := step.Before["name"]; ok {
		if s, strOK := name.AsString(); strOK && s != "" {
			return step.BlockType + "[" + quoteHCLString(s) + "]"
		}
	}
	return step.BlockType
}

func quoteHCLString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `"` + s + `"`
	}
	return string(b)
}

// ---- Shared HCL utilities -----------------------------------------------

// evalBodyAttr evaluates a body's attribute expression as a static literal.
// Returns an error if the expression contains variable references.
func evalBodyAttr(body *hclwrite.Body, attr string) (cty.Value, error) {
	a := body.GetAttribute(attr)
	if a == nil {
		return cty.NilVal, fmt.Errorf("attribute %q not found", attr)
	}
	exprBytes := a.Expr().BuildTokens(nil).Bytes()
	expr, diags := hclsyntax.ParseExpression(exprBytes, "<attr>", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return cty.NilVal, fmt.Errorf("parse expression: %s", diags.Error())
	}
	v, diags := expr.Value(nil)
	if diags.HasErrors() || !v.IsWhollyKnown() {
		// diags errors: variable not found; !IsWhollyKnown: var ref evaluated to
		// cty.DynamicVal — in both cases the expression is not a static literal.
		return cty.NilVal, fmt.Errorf("expression is not a static literal")
	}
	return v, nil
}

// evalAttr is a convenience wrapper for callers that have a block reference.
func evalAttr(block *hclwrite.Block, attr string) (cty.Value, error) {
	return evalBodyAttr(block.Body(), attr)
}

// isSensitiveAttr returns true when attr is marked sensitive in afterSensitive.
//
// Terraform plan JSON uses after_sensitive as follows:
//   - bool true  → the whole attribute value is sensitive
//   - bool false → not sensitive
//   - {}         → nested attribute exists but has no sensitive values
//   - {key:true} → a nested key is sensitive, but the attr itself may not be
//
// We guard conservatively: skip only when the attr's entry is exactly the
// boolean true. An empty map {}, false, or a nested map means the top-level
// attr value is not wholly sensitive and can be absorbed safely.
func isSensitiveAttr(afterSensitive tfplan.TFValue, attr string) bool {
	if afterSensitive.IsNull() {
		return false
	}
	if b, ok := afterSensitive.AsBool(); ok {
		return b
	}
	obj, ok := afterSensitive.AsObject()
	if !ok {
		return false
	}
	v := obj[attr]
	b, ok := v.AsBool()
	return ok && b
}

func isSensitiveNested(afterSensitive tfplan.TFValue, attr string) bool {
	if afterSensitive.IsNull() {
		return false
	}
	if b, ok := afterSensitive.AsBool(); ok {
		return b
	}
	obj, ok := afterSensitive.AsObject()
	if !ok {
		return false
	}
	return hasSensitiveValue(obj[attr])
}

// sensitiveSlice returns the sensitivity descriptor for a slice attribute.
// Result is a TFList of per-element sensitivity flags, or TFBool(true) if the
// entire attribute is sensitive, or TFNull if not sensitive.
func sensitiveSlice(afterSensitive tfplan.TFValue, attr string) tfplan.TFValue {
	if afterSensitive.IsNull() {
		return tfplan.TFValue{}
	}
	if b, ok := afterSensitive.AsBool(); ok && b {
		return tfplan.TFBoolVal(true)
	}
	obj, ok := afterSensitive.AsObject()
	if !ok {
		return tfplan.TFValue{}
	}
	return obj[attr]
}

// sensitiveAt returns the sensitivity descriptor for element i of a slice
// sensitivity value returned by sensitiveSlice.
func sensitiveAt(s tfplan.TFValue, i int) tfplan.TFValue {
	list, ok := s.AsList()
	if !ok {
		return s // TFBool(true) → entire collection sensitive
	}
	if i < 0 || i >= len(list) {
		return tfplan.TFValue{}
	}
	return list[i]
}

func hasSensitiveValue(v tfplan.TFValue) bool {
	if b, ok := v.AsBool(); ok {
		return b
	}
	if obj, ok := v.AsObject(); ok {
		for _, child := range obj {
			if hasSensitiveValue(child) {
				return true
			}
		}
	}
	if list, ok := v.AsList(); ok {
		for _, child := range list {
			if hasSensitiveValue(child) {
				return true
			}
		}
	}
	return false
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
