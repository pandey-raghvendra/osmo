terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

# Root-level resource with a literal attribute (tests root absorb path).
resource "random_string" "root" {
  length  = 8
  special = false
}

# Resource whose attribute flows from a module argument literal
# (tests the module-arg provenance path: module sets prefix = var.prefix).
module "pet" {
  source = "./modules/pet"
  prefix = "alpha"
}
