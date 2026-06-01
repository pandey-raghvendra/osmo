<img width="350" height="200" alt="image" src="https://github.com/user-attachments/assets/a9e8b372-7929-4a7e-97d9-c80eb699abb8" />

<p align="center">
  <a href="https://github.com/pandey-raghvendra/osmo/stargazers">
    <img src="https://img.shields.io/github/stars/pandey-raghvendra/osmo?style=social" alt="GitHub Stars"/>
  </a>
  <a href="https://github.com/pandey-raghvendra/osmo/releases">
    <img src="https://img.shields.io/github/v/release/pandey-raghvendra/osmo" alt="Latest Release"/>
  </a>
  <a href="https://github.com/pandey-raghvendra/osmo/actions/workflows/ci.yml">
    <img src="https://github.com/pandey-raghvendra/osmo/actions/workflows/ci.yml/badge.svg" alt="CI"/>
  </a>
  <a href="https://goreportcard.com/report/github.com/pandey-raghvendra/osmo">
    <img src="https://goreportcard.com/badge/github.com/pandey-raghvendra/osmo" alt="Go Report Card"/>
  </a>
  <img src="https://img.shields.io/badge/license-Apache%202.0-blue" alt="License"/>
</p>

# osmo

**Osmosis for your Terraform.** Reality seeps into your config until both sides equalize.

`osmo` detects Terraform drift and rewrites your `.tf` files so that **configuration follows real-world reality** — the *absorb* direction.

> If you find osmo useful, a ⭐ on GitHub helps others discover it.

---

## Why absorb, not revert

Production incidents get hotfixed directly in the cloud console. Those fixes can't wait for a `terraform apply` cycle. So the source of truth should follow reality, not overwrite it. `osmo` rewrites your `.tf` files to match what's actually running; you review the diff and verify with a normal `terraform plan`.

---

## Quick start

```sh
# Preview changes — writes nothing, prints unified diff
osmo -dir ./infra

# Apply changes to disk
osmo -dir ./infra -write

# Apply, then prove it: re-plan and roll back if drift remains
osmo -dir ./infra -write -verify
```

| Flag | Default | Meaning |
|---|---|---|
| `-dir` | `.` | Terraform working directory |
| `-terraform` | auto-detect | Terraform/OpenTofu binary path (auto-detects `tofu` then `terraform`; overridden by `OSMO_TF_BINARY` env) |
| `-write` | `false` | Write changes to disk (else dry-run diff only) |
| `-verify` | `false` | After writing, run a normal plan; roll back files if any absorbed resource still has a planned change (requires `-write`; not usable with `-plan-json`) |
| `-approve` | `false` | Interactively approve each file change before writing (requires `-write` and a TTY) |
| `-json` | `false` | Emit a single JSON object to stdout instead of human-readable output |
| `-target` | `` | Only absorb drift on this resource address (repeatable / comma-separated) |
| `-exclude` | `` | Skip drift on this resource address (repeatable / comma-separated; wins over `-target`) |
| `-plan-json` | `` | Path to pre-generated `terraform show -json` output (skips detection) |

## OpenTofu support

osmo is compatible with both Terraform and OpenTofu. Binary selection priority:

1. `OSMO_TF_BINARY` env var — always wins
2. `-terraform` CLI flag
3. `.osmo.json` `defaults.terraform` field
4. Auto-detect: `tofu` if found on `PATH`, otherwise `terraform`

```sh
# Explicit: use tofu
osmo -dir ./infra -terraform tofu

# Env var (useful in CI)
OSMO_TF_BINARY=tofu osmo -dir ./infra

# Auto-detect: no flags needed if tofu is on PATH
osmo -dir ./infra
```

All flags, `-verify`, `-json`, and Terraform Cloud support work identically with OpenTofu.

---

## Exit codes

osmo follows the Terraform `detailed-exitcode` convention:

| Code | Meaning |
|---|---|
| `0` | No drift detected (or `-target`/`-exclude` matched nothing) |
| `1` | Execution error |
| `2` | Drift found — changes proposed, written, or unresolved drift reported |

Use exit code `2` as the CI gate: if osmo exits 2, the diff needs review.

## JSON output for CI / PR bots

`-json` emits a single JSON object to stdout, all human text suppressed:

```sh
osmo -dir ./infra -json | jq .result
osmo -dir ./infra -write -json > osmo.json
```

```json
{
  "osmo_version": "0.1.4",
  "result": "proposed",
  "drift_count": 2,
  "changes": [
    {
      "path": "infra/main.tf",
      "edits": [{ "address": "aws_instance.web", "attrs": ["instance_type"] }],
      "diff": "--- a/infra/main.tf\n+++ b/infra/main.tf\n..."
    }
  ],
  "unresolved": []
}
```

`result` values: `no_drift` · `no_match` · `proposed` · `absorbed` · `nothing_absorbable` · `verify_failed` · `error`

### GitHub Actions example

Full workflow — detects drift on a schedule, absorbs it, opens a PR, and posts a summary comment:

```yaml
# .github/workflows/drift.yml
name: Terraform drift absorption

on:
  schedule:
    - cron: "0 6 * * *"   # daily at 06:00 UTC
  workflow_dispatch:       # manual trigger

permissions:
  contents: write
  pull-requests: write

jobs:
  absorb:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: hashicorp/setup-terraform@v3
        with:
          terraform_version: "~1"
          cli_config_credentials_token: ${{ secrets.TF_API_TOKEN }}

      - name: Install osmo
        run: |
          brew install pandey-raghvendra/tap/osmo
          # or: go install github.com/pandey-raghvendra/osmo/cmd/osmo@latest

      - name: Terraform init
        run: terraform init
        working-directory: ./infra

      - name: Absorb drift
        id: osmo
        run: |
          osmo -dir ./infra -write -json > osmo.json
          echo "exit_code=$?" >> $GITHUB_OUTPUT
        env:
          AWS_ACCESS_KEY_ID: ${{ secrets.AWS_ACCESS_KEY_ID }}
          AWS_SECRET_ACCESS_KEY: ${{ secrets.AWS_SECRET_ACCESS_KEY }}

      - name: Open PR for absorbed drift
        if: steps.osmo.outputs.exit_code == '2'
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          BRANCH="osmo/absorb-$(date +%Y%m%d-%H%M)"
          git checkout -b "$BRANCH"
          git config user.name  "osmo-bot"
          git config user.email "osmo-bot@users.noreply.github.com"
          git add -A
          git commit -m "chore: absorb Terraform drift $(date +%Y-%m-%d)"
          git push origin "$BRANCH"

          DRIFT_COUNT=$(jq '.drift_count' osmo.json)
          SUMMARY=$(jq -r '
            "## Drift absorbed\n\n" +
            "**Resources drifted:** \(.drift_count)\n\n" +
            (if (.unresolved | length) > 0
             then "**Unresolved (\(.unresolved | length)):** " +
                  ([.unresolved[] | "`\(.address).\(.attr)`: \(.reason)"] | join("\n")) + "\n\n"
             else "" end) +
            "### Diffs\n\n" +
            ([.changes[] | "**`\(.path)`**\n```hcl\n\(.diff)\n```"] | join("\n\n"))
          ' osmo.json)

          gh pr create \
            --title "chore: absorb Terraform drift ($DRIFT_COUNT resource(s))" \
            --body "$SUMMARY" \
            --label "terraform,drift"

      - name: Post drift summary as step summary
        if: always()
        run: |
          if [ -f osmo.json ]; then
            echo "## osmo drift report" >> $GITHUB_STEP_SUMMARY
            echo '```json' >> $GITHUB_STEP_SUMMARY
            jq '{result, drift_count, unresolved_count: (.unresolved | length)}' osmo.json >> $GITHUB_STEP_SUMMARY
            echo '```' >> $GITHUB_STEP_SUMMARY
          fi
```

**Exit codes in CI:**

| Code | Meaning | Action |
|---|---|---|
| `0` | No drift | Pass |
| `1` | Error | Fail the job |
| `2` | Drift found (changes proposed or written) | Open PR / notify |

---

## Verify: closed-loop convergence

osmo rewrites your source of truth, so it should prove the rewrite actually
resolved the drift. `-verify` runs a normal `terraform plan` after writing —
absorb edits config to match reality, so a converged resource has **no planned
change**:

- no absorbed resource has a planned change → success
- a planned change **remains** on any absorbed resource → all written files are
  **rolled back** to their pre-absorb content and osmo exits non-zero

```sh
osmo -dir ./infra -write -verify
```

This guards against a wrong provenance trace or block match silently
corrupting config — osmo never leaves you with edits it can't prove converge.
`-verify` needs a live plan, so it is incompatible with `-plan-json`.

---

## Selective absorption: triage before you codify

Drift is not always a legitimate hotfix — it can be an unauthorized or
malicious change. Absorbing it blindly would launder that change into your
source of truth. Use selection + approval to keep a human in the loop:

```sh
# Only absorb a known-good resource
osmo -dir ./infra -write -target aws_instance.web

# Absorb everything except a suspicious change you want to investigate/revert
osmo -dir ./infra -write -exclude aws_security_group.public

# Review and approve each file change interactively
osmo -dir ./infra -write -approve
```

`-target`/`-exclude` match modules and indexed instances by prefix:
`module.app` matches `module.app.aws_instance.web`, and `aws_instance.web`
matches `aws_instance.web[0]`. `-exclude` always wins over `-target`.

---

## Project config: `.osmo.json`

Place `.osmo.json` in the Terraform working directory to set per-project
defaults (so you don't repeat flags every run) and to extend or override
built-in block identity keys.

```json
{
  "defaults": {
    "dir":       "./infra",
    "terraform": "/usr/local/bin/terraform",
    "targets":   ["module.app"],
    "excludes":  ["aws_instance.bastion"],
    "write":     false,
    "verify":    false,
    "json":      false
  },
  "block_identity": {
    "google_compute_firewall.allow": ["protocol"],
    "azurerm_lb.backend_address_pool": ["name"],
    "my_custom_resource.my_block": ["id", "name"]
  }
}
```

CLI flags always win over `defaults`. Unset flags inherit from `defaults`.
`block_identity` map key format: `"<resource_type>.<block_type>"`. User entries override built-ins.

**Built-in registry** (no config needed):

| Resource | Block | Identity key |
|---|---|---|
| `azurerm_application_gateway` | all named sub-blocks | `name` |
| `azurerm_lb` | `frontend_ip_configuration` | `name` |
| `google_compute_firewall` | `allow`, `deny` | `protocol` |
| `google_compute_backend_service` | `backend` | `group` |
| `google_container_cluster` | `node_pool` | `name` |

---

## How it works

```
terraform plan -refresh-only -out=plan
terraform show -json plan
  └─ resource_drift[]: before / after / after_sensitive
        │
        ├─ scalar attrs  ──► provenance trace through configuration tree
        │                         resolves to the single literal: resource attr,
        │                         module call arg, or variable default
        │
        ├─ nested blocks ──► HCL AST navigation (any depth)
        │                         literal attr → SetAttributeValue
        │                         var.x attr   → provenance trace (same chain)
        │                         add/remove   → AppendBlock / RemoveBlock
        │
        └─ dynamic blocks ──► extract for_each var from HCL tokens
                                   trace var chain → update collection literal

osmo emits unified diff  (or writes files with -write)
you run terraform plan   → 0 diff = drift fully resolved
```

---

## What it absorbs

| Drift pattern | Example | Status |
|---|---|---|
| Scalar attr — literal | `instance_type = "t3.micro"` → `"t3.large"` | ✅ |
| Scalar attr — `var.x` in module | root passes `size = 20`; resource uses `var.size` | ✅ |
| Scalar attr — deep module chain | `module.a → module.b → resource` | ✅ |
| Scalar attr — `for_each` map-of-objects (`each.value.X`) | one map entry's attr updated, siblings intact | ✅ |
| Scalar attr — `for_each` map-of-scalars (`each.value`) | scalar entry updated, siblings intact | ✅ |
| Nested block attr — literal (any depth) | `ebs_block_device { volume_size = 20 }` | ✅ |
| Nested block attr — `var.x` ref | `root_block_device { volume_size = var.size }` | ✅ |
| Nested block add | new `ingress {}` block added out-of-band | ✅ |
| Nested block remove | `ingress {}` block removed out-of-band | ✅ |
| Multi-instance block matching | two `ebs_block_device` blocks; correct one updated | ✅ |
| Deep nesting (3+ levels) | `server_side_encryption_configuration > rule > ...` | ✅ |
| `dynamic` block — `for_each = var.x` | collection variable updated to full after-state | ✅ |
| Sensitive attr (`after_sensitive = true`) | skipped, reported — never written to plain text | ✅ |
| Scalar attr removed from reality (null or absent in after) | literal removed from resource block | ✅ |
| Nested block attr removed from reality | literal removed from nested block body | ✅ |

### Safely reported, never silently wrong

Each unresolvable drift prints `! <address>.<attr>: <reason>`. Nothing is guessed.

| Pattern | Reason reported |
|---|---|
| `local.*` | locals not present in plan JSON |
| `each.value.X` / `each.value` (scalar) | single-instance for_each map patched automatically — see above |
| `each.key` / `count.*` | meta-argument derived from instance identity; cannot drift independently |
| Composed expression (`"${var.a}-${var.b}"`) | multiple references |
| Remote module constant | cannot edit registry/git source in place |
| Shared constant across `for_each` instances | cannot isolate one instance |
| `dynamic` block with `for_each = local.x` | local not traceable |
| `dynamic` block with map `for_each` | map reconstruction not supported |
| Sensitive attr | never writes secrets to plain-text config |
| Null after-value | removal from config not auto-applied |

---

## Module-aware provenance

osmo is provenance-driven, not a naive HCL grep. Each drifted attribute is traced through the plan's `configuration` tree to the **single literal** that controls it:

```
module.app.aws_instance.web   root_block_device.volume_size drifted

configuration tree:
  module_calls.app.expressions.vol_size  = { constant_value: 20 }   ← edit here
  module.app.resources[aws_instance.web]
    .expressions.root_block_device[0]
      .volume_size = { references: ["var.vol_size"] }               ← traced through
```

The edit lands at the **root call argument** — correct blast radius. The shared module source is never touched.

---

## Dynamic blocks

`dynamic` blocks are supported when `for_each` is a direct `var.x` reference:

```hcl
# modules/sg/main.tf
variable "rules" {}

resource "aws_security_group" "sg" {
  dynamic "ingress" {
    for_each = var.rules          # ← osmo traces this
    content {
      from_port = ingress.value.from_port
      to_port   = ingress.value.to_port
      protocol  = ingress.value.protocol
    }
  }
}
```

```hcl
# main.tf (root)
module "sg" {
  source = "./modules/sg"
  rules  = [                      # ← osmo rewrites this
    { from_port = 80,  to_port = 80,  protocol = "tcp" },
    { from_port = 443, to_port = 443, protocol = "tcp" },   # absorbed
  ]
}
```

Drift that added the 443 rule out-of-band causes osmo to update the `rules` list in the root module call — no literal `ingress {}` block is injected.

---

## Terraform Cloud / Remote execution

### Auto-detection (recommended)

osmo detects the TFC backend from `.terraform/terraform.tfstate` automatically. For drift detection, run `terraform init` first, then use `-plan-json` with a plan downloaded from TFC. For `-verify`, osmo creates a speculative plan via the TFC API — no extra flags needed:

```sh
export TFE_TOKEN=<your-team-token>   # standard Terraform env var

# Drift detection: pass a pre-generated plan JSON
osmo -dir ./infra -plan-json plan.json

# -verify: osmo auto-detects TFC and creates a speculative plan via API
osmo -dir ./infra -plan-json plan.json -write -verify
```

**Tag-based workspace selection** (`cloud { workspaces { tags = [...] } }`): run `terraform workspace select <name>` first so osmo knows which workspace to use for verify.

### Getting the plan JSON from TFC

```sh
# Option A: TFC web UI
# Plans → <run> → Download JSON → save as plan.json

# Option B: TFC API
TFC_ORG=my-org
TFC_WORKSPACE=my-workspace
WS_ID=$(curl -sS -H "Authorization: Bearer $TFE_TOKEN" \
  "https://app.terraform.io/api/v2/organizations/$TFC_ORG/workspaces/$TFC_WORKSPACE" \
  | jq -r '.data.id')
RUN_ID=$(curl -sS -H "Authorization: Bearer $TFE_TOKEN" \
  -H "Content-Type: application/vnd.api+json" \
  -d "{\"data\":{\"attributes\":{\"refresh-only\":true,\"plan-only\":true},\"type\":\"runs\",\"relationships\":{\"workspace\":{\"data\":{\"type\":\"workspaces\",\"id\":\"$WS_ID\"}}}}}" \
  https://app.terraform.io/api/v2/runs | jq -r '.data.id')
# poll until status = planned_and_finished, then:
PLAN_ID=$(curl -sS -H "Authorization: Bearer $TFE_TOKEN" \
  "https://app.terraform.io/api/v2/runs/$RUN_ID" | jq -r '.data.relationships.plan.data.id')
curl -sS -H "Authorization: Bearer $TFE_TOKEN" \
  "https://app.terraform.io/api/v2/plans/$PLAN_ID/json-output" > plan.json
```

**Local execution mode:** if you use TFC only for remote state (not remote execution), switch the workspace execution mode to **Local** — then `osmo -dir ./infra` works with no extra steps.

---

## Provider support

osmo is **provider-agnostic**: it reads `terraform show -json` (or `tofu show -json`) output and edits HCL. Any provider whose resources appear in `resource_drift[]` is supported. Tested against:

- AWS (`aws_*`)
- Azure (`azurerm_*`)
- Custom local modules

Works with both Terraform and OpenTofu — see [OpenTofu support](#opentofu-support).

---

## Requirements

- Go 1.21+ (to build from source)
- Terraform ≥ 1.0 **or** OpenTofu ≥ 1.6 (for `show -json` with `resource_drift`)

---

## Install

### Homebrew (macOS / Linux)
```sh
brew install pandey-raghvendra/osmo/osmo
```

### Go install
```sh
go install github.com/pandey-raghvendra/osmo/cmd/osmo@latest
```

### Build from source
```sh
git clone https://github.com/pandey-raghvendra/osmo
cd osmo
go build -o osmo ./cmd/osmo
```

### GitHub Actions

```yaml
- uses: pandey-raghvendra/osmo@v1
  with:
    dir: ./infra
    write: 'true'
    json: 'true'
  # outputs: result, drift_count, exit_code, json_path
```

All flags are available as inputs. `OSMO_TF_BINARY` is set automatically when you pass `terraform_binary: tofu`. See [`action.yml`](action.yml) for the full input/output reference.

---

## Known limitations

| Gap | Notes |
|---|---|
| `local.*` provenance | locals absent from plan JSON — fundamental Terraform limit, cannot be fixed |
| Composed expressions | `"${var.a}-${var.b}"` has multiple references — reported as unresolved |
| `dynamic` + map `for_each` | map reconstruction from expanded blocks not supported |
| Out-of-band *created* resources | resources that exist in Azure/AWS but not in config are invisible to `resource_drift` — use `terraform import` then osmo |
| Module arg / var default deletion | when a removed attr traces through a module arg or variable default, osmo reports it unresolved; only resource-block literals are auto-removed |
| Terraform Cloud remote execution | `-verify` uses the TFC API (speculative plan); `-plan-json` mode requires downloading the plan JSON from TFC first |
| Windows line endings | `hclwrite` outputs LF; files with CRLF line endings may show larger diffs |

---

## Troubleshooting

**Enable debug output**

```sh
OSMO_DEBUG=1 osmo -dir ./infra
# or: osmo -dir ./infra -debug
```

Debug output shows: how many resources drifted, which file each attribute resolved to, every Unresolved with its reason, and what the verify plan returned. Always include this output when filing a bug report.

---

**`terraform plan -refresh-only` fails inside osmo**

osmo runs plan in a subprocess; all the usual causes apply — missing credentials, wrong workspace, version mismatch. Run `terraform plan -refresh-only` yourself first to isolate the issue.

**`resource not found in configuration`**

The resource is in state but osmo can't find its `configuration` block in the plan JSON. Common cause: the resource is inside a module sourced from a registry/Git URL rather than a local path — osmo cannot edit remote sources.

**`traces to root var.X which has no literal default`**

The drifted attribute traces to a variable with no `default` value (it's required input). Edit the module call's argument or the variable default manually.

**`ambiguous nested block match`**

Two or more blocks inside the resource have the same score — osmo can't identify which one changed without risking a wrong edit. Add a `block_identity` entry to `.osmo.json` to tell osmo which attribute uniquely identifies blocks of that type.

**`-verify` rolls everything back unexpectedly**

`-verify` runs a full `terraform plan` after writing and checks whether the absorbed resources still have planned changes. If your config has unrelated pending changes (i.e. things terraform would change anyway), those planned changes will trigger rollback. Use `-target` to scope both osmo and verify to the drifted resource only.

**`-approve requires an interactive TTY`**

You ran osmo with `-approve` in CI or a non-interactive shell. Use `-target`/`-exclude` for CI-safe scoping instead.

**Diff looks right but `terraform plan` still shows changes**

Most likely the drifted attribute was traced to a variable/module arg that controls multiple resources — osmo updates the literal, which may trigger changes on siblings. Review the diff carefully and use `-target` to absorb one resource at a time.

---

## License

[Apache License 2.0](LICENSE) — permissive, with explicit patent grant; suitable for use inside corporate infrastructure pipelines.
