# osmo

**Osmosis for your Terraform.** Reality seeps into your config until both sides equalize.

`osmo` detects Terraform drift and rewrites your `.tf` files so that **configuration follows real-world reality** — the *absorb* direction.

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

# Verify drift is resolved
terraform -chdir=./infra plan   # should show: No changes
```

| Flag | Default | Meaning |
|---|---|---|
| `-dir` | `.` | Terraform working directory |
| `-terraform` | `terraform` | Terraform binary path |
| `-write` | `false` | Write changes to disk (else diff only) |

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
| Scalar attr — `for_each` scoped | one map entry updated, siblings intact | ✅ |
| Nested block attr — literal (any depth) | `ebs_block_device { volume_size = 20 }` | ✅ |
| Nested block attr — `var.x` ref | `root_block_device { volume_size = var.size }` | ✅ |
| Nested block add | new `ingress {}` block added out-of-band | ✅ |
| Nested block remove | `ingress {}` block removed out-of-band | ✅ |
| Multi-instance block matching | two `ebs_block_device` blocks; correct one updated | ✅ |
| Deep nesting (3+ levels) | `server_side_encryption_configuration > rule > ...` | ✅ |
| `dynamic` block — `for_each = var.x` | collection variable updated to full after-state | ✅ |
| Sensitive attr (`after_sensitive = true`) | skipped, reported — never written to plain text | ✅ |
| Null after-value (attr removed in reality) | skipped, reported — removal not auto-applied | ✅ |

### Safely reported, never silently wrong

Each unresolvable drift prints `! <address>.<attr>: <reason>`. Nothing is guessed.

| Pattern | Reason reported |
|---|---|
| `local.*` | locals not present in plan JSON |
| `each.*` / `count.*` | meta-argument, not a literal |
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

## Provider support

osmo is **provider-agnostic**: it reads `terraform show -json` output and edits HCL. Any provider whose resources appear in `resource_drift[]` is supported. Tested against:

- AWS (`aws_*`)
- Azure (`azurerm_*`)
- Custom local modules

---

## Requirements

- Go 1.21+ (to build from source)
- Terraform ≥ 1.0 (for `show -json` with `resource_drift`)

---

## Install

### Homebrew (macOS / Linux)
```sh
# coming soon
```

### Go install
```sh
go install github.com/raghav/osmo/cmd/osmo@latest
```

### Build from source
```sh
git clone https://github.com/pandey-raghvendra/osmo
cd osmo
go build -o osmo ./cmd/osmo
```

---

## Known limitations

| Gap | Notes |
|---|---|
| `local.*` provenance | locals absent from plan JSON — fundamental limit |
| Composed expressions | `"${var.a}-${var.b}"` has multiple references |
| `dynamic` + map `for_each` | map reconstruction from expanded blocks not supported |
| Out-of-band *created* resources | needs `import {}` + HCL codegen — out of scope |
| Post-absorb verification | run `terraform plan` yourself to confirm 0 diff |

---

## License

[Apache License 2.0](LICENSE) — permissive, with explicit patent grant; suitable for use inside corporate infrastructure pipelines.
