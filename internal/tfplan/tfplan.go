// Package tfplan runs a refresh-only Terraform plan and extracts drift.
package tfplan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Drift describes a single resource whose real-world state diverged from prior
// Terraform state. Before is what Terraform recorded; After is refreshed reality.
type Drift struct {
	Address string                 // e.g. "aws_instance.web"
	Type    string                 // e.g. "aws_instance"
	Name    string                 // e.g. "web"
	Before  map[string]interface{} // prior state attributes
	After   map[string]interface{} // refreshed real-world attributes
}

// planJSON is the subset of `terraform show -json` we consume.
type planJSON struct {
	ResourceDrift []struct {
		Address string `json:"address"`
		Type    string `json:"type"`
		Name    string `json:"name"`
		Change  struct {
			Before map[string]interface{} `json:"before"`
			After  map[string]interface{} `json:"after"`
		} `json:"change"`
	} `json:"resource_drift"`
}

// Detect runs a refresh-only plan in dir and returns detected drift.
// terraformBin lets callers override the binary (default "terraform").
func Detect(ctx context.Context, dir, terraformBin string) ([]Drift, error) {
	if terraformBin == "" {
		terraformBin = "terraform"
	}

	planFile, err := os.CreateTemp(dir, "drift-*.tfplan")
	if err != nil {
		return nil, fmt.Errorf("create temp plan file: %w", err)
	}
	planPath := planFile.Name()
	planFile.Close()
	defer os.Remove(planPath)

	// Refresh-only: detect drift without proposing config-driven changes.
	if err := run(ctx, dir, terraformBin,
		"plan", "-refresh-only", "-input=false", "-no-color",
		"-out="+filepath.Base(planPath)); err != nil {
		return nil, fmt.Errorf("terraform plan -refresh-only: %w", err)
	}

	out, err := output(ctx, dir, terraformBin,
		"show", "-json", filepath.Base(planPath))
	if err != nil {
		return nil, fmt.Errorf("terraform show -json: %w", err)
	}

	var pj planJSON
	if err := json.Unmarshal(out, &pj); err != nil {
		return nil, fmt.Errorf("parse plan json: %w", err)
	}

	drifts := make([]Drift, 0, len(pj.ResourceDrift))
	for _, rd := range pj.ResourceDrift {
		drifts = append(drifts, Drift{
			Address: rd.Address,
			Type:    rd.Type,
			Name:    rd.Name,
			Before:  rd.Change.Before,
			After:   rd.Change.After,
		})
	}
	return drifts, nil
}

func run(ctx context.Context, dir, bin string, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func output(ctx context.Context, dir, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}
