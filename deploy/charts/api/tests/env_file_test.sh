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

# Default: no envFile configured, nothing extra is rendered.
helm template default "$CHART_DIR" > "$TEST_DIR/default.yaml"
assert_not_contains "$TEST_DIR/default.yaml" "name: app-env-file"
assert_not_contains "$TEST_DIR/default.yaml" "mountPath: /app/.env"
assert_not_contains "$TEST_DIR/default.yaml" "name: CS_BOT_TOKEN_KEY"
assert_not_contains "$TEST_DIR/default.yaml" "name: GIT_SERVER_TEMPLATE_ADMIN_TOKEN"
assert_env_value "$TEST_DIR/default.yaml" "GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS" ""

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

# V4 production settings use externally provisioned Secrets rather than values.
helm template v4-secrets "$CHART_DIR" \
  --set botTokenKey.existingSecret=costrict-bot-key \
  --set botTokenKey.key=token-key \
  --set gitServerTemplate.endpoint=http://gitea.gitea.svc:3000 \
  --set gitServerTemplate.adminToken.existingSecret=gitea-admin \
  --set gitServerTemplate.adminToken.key=token \
  --set gitServerTemplate.displayName=Production \
  --set gitServerTemplate.adminUser=gitadmin \
  --set gitServerTemplate.adminPassword=change-me \
  --set gitCapability.discoveryExcludedOwners=mirror > "$TEST_DIR/v4-secrets.yaml"
assert_contains "$TEST_DIR/v4-secrets.yaml" "name: CS_BOT_TOKEN_KEY"
assert_contains "$TEST_DIR/v4-secrets.yaml" "name: costrict-bot-key"
assert_contains "$TEST_DIR/v4-secrets.yaml" "key: token-key"
assert_env_value "$TEST_DIR/v4-secrets.yaml" "GIT_SERVER_TEMPLATE_ENDPOINT" "http://gitea.gitea.svc:3000"
assert_contains "$TEST_DIR/v4-secrets.yaml" "name: GIT_SERVER_TEMPLATE_ADMIN_TOKEN"
assert_contains "$TEST_DIR/v4-secrets.yaml" "name: gitea-admin"
assert_contains "$TEST_DIR/v4-secrets.yaml" "key: token"
assert_env_value "$TEST_DIR/v4-secrets.yaml" "GIT_SERVER_TEMPLATE_DISPLAY_NAME" "Production"
assert_env_value "$TEST_DIR/v4-secrets.yaml" "GIT_SERVER_TEMPLATE_ADMIN_USER" "gitadmin"
assert_env_value "$TEST_DIR/v4-secrets.yaml" "GIT_SERVER_TEMPLATE_ADMIN_PASSWORD" "change-me"
assert_env_value "$TEST_DIR/v4-secrets.yaml" "GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS" "mirror"

echo "api environment render tests passed"
