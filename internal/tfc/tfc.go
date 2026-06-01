// Package tfc implements Terraform Cloud / Enterprise support for osmo.
//
// When a workspace uses remote execution (the "remote" or "cloud" backend),
// osmo cannot run terraform plan locally for -verify. This package detects
// the TFC backend and drives a speculative plan via the TFC API instead.
package tfc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// httpClient is the shared HTTP client for all TFC API calls. The 30 s timeout
// bounds individual request latency; context cancellation (Ctrl-C) still
// terminates the call immediately if it fires first.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// Backend holds TFC/TFE connection details extracted from the initialized
// Terraform backend configuration.
type Backend struct {
	Host         string // e.g. "app.terraform.io"
	Organization string
	Workspace    string
	Token        string // from TFE_TOKEN env var
}

// tfState is the subset of .terraform/terraform.tfstate we need.
type tfState struct {
	Backend struct {
		Type   string `json:"type"`
		Config struct {
			Hostname     string `json:"hostname"`
			Organization string `json:"organization"`
			Workspaces   struct {
				Name string `json:"name"`
			} `json:"workspaces"`
		} `json:"config"`
	} `json:"backend"`
}

// DetectBackend reads .terraform/terraform.tfstate from dir and returns TFC
// connection details if the backend type is "remote" or "cloud".
// Returns (nil, nil) when the backend is local or not initialized.
func DetectBackend(dir string) (*Backend, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ".terraform", "terraform.tfstate"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // not initialized or local backend
		}
		return nil, fmt.Errorf("read .terraform/terraform.tfstate: %w", err)
	}

	var ts tfState
	if err := json.Unmarshal(raw, &ts); err != nil {
		return nil, fmt.Errorf("parse .terraform/terraform.tfstate: %w", err)
	}

	bt := ts.Backend.Type
	if bt != "remote" && bt != "cloud" {
		return nil, nil
	}

	host := ts.Backend.Config.Hostname
	if host == "" {
		host = "app.terraform.io"
	}
	org := ts.Backend.Config.Organization
	if org == "" {
		return nil, fmt.Errorf("TFC backend detected but organization not set in .terraform/terraform.tfstate")
	}

	// Workspace name resolution order:
	//  1. .terraform/environment (set by `terraform workspace select`)
	//  2. workspaces.name from backend config (explicit name)
	//  3. tag-based selection: .terraform/environment must be set — if not, error
	ws := ""
	if env, err := os.ReadFile(filepath.Join(dir, ".terraform", "environment")); err == nil {
		if name := strings.TrimSpace(string(env)); name != "" && name != "default" {
			ws = name
		}
	}
	if ws == "" {
		ws = ts.Backend.Config.Workspaces.Name
	}
	if ws == "" {
		return nil, fmt.Errorf("TFC backend detected but workspace name cannot be determined — " +
			"for tag-based workspace selection run `terraform workspace select <name>` first, " +
			"or set workspaces.name in your cloud {} block")
	}

	token := os.Getenv("TFE_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("TFC backend detected but TFE_TOKEN env var is not set")
	}

	return &Backend{
		Host:         host,
		Organization: org,
		Workspace:    ws,
		Token:        token,
	}, nil
}

// PlannedChanges creates a speculative (plan-only) run in TFC, waits for the
// plan to complete, and returns the list of resource addresses Terraform would
// still act on — equivalent to tfplan.PlannedChanges but executed remotely.
func (b *Backend) PlannedChanges(ctx context.Context, dir string) ([]string, error) {
	wsID, err := b.workspaceID(ctx)
	if err != nil {
		return nil, err
	}

	cvID, uploadURL, err := b.createConfigVersion(ctx, wsID)
	if err != nil {
		return nil, err
	}

	tarball, err := buildTarball(dir)
	if err != nil {
		return nil, fmt.Errorf("build workspace tarball: %w", err)
	}

	if err := b.uploadTarball(ctx, uploadURL, tarball); err != nil {
		return nil, err
	}

	runID, err := b.createRun(ctx, wsID, cvID)
	if err != nil {
		return nil, err
	}

	planID, err := b.waitForPlan(ctx, runID)
	if err != nil {
		return nil, err
	}

	planJSON, err := b.fetchPlanJSON(ctx, planID)
	if err != nil {
		return nil, err
	}

	return parsePlanJSON(planJSON)
}

// --- TFC API helpers ---------------------------------------------------------

func (b *Backend) baseURL() string {
	return "https://" + b.Host + "/api/v2"
}

func (b *Backend) do(ctx context.Context, method, path string, body []byte, contentType string) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.baseURL()+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	} else {
		req.Header.Set("Content-Type", "application/vnd.api+json")
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("TFC API %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("TFC API %s %s: HTTP %d: %s", method, path, resp.StatusCode, out)
	}
	return out, nil
}

func (b *Backend) workspaceID(ctx context.Context) (string, error) {
	raw, err := b.do(ctx, http.MethodGet,
		fmt.Sprintf("/organizations/%s/workspaces/%s", b.Organization, b.Workspace),
		nil, "")
	if err != nil {
		return "", fmt.Errorf("get workspace: %w", err)
	}
	var resp struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("parse workspace response: %w", err)
	}
	return resp.Data.ID, nil
}

func (b *Backend) createConfigVersion(ctx context.Context, wsID string) (cvID, uploadURL string, err error) {
	body, _ := json.Marshal(map[string]interface{}{
		"data": map[string]interface{}{
			"type": "configuration-versions",
			"attributes": map[string]interface{}{
				"auto-queue-runs": false,
				"speculative":     true,
			},
		},
	})
	raw, err := b.do(ctx, http.MethodPost,
		fmt.Sprintf("/workspaces/%s/configuration-versions", wsID),
		body, "")
	if err != nil {
		return "", "", fmt.Errorf("create config version: %w", err)
	}
	var resp struct {
		Data struct {
			ID    string `json:"id"`
			Links struct {
				Upload string `json:"upload"`
			} `json:"links"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", "", fmt.Errorf("parse config version response: %w", err)
	}
	return resp.Data.ID, resp.Data.Links.Upload, nil
}

func (b *Backend) uploadTarball(ctx context.Context, uploadURL string, tarball []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(tarball))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload tarball: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload tarball: HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}

func (b *Backend) createRun(ctx context.Context, wsID, cvID string) (string, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"data": map[string]interface{}{
			"type": "runs",
			"attributes": map[string]interface{}{
				"is-destroy": false,
				"plan-only":  true,
				"message":    "osmo verify (speculative plan)",
			},
			"relationships": map[string]interface{}{
				"workspace": map[string]interface{}{
					"data": map[string]interface{}{"type": "workspaces", "id": wsID},
				},
				"configuration-version": map[string]interface{}{
					"data": map[string]interface{}{"type": "configuration-versions", "id": cvID},
				},
			},
		},
	})
	raw, err := b.do(ctx, http.MethodPost, "/runs", body, "")
	if err != nil {
		return "", fmt.Errorf("create run: %w", err)
	}
	var resp struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("parse run response: %w", err)
	}
	return resp.Data.ID, nil
}

// waitForPlan polls the run until it reaches a terminal plan state and returns
// the plan ID.
func (b *Backend) waitForPlan(ctx context.Context, runID string) (string, error) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			raw, err := b.do(ctx, http.MethodGet, "/runs/"+runID, nil, "")
			if err != nil {
				return "", fmt.Errorf("poll run %s: %w", runID, err)
			}
			var resp struct {
				Data struct {
					Attributes struct {
						Status string `json:"status"`
					} `json:"attributes"`
					Relationships struct {
						Plan struct {
							Data struct {
								ID string `json:"id"`
							} `json:"data"`
						} `json:"plan"`
					} `json:"relationships"`
				} `json:"data"`
			}
			if err := json.Unmarshal(raw, &resp); err != nil {
				return "", fmt.Errorf("parse run status: %w", err)
			}
			switch resp.Data.Attributes.Status {
			case "planned", "planned_and_finished", "cost_estimated", "policy_checked":
				return resp.Data.Relationships.Plan.Data.ID, nil
			case "errored", "canceled", "force_canceled", "discarded":
				return "", fmt.Errorf("TFC run %s ended with status %q", runID, resp.Data.Attributes.Status)
			}
			// still running — keep polling
		}
	}
}

func (b *Backend) fetchPlanJSON(ctx context.Context, planID string) ([]byte, error) {
	return b.do(ctx, http.MethodGet, "/plans/"+planID+"/json-output", nil, "")
}

// --- plan JSON parsing -------------------------------------------------------

// planJSON is the minimum shape of a terraform show -json output we need.
type planJSON struct {
	ResourceChanges []struct {
		Address string `json:"address"`
		Change  struct {
			Actions []string `json:"actions"`
		} `json:"change"`
	} `json:"resource_changes"`
}

func parsePlanJSON(raw []byte) ([]string, error) {
	var pj planJSON
	if err := json.Unmarshal(raw, &pj); err != nil {
		return nil, fmt.Errorf("parse TFC plan JSON: %w", err)
	}
	var actionable []string
	for _, rc := range pj.ResourceChanges {
		if isActionable(rc.Change.Actions) {
			actionable = append(actionable, rc.Address)
		}
	}
	return actionable, nil
}

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

// --- tarball builder ---------------------------------------------------------

// buildTarball creates a gzip'd tar archive of all .tf and .tfvars files in dir.
// TFC expects a flat tarball (files at root, no top-level directory wrapper).
func buildTarball(dir string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	patterns := []string{"*.tf", "*.tfvars", "*.tfvars.json"}
	for _, pat := range patterns {
		matches, err := filepath.Glob(filepath.Join(dir, pat))
		if err != nil {
			return nil, err
		}
		for _, p := range matches {
			info, err := os.Stat(p)
			if err != nil {
				return nil, err
			}
			content, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			hdr := &tar.Header{
				Name:    filepath.Base(p),
				Mode:    0o644,
				Size:    info.Size(),
				ModTime: info.ModTime(),
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return nil, err
			}
			if _, err := tw.Write(content); err != nil {
				return nil, err
			}
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WorkspaceURL returns the web URL for the workspace (useful for log output).
func (b *Backend) WorkspaceURL() string {
	return fmt.Sprintf("https://%s/app/%s/workspaces/%s", b.Host, b.Organization, b.Workspace)
}


