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

	var unresolved []provenance.Unresolved

	type scalarOp struct {
		target    *provenance.Target
		driftAddr string
		attr      string
	}
	type nestedAttrOp struct {
		addr address.Addr
		edit BlockAttrEdit
	}
	type nestedStructOp struct {
		addr   address.Addr
		change BlockStructuralChange
	}

	var scalarOps []scalarOp
	var nestedAttrOps []nestedAttrOp
	var nestedStructOps []nestedStructOp

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
			t, u := provenance.Trace(root, addr, k, av)
			if u != nil {
				unresolved = append(unresolved, *u)
				continue
			}
			scalarOps = append(scalarOps, scalarOp{t, d.Address, k})
		}

		// Nested block attrs + structural changes → recursive walk.
		// walkDriftMap skips root-level scalars (handled above) and emits
		// BlockAttrEdits for nested literals and BlockStructuralChanges for
		// count-change (add/remove) events.
		blockEdits, blockStructs := walkDriftMap(addr, d.Address, nil, d.Before, d.After)
		for _, e := range blockEdits {
			nestedAttrOps = append(nestedAttrOps, nestedAttrOp{addr, e})
		}
		for _, s := range blockStructs {
			nestedStructOps = append(nestedStructOps, nestedStructOp{addr, s})
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

	// Nested attr ops: always edit in the resource's own source directory.
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
		path, qualAttr, u := de.applyBlockAttrEdit(o.edit)
		if u != nil {
			unresolved = append(unresolved, *u)
			continue
		}
		if path != "" {
			record(path, o.edit.DriftAddr, qualAttr)
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
//   - BlockStructuralChanges for nested block count changes at any depth
//
// Root-level scalar attrs (path == nil) are skipped because those go through
// the provenance path in Plan.
func walkDriftMap(
	addr address.Addr,
	driftAddr string,
	path []BlockStep,
	before, after map[string]interface{},
) (edits []BlockAttrEdit, structs []BlockStructuralChange) {
	for k, av := range after {
		bv := before[k]
		if reflect.DeepEqual(bv, av) {
			continue
		}
		afterSlice, isSlice := av.([]interface{})
		if !isSlice {
			if len(path) == 0 {
				// Root-level scalar: handled via provenance in Plan.
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

		if len(bMaps) == len(aMaps) {
			// Same count: blocks are the same logical instances with drifted
			// attrs. Match positionally — no structural change.
			for i := range aMaps {
				bm, am := bMaps[i], aMaps[i]
				drifted := changedAttrs(bm, am)
				if len(drifted) == 0 {
					continue
				}
				newStep := BlockStep{BlockType: k, Before: bm, Drifted: drifted}
				newPath := append(append([]BlockStep(nil), path...), newStep)
				subEdits, subStructs := walkDriftMap(addr, driftAddr, newPath, bm, am)
				edits = append(edits, subEdits...)
				structs = append(structs, subStructs...)
			}
		} else {
			// Different count: some blocks were added or removed out-of-band.
			// Use greedy scoring to match before↔after pairs, then report
			// structural changes and recurse into matched pairs for attr diffs.
			matches, addedIdx, removedIdx := matchBlockElements(bMaps, aMaps)
			if len(addedIdx) > 0 || len(removedIdx) > 0 {
				var added, removed []map[string]interface{}
				for _, i := range addedIdx {
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
				subEdits, subStructs := walkDriftMap(addr, driftAddr, newPath, bm, am)
				edits = append(edits, subEdits...)
				structs = append(structs, subStructs...)
			}
		}
	}
	return
}

// matchBlockElements greedily matches before elements to after elements by
// attribute similarity, returning matched index pairs, unmatched after indices
// (added), and unmatched before indices (removed).
func matchBlockElements(before, after []map[string]interface{}) (matches [][2]int, added, removed []int) {
	used := make([]bool, len(after))
	for bi, bm := range before {
		bestScore, bestAi := -1, -1
		for ai, am := range after {
			if used[ai] {
				continue
			}
			if s := mapMatchScore(bm, am); s > bestScore {
				bestScore, bestAi = s, ai
			}
		}
		// Accept the match only if at least one key matches.
		if bestAi >= 0 && bestScore > 0 {
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
func mapMatchScore(a, b map[string]interface{}) int {
	score := 0
	for k, av := range a {
		if bv, ok := b[k]; ok && reflect.DeepEqual(av, bv) {
			score++
		}
	}
	return score
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
// rewrites the attribute. Returns the file path edited, the qualified attr
// name, and an Unresolved if the attr is a var ref or absent.
func (de *dirEditor) applyBlockAttrEdit(edit BlockAttrEdit) (path, qualAttr string, u *provenance.Unresolved) {
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
			// Block path not found in this file; try the next.
			continue
		}
		if targetBody.GetAttribute(edit.Attr) == nil {
			// Computed / not in config: skip silently (same rule as top-level).
			return p, qualAttr, nil
		}
		if _, evalErr := evalBodyAttr(targetBody, edit.Attr); evalErr != nil {
			return p, qualAttr, &provenance.Unresolved{
				Address: edit.DriftAddr,
				Attr:    qualAttr,
				Reason:  "nested block attr is a variable reference; var-chain tracing inside nested blocks not yet supported",
			}
		}
		newVal, ctyErr := toCty(edit.Value)
		if ctyErr != nil {
			return p, qualAttr, &provenance.Unresolved{
				Address: edit.DriftAddr, Attr: qualAttr, Reason: ctyErr.Error(),
			}
		}
		targetBody.SetAttributeValue(edit.Attr, newVal)
		ff.dirty = true
		return p, qualAttr, nil
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
			unresolved = append(unresolved, provenance.Unresolved{
				Address: sc.DriftAddr, Attr: sc.BlockType,
				Reason: "could not navigate to parent block: " + err.Error(),
			})
			return p, nil, unresolved
		}

		// Remove blocks that disappeared out-of-band.
		for _, bm := range sc.Removed {
			block := findMatchingNestedBlock(parentBody, sc.BlockType, bm, map[string]interface{}{})
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
		block := findMatchingNestedBlock(cur, step.BlockType, step.Before, step.Drifted)
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
) *hclwrite.Block {
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
		stable := make(map[string]interface{}, len(before))
		for k, v := range before {
			if _, isDrifted := drifted[k]; !isDrifted {
				stable[k] = v
			}
		}
		best, bestScore := candidates[0], -1
		for _, c := range candidates {
			if s := bodyMatchScore(c.Body(), stable); s > bestScore {
				bestScore = s
				best = c
			}
		}
		return best
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

// qualifiedAttr returns "blockA.blockB.attr" notation for a nested attr.
func qualifiedAttr(steps []BlockStep, attr string) string {
	if len(steps) == 0 {
		return attr
	}
	parts := make([]string, 0, len(steps)+1)
	for _, s := range steps {
		parts = append(parts, s.BlockType)
	}
	parts = append(parts, attr)
	return strings.Join(parts, ".")
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
