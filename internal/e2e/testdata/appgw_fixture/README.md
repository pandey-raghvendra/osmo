Place sanitized Azure Application Gateway `.tf` files for the optional real
drift fixture in this directory.

Expected companion file:

- `../real_appgw_drift_show.json`: output from
  `terraform show -json drift.tfplan`

The optional e2e test is skipped until both this directory contains `.tf` files
and the JSON fixture exists.
