# Changelog

All notable changes to osmo are documented here.

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [Unreleased]

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
