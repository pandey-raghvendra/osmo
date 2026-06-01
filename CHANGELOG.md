# Changelog

All notable changes to osmo are documented here.

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [Unreleased]

---

## [1.4.0] - 2026-06-02

### Added
- **`osmo ui` subcommand**: interactive terminal TUI (Bubble Tea) for drift
  triage and selective absorb. No API key, fully offline.
  - Resource list with ✅ / ⚠️  / 🚩 verdict icons and action status
  - Before/after diff panel for selected resource with syntax colouring
  - Per-resource toggle: `[a]` absorb · `[s]` skip
  - Bulk shortcuts: `[A]` absorb all safe · `[S]` skip all
  - `[x]` executes osmo immediately with the right `-target`/`-exclude` flags
  - Diff panel scrolling (`pgup`/`pgdn`) for large nested changes
  - Reads from live detection, piped `-json`, or `-plan-json` file

---

## [1.3.0] - 2026-06-02

### Added
- **`osmo triage` subcommand**: classifies every drifted resource as SAFE /
  REVIEW / FLAG before anything is absorbed. Fully offline — no API key, no
  network calls, deterministic rule engine.
  - SAFE: tags, descriptions, scalar size/type changes
  - REVIEW: capacity/autoscaler attrs (`desired_capacity`, `replica_count`, …);
    suggests `lifecycle.ignore_changes` as alternative
  - FLAG: security-sensitive resource types (IAM, security groups, KMS, network
    ACLs, S3 public-access blocks, Azure NSG, GCP firewall rules, …) and
    security-sensitive attribute patterns (`cidr`, `policy`, `ingress`,
    `egress`, `kms_key`, `principal`, …)
  - Emits a ready-to-run `osmo … -write -target … -exclude …` command
    scoped to only the safe resources
  - `-json` flag for machine-readable output
  - Three invocation modes: live detection, piped from `osmo -json`, or
    pre-generated `-plan-json`
- **`.osmo.json` triage config**: `"triage"` section extends the built-in rule
  registry with per-project `flag_resources`, `flag_attrs`, `safe_attrs`.
  Follows the same extensibility pattern as `block_identity`.

---

## [1.2.0] - 2026-06-02

### Added
- **OpenTofu support**: osmo auto-detects `tofu` on `PATH` when no binary is
  explicitly configured; falls back to `terraform`. Priority chain:
  `OSMO_TF_BINARY` env → `-terraform` flag → `.osmo.json` default → auto-detect.
  All features (absorb, verify, JSON output, TFC) work identically with OpenTofu.
- **GitHub Action** (`action.yml`): `uses: pandey-raghvendra/osmo@v1` installs
  osmo and runs it with all flags as inputs. Outputs: `result`, `drift_count`,
  `exit_code`, `json_path`.
- **Community files**: `SECURITY.md` (private vulnerability reporting, threat
  model), `CONTRIBUTING.md` (dev setup, test patterns, commit style),
  `CODE_OF_CONDUCT.md`, GitHub issue templates (bug / feature request), and
  PR template.

### Fixed
- **TFC HTTP client timeout**: TFC API calls used `http.DefaultClient` (no
  timeout) — a stalled network connection would block indefinitely. Replaced
  with a 30 s timeout client; context cancellation (Ctrl-C) still fires first.
- **GitHub Action exit code**: `osmo ... -json > file || true` swallowed
  osmo's exit code, making CI always see exit 0. Fixed with `; EXIT_CODE=$?`.
- **Goreleaser changelog**: `chore:` commits no longer appear in release notes.

### Changed
- `-terraform` flag default updated to reflect auto-detection behaviour
  (`tofu` → `terraform`).
- README: OpenTofu section, updated Requirements, GitHub Action install snippet,
  corrected TFC known-limitation entry.

### Tests
- `internal/provenance`: 31 new direct unit tests (0 % → 76 % coverage) —
  `Trace`, `TraceForEach`, `singleVarRef`, `eachValueMapAttr`, `isCountKey`,
  `quoteKey`, `fullAddr`.
- `internal/absorb`: `renderTFValue` (all types + error paths),
  `hclEscapeString` (all special chars including backslash, newline, tab, CR),
  `isAmbiguousNestedMatch`.
- `internal/tfplan`: `ParseDrift` (7 cases), `TFValue` accessors, `MarshalJSON`
  (51 % → 64 % coverage).
- `cmd/osmo`: `applyConfigDefaults` (3 cases), `run` validation errors,
  JSON-mode drift-found path (17 % → 33 % coverage).

---

## [1.1.0] - 2026-06-01

### Added
- **`for_each` map-of-scalars (`each.value` direct)**: drift on resources using
  `for_each = {a = "t3.micro"}` + `instance_type = each.value` now absorbed
  automatically — single instance patched, siblings untouched.
- **`for_each` map-of-objects (`each.value.X`)**: drift on resources using
  `for_each = {a = {size = "t3.micro"}}` + `instance_type = each.value.size`
  now absorbed automatically.
- **`-debug` flag / `OSMO_DEBUG=1`**: prints debug trace to stderr — drift
  addresses, absorb decisions, Unresolved reasons, verify plan results. Never
  pollutes `-json` stdout.
- **Terraform Cloud `-verify` auto-detection**: when `.terraform/terraform.tfstate`
  shows a `remote` or `cloud` backend, `-verify` creates a speculative plan via
  the TFC API (`TFE_TOKEN`) instead of running `terraform plan` locally.
- **TFC tag-based workspace support**: `cloud {}` with `workspaces { tags = [...] }`
  resolves workspace name from `.terraform/environment` (set by
  `terraform workspace select`); clear error when name is not determinable.
- **GitHub Actions drift workflow**: full example in README — schedules daily,
  absorbs drift, opens a PR with diffs and unresolved summary.

### Fixed
- **`no_match` JSON `drift_count`**: was always `0` when `-target`/`-exclude`
  filtered all drifted resources; now reports the actual pre-filter count.
- **Windows CRLF line endings**: `.tf` files with `\r\n` endings now have their
  line endings preserved after absorb — `hclwrite` output normalised to LF
  before parse, restored to CRLF on emit.

---

## [1.0.0] - 2026-06-01

### Added
- **`terraform fmt` post-absorb**: each changed file piped through
  `terraform fmt -` in-memory before diffs are shown and before writing.
  Both diff output and written files are fmt-clean. Non-fatal on failure.
- **Stars/release/CI/Go Report Card badges** in README.
- CI now runs `go test ./...` (all packages) with Terraform available in
  the runner. Previously only `internal/absorb` and `internal/address` ran.
- Release workflow runs full test suite before GoReleaser.

### Changed
- Version bumped to 1.0.0: core absorption engine, verify+rollback,
  selective absorption, JSON output, extensible block identity registry,
  deletion handling, and project config file are all stable.

---

## [0.1.6] - 2026-06-01

### Added
- **`terraform fmt` post-absorb**: each changed file's HCL is piped through
  `terraform fmt -` in-memory before diffs are shown and before writing to
  disk. Both the diff output and the written files are fmt-clean. Failures are
  non-fatal — osmo warns and proceeds with unformatted content.
- **`.osmo.json` defaults section**: per-project defaults for `dir`,
  `terraform`, `targets`, `excludes`, `write`, `verify`, `json`. CLI flags
  always take precedence. Removes the need to repeat flags on every invocation.
- **`[dry run]` labeling**: output explicitly says `[dry run]` when `-write` is
  not set, removing ambiguity between proposed and applied changes.
- **Troubleshooting section** and expanded known-limitations table in README.
- **CHANGELOG.md** (this file).

---

## [0.1.5] - 2026-06-01

### Added
- **Extensible block identity registry** (`internal/blockid`): built-in
  identity keys for Azure AppGW, Azure LB, GCP Compute Firewall, GCP Backend
  Service, and GCP Container Cluster. Previously only Azure AppGW was covered
  and the logic was hardcoded.
- **`.osmo.json` config file** — `block_identity` section lets projects extend
  or override built-in identity keys without upgrading osmo. User entries win
  over built-ins.

### Changed
- `identityKeys()` switch removed from `absorb.go`; replaced by
  `blockid.Registry` threaded through `walkDriftMap` / `matchBlockElements`.

---

## [0.1.4] - 2026-06-01

### Added
- **Structured exit codes**: `0` = no drift, `1` = error, `2` = drift found
  (proposed, absorbed, or unresolved). Mirrors `terraform -detailed-exitcode`.
- **`-json` flag**: emits a single JSON object to stdout with `osmo_version`,
  `result`, `drift_count`, `changes[]` (path / edits / diff), and
  `unresolved[]`. All human text suppressed from stdout in JSON mode; stderr
  status lines remain for CI logs.
- **TTY guard on `-approve`**: non-interactive shells get `exit 1` with a clear
  message instead of hanging.
- GitHub Actions example in README.

### Fixed
- `-approve` could hang indefinitely when stdin was not a terminal (CI).

---

## [0.1.3] - 2026-06-01

### Added
- **`-verify` closed-loop convergence**: after `-write`, runs a normal
  `terraform plan`; if any absorbed resource still has a planned change, all
  written files are rolled back and osmo exits non-zero.
- **Selective absorption**: `-target` / `-exclude` filter drift by resource
  address (prefix-matching modules and indexed instances). `-exclude` wins over
  `-target`.
- **`-approve`**: interactive per-file approval before writing.

### Fixed
- Verify uses a config-driven plan (not refresh-only); refresh-only compares
  state↔reality and ignores config, so it would always report drift after an
  absorb.

---

## [0.1.2] - 2026-06-01

### Added
- **`BlockAttrRemove`**: attrs absent from refreshed reality are now removed
  from config (nested blocks). Previously reported as Unresolved.
- **Sensitive value guards** in `walkDriftMap`: nested sensitive attrs and
  sensitive dynamic-block collections emit Unresolved instead of being written
  to plain-text config.
- **Identity-based nested block matching** for `azurerm_application_gateway`:
  all named sub-block types matched by `name` key. Fixes false scalar edits
  caused by set-reordering in the Azure provider.
- **Ambiguity detection**: `matchBlockElements` and `findMatchingNestedBlock`
  now return an error on tie, surfaced as Unresolved, instead of silently
  picking a wrong block.
- **`canAddMissingNestedAttr`** whitelist: `backend_http_settings.probe_name`
  and `url_path_map`/`path_rule` reference attrs can be added when absent from
  config.
- **`qualifiedAttr`** now emits `block["name"]` labels for clearer attribution.
- `changedAttrs` reports removed keys (nil value) so before-only attrs route to
  `BlockAttrRemove`.
- Optional Azure Application Gateway e2e test (skipped without fixture).

### Changed
- `walkDriftMap` signature extended: takes `afterSensitive`, returns `removes`
  and `unresolved` in addition to existing return values.
- `matchBlockElements` always identity-matches (even when counts are equal)
  instead of only when counts differ.

---

## [0.1.1] - 2026-05-28

### Added
- **Deletion handling**: root-level scalar attrs with null or absent after-value
  are removed from the resource block in config. Previously reported as
  Unresolved.
- Nested scalar attrs with explicit null after-value (`av == nil`) now route to
  `BlockAttrRemove` instead of Unresolved (unified with the absent-key path).
- `-plan-json` flag: accepts pre-generated `terraform show -json` output,
  enabling use with Terraform Cloud remote execution.

---

## [0.1.0] - 2026-05-20

### Added
- Initial release.
- Drift detection via `terraform plan -refresh-only`.
- Scalar attr absorption with full provenance tracing (resource literal →
  module call arg → variable default, any depth).
- Nested block attr absorption at any depth; multi-instance block matching by
  stable-attribute scoring.
- `dynamic` block support when `for_each` is a direct `var.x` reference.
- Sensitive attr guard: never writes `after_sensitive = true` attrs to
  plain-text config.
- Unified diff output; `-write` to apply.
- `for_each`-scoped isolation: updates the correct map entry without touching
  siblings.
- Homebrew tap and GoReleaser-based release pipeline.
