// Package config parses the `configuration` block of `terraform show -json`
// output. This tree records, per resource attribute, whether the value is a
// literal (constant_value) or a reference (references: ["var.x", ...]) — the
// provenance information osmo needs to absorb drift through module calls.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Plan is the subset of `terraform show -json` we need for provenance.
type Plan struct {
	Configuration struct {
		RootModule Module `json:"root_module"`
	} `json:"configuration"`
}

// Module is one module's configuration: its resources and the module calls it
// makes. The root module and every nested module share this shape.
type Module struct {
	Resources   []Resource            `json:"resources"`
	ModuleCalls map[string]ModuleCall `json:"module_calls"`
	// Variables holds variable declarations (for default-value provenance).
	Variables map[string]Variable `json:"variables"`
	// Outputs is unused today but part of the schema.
}

// Resource is a resource/data block within a module.
type Resource struct {
	Address     string                `json:"address"` // relative to its module, e.g. "aws_instance.web"
	Mode        string                `json:"mode"`    // "managed" | "data"
	Type        string                `json:"type"`
	Name        string                `json:"name"`
	Expressions map[string]Expression `json:"expressions"`
	rawExprJSON json.RawMessage       // set in UnmarshalJSON; used by FindNestedExpression
}

// UnmarshalJSON decodes a Resource, capturing the raw expressions JSON so that
// nested block expressions (arrays of sub-expression objects) can be navigated
// later via FindNestedExpression — they decode to zero Expression otherwise.
func (r *Resource) UnmarshalJSON(b []byte) error {
	type Alias struct {
		Address     string                `json:"address"`
		Mode        string                `json:"mode"`
		Type        string                `json:"type"`
		Name        string                `json:"name"`
		Expressions map[string]Expression `json:"expressions"`
	}
	var a Alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	r.Address = a.Address
	r.Mode = a.Mode
	r.Type = a.Type
	r.Name = a.Name
	r.Expressions = a.Expressions
	// Also stash the raw expressions blob for nested lookups.
	var raw struct {
		Expressions json.RawMessage `json:"expressions"`
	}
	_ = json.Unmarshal(b, &raw) // best-effort; ignore error
	r.rawExprJSON = raw.Expressions
	return nil
}

// FindNestedExpression navigates the raw expressions JSON along a block-type
// path and returns the Expression for attr at the leaf. path is the sequence
// of block type names from the resource body to the containing block
// (e.g., ["ebs_block_device"] for ebs_block_device.volume_size).
//
// When a block type has multiple array elements (multiple block instances with
// different expressions), the element whose attr has references is preferred
// over one with constant_value — because this method is called specifically when
// a variable reference was detected in the live HCL.
func (r *Resource) FindNestedExpression(path []string, attr string) *Expression {
	if len(r.rawExprJSON) == 0 || len(path) == 0 {
		return nil
	}
	cur := r.rawExprJSON
	for i, blockType := range path {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(cur, &obj); err != nil {
			return nil
		}
		arrRaw, ok := obj[blockType]
		if !ok {
			return nil
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(arrRaw, &arr); err != nil {
			return nil
		}
		if len(arr) == 0 {
			return nil
		}
		if i == len(path)-1 {
			// Leaf step: scan all block instances for attr, prefer var ref.
			return findExprInBlockArray(arr, attr)
		}
		// Intermediate step: take the first element and keep descending.
		cur = arr[0]
	}
	return nil
}

// findExprInBlockArray scans an array of sub-expression objects for the first
// element whose attr is defined, preferring elements with References over
// constant_value.
func findExprInBlockArray(arr []json.RawMessage, attr string) *Expression {
	var fallback *Expression
	for _, elem := range arr {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(elem, &obj); err != nil {
			continue
		}
		exprRaw, ok := obj[attr]
		if !ok {
			continue
		}
		var expr Expression
		if err := json.Unmarshal(exprRaw, &expr); err != nil {
			continue
		}
		if len(expr.References) > 0 {
			return &expr // prefer var ref — caller wants this
		}
		if fallback == nil {
			tmp := expr
			fallback = &tmp
		}
	}
	return fallback
}

// ModuleCall is a `module "x" {}` block. Expressions are the arguments passed
// in (evaluated in the PARENT scope). Module is the called module's own config.
type ModuleCall struct {
	Source      string                `json:"source"`
	Expressions map[string]Expression `json:"expressions"`
	Module      Module                `json:"module"`
}

// Variable is a variable declaration; Default is its literal default if any.
type Variable struct {
	Default json.RawMessage `json:"default"`
}

// Expression captures one attribute's provenance. Exactly one of HasConstant /
// References is typically meaningful. Nested-block expressions (arrays/objects
// of sub-expressions) are not modeled; such attributes parse to a zero
// Expression and are treated as unresolvable.
type Expression struct {
	HasConstant   bool
	ConstantValue interface{}
	References    []string
}

// UnmarshalJSON tolerates the several shapes an expression can take. A scalar
// attribute is an object {"constant_value":...} and/or {"references":[...]}.
// Block attributes can be arrays or nested objects; those decode to a zero
// Expression instead of erroring.
func (e *Expression) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		// Array (repeated block) or other non-object shape: leave zero.
		return nil
	}
	var aux struct {
		ConstantValue json.RawMessage `json:"constant_value"`
		References    []string        `json:"references"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		// Unknown object shape (nested block expressions): leave zero.
		return nil
	}
	e.References = aux.References
	if aux.ConstantValue != nil {
		e.HasConstant = true
		if err := json.Unmarshal(aux.ConstantValue, &e.ConstantValue); err != nil {
			return fmt.Errorf("decode constant_value: %w", err)
		}
	}
	return nil
}

// Parse extracts the configuration tree from raw `terraform show -json` bytes.
func Parse(raw []byte) (*Module, error) {
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse configuration: %w", err)
	}
	return &p.Configuration.RootModule, nil
}

// FindResource returns the resource with the given relative address within m,
// or nil if absent.
func (m *Module) FindResource(addr string) *Resource {
	for i := range m.Resources {
		if m.Resources[i].Address == addr {
			return &m.Resources[i]
		}
	}
	return nil
}
