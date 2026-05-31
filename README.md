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

## Module-aware (custom modules)

osmo is provenance-driven, not a naive HCL grep. Each drifted attribute is
traced through the plan's `configuration` tree to the **single literal** that
controls it, which may be:

- a resource attribute in the root module,
- a **module-call argument** in the root (when the module sets `attr = var.x`),
- the same, chained through **nested modules** (`module.a.module.b...`),
- a **variable default**.

The edit lands at the literal's real source — for module inputs that means the
**root call argument** (correct blast radius: only that instance), never the
shared module source.

**Instance scoping (`for_each`/`count`):** when a drifted instance maps to one
entry of a map argument, osmo edits just that entry and leaves siblings intact.

**Safely skipped & reported (never silently wrong):**

- values derived from `local.*` (not present in plan JSON),
- `each.*` / `count.*` meta-arguments,
- references to other resources/outputs or composed expressions,
- constants hardcoded inside a **remote** module source (registry/git),
- a constant shared across instances that cannot be isolated.

Each skip prints `! <address>.<attr>: <reason>`.

## Scope

**Now:** attribute drift on existing resources, through local custom modules
(root + nested), with instance scoping; diff to stdout (`-write` to apply).

**Not yet:**
- Out-of-band *created* resources (needs `import {}` block + HCL codegen)
- Nested *block* drift (e.g. `ebs_block_device {}`) — top-level attrs only
- `local.*` provenance (locals aren't in plan JSON)
- PR creation (planned — open a branch + PR instead of local diff)

## License

[Apache License 2.0](LICENSE). Permissive, with an explicit patent grant —
suitable for adoption inside companies' infrastructure pipelines.
