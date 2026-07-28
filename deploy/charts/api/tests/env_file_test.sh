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

# Default: no envFile configured, nothing extra is rendered.
helm template default "$CHART_DIR" > "$TEST_DIR/default.yaml"
assert_not_contains "$TEST_DIR/default.yaml" "name: app-env-file"
assert_not_contains "$TEST_DIR/default.yaml" "mountPath: /app/.env"

# ConfigMap mounted as /app/.env via subPath.
helm template with-env-file "$CHART_DIR" \
  --set envFile.existingConfigMap=costrict-api-env > "$TEST_DIR/with-env-file.yaml"
assert_contains "$TEST_DIR/with-env-file.yaml" "name: app-env-file"
assert_contains "$TEST_DIR/with-env-file.yaml" "mountPath: /app/.env"
assert_contains "$TEST_DIR/with-env-file.yaml" "subPath: .env"
assert_contains "$TEST_DIR/with-env-file.yaml" "readOnly: true"
assert_contains "$TEST_DIR/with-env-file.yaml" "name: costrict-api-env"
assert_contains "$TEST_DIR/with-env-file.yaml" "key: .env"
assert_contains "$TEST_DIR/with-env-file.yaml" "path: .env"

# Custom ConfigMap key is honoured.
helm template custom-key "$CHART_DIR" \
  --set envFile.existingConfigMap=costrict-api-env \
  --set envFile.key=custom.env > "$TEST_DIR/custom-key.yaml"
assert_contains "$TEST_DIR/custom-key.yaml" "key: custom.env"
assert_contains "$TEST_DIR/custom-key.yaml" "path: .env"

echo "api env file render tests passed"
