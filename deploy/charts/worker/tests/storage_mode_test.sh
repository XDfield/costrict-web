#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
CHART_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

assert_contains() {
  local file="$1"
  local text="$2"
  grep -Fq -- "$text" "$file" || {
    echo "expected rendered manifest to contain: $text" >&2
    exit 1
  }
}

assert_not_contains() {
  local file="$1"
  local text="$2"
  if grep -Fq -- "$text" "$file"; then
    echo "expected rendered manifest not to contain: $text" >&2
    exit 1
  fi
}

assert_env_value() {
  local file="$1"
  local name="$2"
  local value="$3"
  grep -F -A1 -- "- name: $name" "$file" \
    | grep -Fq -- "value: \"$value\"" || {
      echo "expected rendered env $name to equal: $value" >&2
      exit 1
    }
}

helm template local-storage "$CHART_DIR" > "$TEST_DIR/local.yaml"
assert_not_contains "$TEST_DIR/local.yaml" "kind: PersistentVolumeClaim"
assert_not_contains "$TEST_DIR/local.yaml" "kind: StorageClass"
assert_contains "$TEST_DIR/local.yaml" "name: artifacts-storage"
assert_contains "$TEST_DIR/local.yaml" "claimName: api-artifacts"
assert_contains "$TEST_DIR/local.yaml" "name: ARTIFACT_STORAGE_BACKEND"
assert_contains "$TEST_DIR/local.yaml" 'value: "local"'
assert_contains "$TEST_DIR/local.yaml" "name: ARTIFACT_STORAGE_PATH"
assert_not_contains "$TEST_DIR/local.yaml" "name: S3_ENDPOINT"
assert_not_contains "$TEST_DIR/local.yaml" "name: GIT_SYSTEM_WEBHOOK_BASE_URL"
assert_env_value "$TEST_DIR/local.yaml" "GIT_SYSTEM_HOOK_RECONCILE_INTERVAL_SECONDS" "300"
assert_env_value "$TEST_DIR/local.yaml" "WORKER_CONCURRENCY" "5"
assert_not_contains "$TEST_DIR/local.yaml" "name: CONCURRENCY"
assert_env_value "$TEST_DIR/local.yaml" "GIT_CAPABILITY_WORKER_CONCURRENCY" "8"
assert_env_value "$TEST_DIR/local.yaml" "GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS" ""

helm template v4-worker "$CHART_DIR" \
  --set config.concurrency=12 \
  --set gitCapability.workerConcurrency=16 \
  --set gitCapability.discoveryExcludedOwners=mirror \
  --set gitCapability.reconcileInterval=2m \
  --set gitCapability.reconcileBatchSize=80 > "$TEST_DIR/v4-worker.yaml"
assert_env_value "$TEST_DIR/v4-worker.yaml" "WORKER_CONCURRENCY" "12"
assert_env_value "$TEST_DIR/v4-worker.yaml" "GIT_CAPABILITY_WORKER_CONCURRENCY" "16"
assert_env_value "$TEST_DIR/v4-worker.yaml" "GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS" "mirror"
assert_env_value "$TEST_DIR/v4-worker.yaml" "GIT_CAPABILITY_RECONCILE_INTERVAL" "2m"
assert_env_value "$TEST_DIR/v4-worker.yaml" "GIT_CAPABILITY_RECONCILE_BATCH_SIZE" "80"

helm template webhook-enabled "$CHART_DIR" \
  --set gitSystemWebhook.baseURL=https://cloud.example/cloud-api \
  --set gitSystemWebhook.reconcileIntervalSeconds=37 > "$TEST_DIR/webhook-enabled.yaml"
assert_env_value "$TEST_DIR/webhook-enabled.yaml" "GIT_SYSTEM_WEBHOOK_BASE_URL" "https://cloud.example/cloud-api"
assert_env_value "$TEST_DIR/webhook-enabled.yaml" "GIT_SYSTEM_HOOK_RECONCILE_INTERVAL_SECONDS" "37"

helm template local-shared "$CHART_DIR" \
  --set artifactStorage.local.existingClaim=api-artifacts-rwx > "$TEST_DIR/local-shared.yaml"
assert_not_contains "$TEST_DIR/local-shared.yaml" "kind: PersistentVolumeClaim"
assert_not_contains "$TEST_DIR/local-shared.yaml" "kind: StorageClass"
assert_contains "$TEST_DIR/local-shared.yaml" "name: artifacts-storage"
assert_contains "$TEST_DIR/local-shared.yaml" "claimName: api-artifacts-rwx"
assert_contains "$TEST_DIR/local-shared.yaml" "mountPath: /app/data/artifacts"

if helm template incomplete-local "$CHART_DIR" \
  --set artifactStorage.local.existingClaim= > "$TEST_DIR/incomplete-local.out" 2>&1; then
  echo "expected local storage without a shared claim to fail rendering" >&2
  exit 1
fi
assert_contains "$TEST_DIR/incomplete-local.out" "artifactStorage.local.existingClaim is required"

if helm template incomplete-local-path "$CHART_DIR" \
  --set artifactStorage.local.mountPath= > "$TEST_DIR/incomplete-local-path.out" 2>&1; then
  echo "expected local storage without a mount path to fail rendering" >&2
  exit 1
fi
assert_contains "$TEST_DIR/incomplete-local-path.out" "artifactStorage.local.mountPath is required"

helm template s3-storage "$CHART_DIR" \
  --set artifactStorage.backend=s3 \
  --set artifactStorage.s3.endpoint=https://object-storage.example.internal \
  --set artifactStorage.s3.bucket=costrict-artifacts \
  --set artifactStorage.s3.region=internal \
  --set artifactStorage.s3.existingSecret=costrict-s3 \
  --set artifactStorage.s3.ca.existingSecret=costrict-s3-ca > "$TEST_DIR/s3.yaml"

assert_not_contains "$TEST_DIR/s3.yaml" "kind: PersistentVolumeClaim"
assert_not_contains "$TEST_DIR/s3.yaml" "kind: StorageClass"
assert_not_contains "$TEST_DIR/s3.yaml" "name: artifacts-storage"
assert_not_contains "$TEST_DIR/s3.yaml" "name: ARTIFACT_STORAGE_PATH"
assert_contains "$TEST_DIR/s3.yaml" 'value: "s3"'
assert_contains "$TEST_DIR/s3.yaml" "name: S3_ENDPOINT"
assert_contains "$TEST_DIR/s3.yaml" "name: AWS_ACCESS_KEY_ID"
assert_contains "$TEST_DIR/s3.yaml" "name: AWS_REQUEST_CHECKSUM_CALCULATION"
assert_contains "$TEST_DIR/s3.yaml" "name: AWS_RESPONSE_CHECKSUM_VALIDATION"
assert_contains "$TEST_DIR/s3.yaml" 'value: "when_required"'
assert_contains "$TEST_DIR/s3.yaml" "name: S3_CA_FILE"
assert_contains "$TEST_DIR/s3.yaml" "secretName: costrict-s3-ca"
assert_contains "$TEST_DIR/s3.yaml" "name: artifact-storage-ca"

if helm template invalid-storage "$CHART_DIR" \
  --set artifactStorage.backend=unknown > "$TEST_DIR/invalid.out" 2>&1; then
  echo "expected an unknown artifact storage backend to fail rendering" >&2
  exit 1
fi
assert_contains "$TEST_DIR/invalid.out" "artifactStorage.backend must be one of local or s3"

if helm template incomplete-s3 "$CHART_DIR" \
  --set artifactStorage.backend=s3 > "$TEST_DIR/incomplete.out" 2>&1; then
  echo "expected incomplete S3 configuration to fail rendering" >&2
  exit 1
fi
assert_contains "$TEST_DIR/incomplete.out" "artifactStorage.s3.endpoint is required"

for ca_field in key mountPath; do
  if helm template incomplete-s3-ca "$CHART_DIR" \
    --set artifactStorage.backend=s3 \
    --set artifactStorage.s3.endpoint=https://object-storage.example.internal \
    --set artifactStorage.s3.bucket=costrict-artifacts \
    --set artifactStorage.s3.region=internal \
    --set artifactStorage.s3.existingSecret=costrict-s3 \
    --set artifactStorage.s3.ca.existingSecret=costrict-s3-ca \
    --set "artifactStorage.s3.ca.${ca_field}=" > "$TEST_DIR/incomplete-ca-${ca_field}.out" 2>&1; then
    echo "expected S3 CA configuration without ca.${ca_field} to fail rendering" >&2
    exit 1
  fi
  assert_contains "$TEST_DIR/incomplete-ca-${ca_field}.out" \
    "artifactStorage.s3.ca.${ca_field} is required"
done

echo "worker storage mode render tests passed"
