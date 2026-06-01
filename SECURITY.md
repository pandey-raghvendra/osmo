# Security Policy

## Supported versions

| Version | Supported |
|---------|-----------|
| Latest release | ✅ |
| Older releases | ❌ — upgrade to latest |

## Reporting a vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

Report privately via [GitHub Security Advisories](https://github.com/pandey-raghvendra/osmo/security/advisories/new).

Include:
- Description and impact
- Steps to reproduce
- osmo version (`osmo -version`) and OS
- Any relevant `.tf` snippets or plan JSON (redact real credentials)

Expected response: acknowledgement within 72 hours, patch within 14 days for confirmed issues.

## Threat model notes

osmo reads `terraform show -json` output (which may contain resource state) and rewrites `.tf` files on disk. It never:
- Calls any cloud provider API directly
- Transmits plan data or HCL to any remote service
- Stores credentials or state beyond the lifetime of the process

Sensitive attributes (`after_sensitive = true` in the plan) are explicitly detected and never written to plain-text config.
