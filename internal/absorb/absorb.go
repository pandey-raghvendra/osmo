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
	"reflect"
	"sort"
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
	Before    map[string]interface{}
	Drifted   map[string]interface{}
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
	Value     interface{}
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
	Added     []map[string]interface{}
	Removed   []map[string]interface{}
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
	Steps     []BlockStep              // path to the body containing the dynamic block
	BlockType string                   // the dynamic block's label, e.g. "ingress"
	AfterFull []map[string]interface{} // complete after-state of this block type
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
			if reflect.DeepEqual(bv, av) {
				continue
			}
			if _, isSlice := av.([]interface{}); isSlice {
				continue // handled by walkDriftMap below
			}
			// Null after-value: attr removed from reality → remove from config.
			if av == nil {
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
			t, u := provenance.Trace(root, addr, k, av)
			if u != nil {
				unresolved = append(unresolved, *u)
				continue
			}
			scalarOps = append(scalarOps, scalarOp{t, d.Address, k})
		}
		// Attrs present in before but absent from after: also removed from reality.
		for k, bv := range d.Before {
			if _, inAfter := d.After[k]; inAfter {
				continue
			}
			if reflect.DeepEqual(bv, nil) {
				continue
			}
			if _, isSlice := bv.([]interface{}); isSlice {
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

// changedAttrs returns keys whose value differs between before and after.
func changedAttrs(before, after map[string]interface{}) map[string]interface{} {
	changed := map[string]interface{}{}
	for k, av := range after {
		if bv, ok := before[k]; !ok || !reflect.DeepEqual(bv, av) {
			changed[k] = av
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			changed[k] = nil
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
		f, diags := hclwrite.ParseConfig(src, p, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return nil, fmt.Errorf("parse %s: %s", p, diags.Error())
		}
		de.files[p] = &tfFile{src: src, f: f}
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
	before, after map[string]interface{},
	afterSensitive interface{},
) (edits []BlockAttrEdit, removes []BlockAttrRemove, structs []BlockStructuralChange, dynUpdates []DynamicBlockUpdate, unresolved []provenance.Unresolved) {
	for k, av := range after {
		bv := before[k]
		if reflect.DeepEqual(bv, av) {
			continue
		}
		qualAttr := qualifiedAttr(path, k)
		afterSlice, isSlice := av.([]interface{})
		if !isSlice {
			if len(path) == 0 {
				// Root-level scalar: handled via provenance in Plan.
				continue
			}
			if av == nil {
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

		// Nested block (slice value).
		beforeSlice, _ := bv.([]interface{})
		bMaps := toMapSlice(beforeSlice)
		aMaps := toMapSlice(afterSlice)
		sensitiveElems := sensitiveSlice(afterSensitive, k)

		if hasSensitiveValue(sensitiveElems) {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: driftAddr, Attr: qualAttr + ".for_each",
				Reason: "sensitive value — cannot absorb dynamic block collection into plain-text config",
			})
		} else {
			// Always emit a DynamicBlockUpdate for the full after collection.
			// applyDynamicBlockUpdate is a no-op for literal (non-dynamic) blocks.
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

		// Match before↔after pairs by identity/similarity even when counts are
		// equal. Terraform providers commonly model nested blocks as sets; Azure
		// Application Gateway is a practical example where positional matching
		// turns delete/create drift into misleading scalar edits.
		matches, addedIdx, removedIdx, matchErr := matchBlockElements(idreg, addr.Type, blockPath(path, k), bMaps, aMaps)
		if matchErr != nil {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: driftAddr, Attr: qualAttr,
				Reason: matchErr.Error(),
			})
			continue
		}
		if len(addedIdx) > 0 || len(removedIdx) > 0 {
			var added, removed []map[string]interface{}
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
		beforeSlice, isSlice := bv.([]interface{})
		if !isSlice {
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
		bMaps := toMapSlice(beforeSlice)
		dynUpdates = append(dynUpdates, DynamicBlockUpdate{
			DriftAddr: driftAddr,
			ResType:   addr.Type,
			ResName:   addr.Name,
			ResMode:   addr.Mode,
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
func matchBlockElements(idreg *blockid.Registry, resourceType string, path []string, before, after []map[string]interface{}) (matches [][2]int, added, removed []int, err error) {
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
		// Accept the match only if at least one key matches.
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

// mapMatchScore counts keys with equal values between two maps (deep equal).
func mapMatchScore(a, b map[string]interface{}, identity []string) int {
	if len(identity) > 0 {
		return identityMatchScore(a, b, identity)
	}
	if hasDifferentNameIdentity(a, b) {
		return 0
	}
	score := 0
	for k, av := range a {
		if bv, ok := b[k]; ok && reflect.DeepEqual(av, bv) {
			score++
		}
	}
	return score
}

func identityMatchScore(a, b map[string]interface{}, keys []string) int {
	score := 0
	for _, k := range keys {
		av, aOK := a[k]
		bv, bOK := b[k]
		if !aOK || !bOK {
			continue
		}
		if !reflect.DeepEqual(av, bv) {
			return 0
		}
		score += 100
	}
	if score == 0 {
		return mapMatchScore(a, b, nil)
	}
	return score
}

func hasDifferentNameIdentity(a, b map[string]interface{}) bool {
	av, aOK := a["name"]
	bv, bOK := b["name"]
	return aOK && bOK && !reflect.DeepEqual(av, bv)
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

func toMapSlice(slice []interface{}) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(slice))
	for _, v := range slice {
		if m, ok := v.(map[string]interface{}); ok {
			result = append(result, m)
		}
	}
	return result
}

// applyBlockAttrEdit navigates to the nested block described by edit.Steps and
// rewrites the attribute. When the attribute is a variable reference rather than
// a literal, it calls provenance.TraceNested (using the configuration tree in
// root) and returns a redirect Target for Plan() to apply via the right dirEditor.
//
// Returns (path, qualAttr, redirect, unresolved):
//   - redirect non-nil: caller must apply the Target (may be in a different dir).
//   - u non-nil: attribute could not be absorbed.
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
				newVal, ctyErr := toCty(edit.Value)
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
					tgt, tu := provenance.TraceNested(root, addr, blockPath, edit.Attr, edit.Value)
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
		newVal, ctyErr := toCty(edit.Value)
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
			block, matchErr := findMatchingNestedBlock(parentBody, sc.BlockType, bm, map[string]interface{}{})
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
	before, drifted map[string]interface{},
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
		stable := make(map[string]interface{}, len(before))
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
func bodyMatchScore(body *hclwrite.Body, stableAttrs map[string]interface{}) int {
	score := 0
	for attr, expected := range stableAttrs {
		if body.GetAttribute(attr) == nil {
			continue
		}
		curCty, err := evalBodyAttr(body, attr)
		if err != nil {
			continue // var ref or expression — can't compare
		}
		expCty, err := toCty(expected)
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

// generateHCLBlock creates an hclwrite.Block from a JSON-decoded attribute map.
// Slice values are treated as nested blocks (recursed); all other non-nil
// values are emitted as literal attributes.
func generateHCLBlock(blockType string, attrs map[string]interface{}) *hclwrite.Block {
	b := hclwrite.NewBlock(blockType, nil)
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := attrs[k]
		if v == nil {
			continue
		}
		if slice, ok := v.([]interface{}); ok {
			for _, elem := range slice {
				if m, ok := elem.(map[string]interface{}); ok {
					b.Body().AppendBlock(generateHCLBlock(k, m))
				}
			}
			continue
		}
		if ctyVal, err := toCty(v); err == nil {
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

// mapsToInterfaceSlice converts []map[string]interface{} to []interface{}.
func mapsToInterfaceSlice(maps []map[string]interface{}) []interface{} {
	result := make([]interface{}, len(maps))
	for i, m := range maps {
		result[i] = m
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
		if s, ok := name.(string); ok && s != "" {
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
func isSensitiveAttr(afterSensitive interface{}, attr string) bool {
	if afterSensitive == nil {
		return false
	}
	// Whole resource marked sensitive.
	if b, ok := afterSensitive.(bool); ok {
		return b
	}
	m, ok := afterSensitive.(map[string]interface{})
	if !ok {
		return false
	}
	v := m[attr]
	b, ok := v.(bool)
	return ok && b // only true when the attr is explicitly marked true
}

func isSensitiveNested(afterSensitive interface{}, attr string) bool {
	if afterSensitive == nil {
		return false
	}
	if b, ok := afterSensitive.(bool); ok {
		return b
	}
	m, ok := afterSensitive.(map[string]interface{})
	if !ok {
		return false
	}
	return hasSensitiveValue(m[attr])
}

func sensitiveSlice(afterSensitive interface{}, attr string) []interface{} {
	if afterSensitive == nil {
		return nil
	}
	if b, ok := afterSensitive.(bool); ok && b {
		return []interface{}{true}
	}
	m, ok := afterSensitive.(map[string]interface{})
	if !ok {
		return nil
	}
	s, _ := m[attr].([]interface{})
	return s
}

func sensitiveAt(s []interface{}, i int) interface{} {
	if i < 0 || i >= len(s) {
		return nil
	}
	return s[i]
}

func hasSensitiveValue(v interface{}) bool {
	switch x := v.(type) {
	case bool:
		return x
	case map[string]interface{}:
		for _, child := range x {
			if hasSensitiveValue(child) {
				return true
			}
		}
	case []interface{}:
		for _, child := range x {
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
