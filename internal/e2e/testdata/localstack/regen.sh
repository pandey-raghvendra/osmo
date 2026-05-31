#!/usr/bin/env bash
# Regenerate real_drift_show.json — a real terraform show -json output that
# contains resource_drift entries — using LocalStack (no real AWS creds needed).
#
# Prerequisites: docker, terraform, aws CLI.
# Usage: bash regen.sh
set -euo pipefail
cd "$(dirname "$0")"

ENDPOINT="http://localhost:4566"
AWS_FLAGS="--endpoint-url $ENDPOINT --region us-east-1 --no-cli-pager"
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_DEFAULT_REGION=us-east-1

echo "==> starting LocalStack..."
docker compose up -d
echo -n "    waiting for health..."
for i in $(seq 1 30); do
  curl -sf "$ENDPOINT/_localstack/health" >/dev/null 2>&1 && break
  sleep 2
  echo -n "."
done
echo " ready"

echo "==> terraform init + apply..."
cd localstack 2>/dev/null || true   # already in the dir if called directly
terraform init -no-color -input=false -upgrade >/dev/null
terraform apply -auto-approve -no-color -input=false

echo "==> injecting drift: add tag Managed=manual via AWS API (bypasses TF)..."
aws s3api put-bucket-tagging $AWS_FLAGS \
  --bucket osmo-drift-test-bucket \
  --tagging 'TagSet=[{Key=Env,Value=test},{Key=Managed,Value=manual}]'
echo "    tags after drift:"
aws s3api get-bucket-tagging $AWS_FLAGS --bucket osmo-drift-test-bucket

echo "==> capture refresh-only plan with real resource_drift..."
terraform plan -refresh-only -out=tf.plan -input=false -no-color
terraform show -json tf.plan > ../real_drift_show.json

echo "==> verifying resource_drift present..."
python3 -c "
import json, sys
d = json.load(open('../real_drift_show.json'))
rd = d.get('resource_drift', [])
print(f'  resource_drift count: {len(rd)}')
if not rd:
    print('ERROR: no resource_drift found — drift injection failed', file=sys.stderr)
    sys.exit(1)
for r in rd:
    print(f'  address={r[\"address\"]}')
    print(f'  before.tags={r[\"change\"][\"before\"].get(\"tags\")}')
    print(f'  after.tags={r[\"change\"][\"after\"].get(\"tags\")}')
"

echo "==> cleanup..."
terraform destroy -auto-approve -no-color -input=false || true
rm -rf .terraform .terraform.lock.hcl tf.plan terraform.tfstate terraform.tfstate.backup
cd "$(dirname "$0")"
docker compose down

echo "wrote $(pwd)/real_drift_show.json"
