# osmo

**Osmosis for your Terraform.** Reality seeps into your config until both sides
equalize.

`osmo` detects Terraform drift and proposes HCL changes that make your
**configuration follow real-world reality** — the "absorb" direction.

## Why absorb, not revert

Production incidents get hotfixed directly in the cloud console. Those fixes
can't wait for a Terraform apply cycle. So the source of truth should follow
reality, not overwrite it. This tool rewrites your `.tf` files to match what's
actually running, then you review and verify with a normal `terraform plan`.

## How it works

```
terraform plan -refresh-only -out=plan   # detect drift, no config-driven changes
terraform show -json plan                 # structured drift (resource_drift[])
  -> for each drifted resource:
       changed attrs = before != after
       rewrite ONLY attrs already present as literals in the HCL block
  -> emit unified diff (or write files with -write)
you run `terraform plan` -> 0 diff = drift resolved
```

### Safety rule (v1)

Only attributes that **(a)** changed between prior state and refreshed reality
**and (b)** already exist as a literal in the resource block are rewritten.
Computed / read-only attributes that never appear in config are never injected,
so the tool cannot produce invalid HCL.

## Usage

```sh
# preview changes (diff only, writes nothing)
osmo -dir ./infra

# apply the absorbed changes to disk
osmo -dir ./infra -write
terraform -chdir=./infra plan   # verify: should show no changes
```

Flags:

| Flag         | Default       | Meaning                              |
|--------------|---------------|--------------------------------------|
| `-dir`       | `.`           | Terraform working directory          |
| `-terraform` | `terraform`   | Terraform binary to invoke           |
| `-write`     | `false`       | Write changes to disk (else diff only)|

## Scope

**v1 (now):** modify attribute drift on resources already in HCL; diff to stdout.

**Not yet:**
- Out-of-band *created* resources (needs `import {}` block + HCL codegen)
- Nested block / nested attribute drift (only top-level attrs today)
- PR creation (planned — open a branch + PR instead of local diff)
