variable "prefix" {
  type = string
}

resource "random_pet" "this" {
  prefix = var.prefix
  length = 2
}
