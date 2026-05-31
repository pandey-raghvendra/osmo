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
