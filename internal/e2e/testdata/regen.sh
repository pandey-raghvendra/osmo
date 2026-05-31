#!/usr/bin/env bash
# Regenerate real_show.json from the fixture using a live terraform run.
# Requires terraform + network (downloads the hashicorp/random provider).
# No cloud credentials needed.
set -euo pipefail

cd "$(dirname "$0")/fixture"

terraform init -no-color -input=false
terraform apply -auto-approve -no-color -input=false
terraform plan -refresh-only -out=tf.plan -input=false -no-color
terraform show -json tf.plan > ../real_show.json

# Clean local state/artifacts; keep only real_show.json + the .tf fixture.
terraform destroy -auto-approve -no-color -input=false || true
rm -rf .terraform .terraform.lock.hcl tf.plan terraform.tfstate terraform.tfstate.backup

echo "wrote $(cd .. && pwd)/real_show.json"
