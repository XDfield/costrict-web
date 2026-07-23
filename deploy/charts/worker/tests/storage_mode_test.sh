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

helm template local-storage "$CHART_DIR" > "$TEST_DIR/local.yaml"
assert_not_contains "$TEST_DIR/local.yaml" "kind: PersistentVolumeClaim"
assert_not_contains "$TEST_DIR/local.yaml" "kind: StorageClass"
assert_contains "$TEST_DIR/local.yaml" "name: artifacts-storage"
assert_contains "$TEST_DIR/local.yaml" "claimName: api-artifacts"
assert_contains "$TEST_DIR/local.yaml" "name: ARTIFACT_STORAGE_BACKEND"
assert_contains "$TEST_DIR/local.yaml" 'value: "local"'
assert_contains "$TEST_DIR/local.yaml" "name: ARTIFACT_STORAGE_PATH"
assert_not_contains "$TEST_DIR/local.yaml" "name: S3_ENDPOINT"

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

echo "worker storage mode render tests passed"
