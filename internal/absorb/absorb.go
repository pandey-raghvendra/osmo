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

	// Trace every changed attribute to an edit target.
	type op struct {
		target    *provenance.Target
		driftAddr string
		attr      string
	}
	var ops []op
	for _, d := range drifts {
		addr, err := address.Parse(d.Address)
		if err != nil {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: d.Address, Attr: "*", Reason: "unparseable address: " + err.Error()})
			continue
		}
		for name, val := range changedAttrs(d.Before, d.After) {
			t, u := provenance.Trace(root, addr, name, val)
			if u != nil {
				unresolved = append(unresolved, *u)
				continue
			}
			ops = append(ops, op{target: t, driftAddr: d.Address, attr: name})
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

	for _, o := range ops {
		dir, err := resolveDir(root, baseDir, o.target.SourceModulePath)
		if err != nil {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: o.driftAddr, Attr: o.attr, Reason: err.Error()})
			continue
		}
		de := editors[dir]
		if de == nil {
			de, err = newDirEditor(dir)
			if err != nil {
				return nil, nil, err
			}
			editors[dir] = de
		}
		path, err := de.apply(o.target)
		if err != nil {
			unresolved = append(unresolved, provenance.Unresolved{
				Address: o.driftAddr, Attr: o.attr, Reason: err.Error()})
			continue
		}
		record(path, o.driftAddr, o.attr)
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

// changedAttrs returns top-level attributes whose value differs between before
// and after. Nested-block drift is out of scope for v1.
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
