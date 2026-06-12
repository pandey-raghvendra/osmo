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
	Address        string  // e.g. "aws_instance.web"
	Type           string  // e.g. "aws_instance"
	Name           string  // e.g. "web"
	Before         TFState // prior state attributes
	After          TFState // refreshed real-world attributes
	AfterSensitive TFValue // TFNull | TFBool | TFObject with per-attr sensitivity flags
}

// planJSON is the subset of `terraform show -json` we consume.
type planJSON struct {
	ResourceDrift []struct {
		Address string `json:"address"`
		Type    string `json:"type"`
		Name    string `json:"name"`
		Change  struct {
			Before         TFState `json:"before"`
			After          TFState `json:"after"`
			AfterSensitive TFValue `json:"after_sensitive"`
		} `json:"change"`
	} `json:"resource_drift"`
	ResourceChanges []struct {
		Address string `json:"address"`
		Change  struct {
			Actions []string `json:"actions"`
		} `json:"change"`
	} `json:"resource_changes"`
}

// Detect runs a refresh-only plan in dir and returns detected drift.
// terraformBin lets callers override the binary (default "terraform").
//
// It also returns the raw `terraform show -json` output, which carries the
// configuration tree used for provenance tracing.
func Detect(ctx context.Context, dir, terraformBin string) ([]Drift, []byte, error) {
	if terraformBin == "" {
		terraformBin = "terraform"
	}

	planFile, err := os.CreateTemp(dir, "drift-*.tfplan")
	if err != nil {
		return nil, nil, fmt.Errorf("create temp plan file: %w", err)
	}
	planPath := planFile.Name()
	planFile.Close()
	defer os.Remove(planPath)

	// Refresh-only: detect drift without proposing config-driven changes.
	if err := run(ctx, dir, terraformBin,
		"plan", "-refresh-only", "-input=false", "-no-color",
		"-out="+filepath.Base(planPath)); err != nil {
		return nil, nil, fmt.Errorf("terraform plan -refresh-only: %w", err)
	}

	raw, err := output(ctx, dir, terraformBin,
		"show", "-json", filepath.Base(planPath))
	if err != nil {
		return nil, nil, fmt.Errorf("terraform show -json: %w", err)
	}

	drifts, err := ParseDrift(raw)
	if err != nil {
		return nil, nil, err
	}
	return drifts, raw, nil
}

// ParseDrift extracts resource drift from raw `terraform show -json` output.
func ParseDrift(raw []byte) ([]Drift, error) {
	var pj planJSON
	if err := json.Unmarshal(raw, &pj); err != nil {
		return nil, fmt.Errorf("parse plan json: %w", err)
	}
	drifts := make([]Drift, 0, len(pj.ResourceDrift))
	for _, rd := range pj.ResourceDrift {
		drifts = append(drifts, Drift{
			Address:        rd.Address,
			Type:           rd.Type,
			Name:           rd.Name,
			Before:         rd.Change.Before,
			After:          rd.Change.After,
			AfterSensitive: rd.Change.AfterSensitive,
		})
	}
	return drifts, nil
}

// PlanJSON runs a normal (config-driven) terraform plan in dir and returns the
// raw terraform show -json output for use with osmo inspect.
func PlanJSON(ctx context.Context, dir, terraformBin string) ([]byte, error) {
	if terraformBin == "" {
		terraformBin = "terraform"
	}
	planFile, err := os.CreateTemp(dir, "inspect-*.tfplan")
	if err != nil {
		return nil, fmt.Errorf("create temp plan file: %w", err)
	}
	planPath := planFile.Name()
	planFile.Close()
	defer os.Remove(planPath)

	if err := run(ctx, dir, terraformBin,
		"plan", "-input=false", "-no-color",
		"-out="+filepath.Base(planPath)); err != nil {
		return nil, fmt.Errorf("terraform plan: %w", err)
	}
	raw, err := output(ctx, dir, terraformBin, "show", "-json", filepath.Base(planPath))
	if err != nil {
		return nil, fmt.Errorf("terraform show -json: %w", err)
	}
	return raw, nil
}

// PlannedChanges runs a normal (config-driven) plan in dir and returns the
// addresses Terraform would still act on, i.e. resources whose actions are not
// purely "no-op"/"read". After osmo absorbs drift into config, a converged
// resource has no actionable change here; a still-actionable address means the
// config does not yet match reality.
func PlannedChanges(ctx context.Context, dir, terraformBin string) ([]string, error) {
	if terraformBin == "" {
		terraformBin = "terraform"
	}

	planFile, err := os.CreateTemp(dir, "verify-*.tfplan")
	if err != nil {
		return nil, fmt.Errorf("create temp plan file: %w", err)
	}
	planPath := planFile.Name()
	planFile.Close()
	defer os.Remove(planPath)

	if err := run(ctx, dir, terraformBin,
		"plan", "-input=false", "-no-color",
		"-out="+filepath.Base(planPath)); err != nil {
		return nil, fmt.Errorf("terraform plan: %w", err)
	}

	raw, err := output(ctx, dir, terraformBin,
		"show", "-json", filepath.Base(planPath))
	if err != nil {
		return nil, fmt.Errorf("terraform show -json: %w", err)
	}

	var pj planJSON
	if err := json.Unmarshal(raw, &pj); err != nil {
		return nil, fmt.Errorf("parse plan json: %w", err)
	}
	var actionable []string
	for _, rc := range pj.ResourceChanges {
		if isActionable(rc.Change.Actions) {
			actionable = append(actionable, rc.Address)
		}
	}
	return actionable, nil
}

// isActionable reports whether a plan action set represents real work. Empty,
// ["no-op"], and ["read"] are not actionable; anything else is.
func isActionable(actions []string) bool {
	for _, a := range actions {
		switch a {
		case "no-op", "read":
		default:
			return true
		}
	}
	return false
}

// Fmt runs `terraform fmt -` on src and returns the formatted HCL.
// Returns (src, err) unchanged if terraform is unavailable or the content
// cannot be formatted — callers should warn and proceed with unformatted output.
func Fmt(ctx context.Context, bin string, src []byte) ([]byte, error) {
	if bin == "" {
		bin = "terraform"
	}
	cmd := exec.CommandContext(ctx, bin, "fmt", "-")
	cmd.Stdin = bytes.NewReader(src)
	// CHECKPOINT_DISABLE=1 prevents terraform from phoning home to
	// check.hashicorp.com on startup, which can hang in restricted CI envs.
	cmd.Env = append(os.Environ(), "CHECKPOINT_DISABLE=1")
	out, err := cmd.Output()
	if err != nil {
		return src, err
	}
	return out, nil
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
