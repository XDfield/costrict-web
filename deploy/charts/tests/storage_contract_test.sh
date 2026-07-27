#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
CHARTS_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)"
API_CHART="$CHARTS_DIR/api"
WORKER_CHART="$CHARTS_DIR/worker"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

assert_contains() {
  local file="$1"
  local text="$2"
  grep -Fq -- "$text" "$file" || {
    echo "expected $file to contain: $text" >&2
    exit 1
  }
}

assert_not_contains() {
  local file="$1"
  local text="$2"
  if grep -Fq -- "$text" "$file"; then
    echo "expected $file not to contain: $text" >&2
    exit 1
  fi
}

extract_s3_defaults() {
  local values_file="$1"
  awk '
    /^  s3:/ {
      capture = 1
    }
    capture && /^[^[:space:]]/ {
      exit
    }
    capture {
      print
    }
  ' "$values_file"
}

extract_s3_defaults "$API_CHART/values.yaml" > "$TEST_DIR/api-s3-defaults.yaml"
extract_s3_defaults "$WORKER_CHART/values.yaml" > "$TEST_DIR/worker-s3-defaults.yaml"

if ! diff -u "$TEST_DIR/api-s3-defaults.yaml" "$TEST_DIR/worker-s3-defaults.yaml"; then
  echo "API and worker artifactStorage.s3 defaults must remain symmetric" >&2
  exit 1
fi

for field in \
  'endpoint: ""' \
  'bucket: ""' \
  'region: ""' \
  'forcePathStyle: true' \
  'accessKeySecretKey: access-key' \
  'secretKeySecretKey: secret-key' \
  'sessionTokenSecretKey: ""' \
  'key: ca.crt' \
  'mountPath: /etc/costrict/object-storage'; do
  assert_contains "$TEST_DIR/api-s3-defaults.yaml" "$field"
done

# The worker local defaults deliberately follow an API release named "api".
# This is a convention, not runtime claim discovery.
helm template api "$API_CHART" > "$TEST_DIR/api-local.yaml"
helm template worker "$WORKER_CHART" > "$TEST_DIR/worker-local.yaml"

for rendered in "$TEST_DIR/api-local.yaml" "$TEST_DIR/worker-local.yaml"; do
  assert_contains "$rendered" "name: artifacts-storage"
  assert_contains "$rendered" "claimName: api-artifacts"
  assert_contains "$rendered" "mountPath: /app/data/artifacts"
  assert_contains "$rendered" "name: ARTIFACT_STORAGE_PATH"
  assert_contains "$rendered" 'value: "/app/data/artifacts"'
done
assert_contains "$TEST_DIR/api-local.yaml" "kind: PersistentVolumeClaim"
assert_not_contains "$TEST_DIR/worker-local.yaml" "kind: PersistentVolumeClaim"

render_s3() {
  local release="$1"
  local chart="$2"
  local output="$3"
  helm template "$release" "$chart" \
    --set artifactStorage.backend=s3 \
    --set artifactStorage.s3.endpoint=https://object-storage.example.internal \
    --set artifactStorage.s3.bucket=costrict-artifacts \
    --set artifactStorage.s3.region=internal \
    --set artifactStorage.s3.existingSecret=costrict-s3 \
    --set artifactStorage.s3.ca.existingSecret=costrict-s3-ca > "$output"
}

render_s3 api "$API_CHART" "$TEST_DIR/api-s3.yaml"
render_s3 worker "$WORKER_CHART" "$TEST_DIR/worker-s3.yaml"

for rendered in "$TEST_DIR/api-s3.yaml" "$TEST_DIR/worker-s3.yaml"; do
  assert_contains "$rendered" "name: S3_ENDPOINT"
  assert_contains "$rendered" 'value: "https://object-storage.example.internal"'
  assert_contains "$rendered" "name: S3_BUCKET"
  assert_contains "$rendered" 'value: "costrict-artifacts"'
  assert_contains "$rendered" "name: S3_REGION"
  assert_contains "$rendered" 'value: "internal"'
  assert_contains "$rendered" "name: S3_FORCE_PATH_STYLE"
  assert_contains "$rendered" 'value: "true"'
  assert_contains "$rendered" "name: costrict-s3"
  assert_contains "$rendered" "key: access-key"
  assert_contains "$rendered" "key: secret-key"
  assert_not_contains "$rendered" "name: AWS_SESSION_TOKEN"
  assert_contains "$rendered" "name: AWS_RESPONSE_CHECKSUM_VALIDATION"
  assert_contains "$rendered" 'value: "when_required"'
  assert_contains "$rendered" 'value: "/etc/costrict/object-storage/ca.crt"'
  assert_contains "$rendered" "mountPath: /etc/costrict/object-storage"
  assert_contains "$rendered" "secretName: costrict-s3-ca"
  assert_contains "$rendered" "key: ca.crt"
done

echo "cross-chart storage contract tests passed"
