#!/usr/bin/env bash
# configure-employment-mapping.sh — set employment_providers field_map +
# provider_detection for a tenant.
#
# Wraps PUT /api/internal/tenant/config (full YAML replace of
# tenant_configs.config_yaml). Reads a YAML file from disk and ships it.
#
# The YAML file should look like examples/idtrust-employment.yaml — it must
# include the employment_providers section with:
#   - enabled:         list of provider names
#   - provider_detection: optional list of {signup_application, provider}
#                          rules (Plan B detection)
#   - field_map:       per-provider {internal_column: external_field_path}
#                          map. Paths may be dotted (e.g.
#                          "properties.oauth_Custom_id" walks flat Casdoor
#                          properties keys; nested maps are also supported).
#
# Required:
#   --yaml         path to YAML file with the employment_providers section
# Optional:
#   --tenant       override CS_USER_TENANT_ID
#   --merge-only   if set, merge with existing YAML instead of replacing
#                  (uses python yaml.discard Preserve — currently NOT
#                  implemented; reserved flag, errors out)
#
# Env (see lib/common.sh).
#
# Example:
#   ./configure-employment-mapping.sh \
#       --tenant acme-corp \
#       --yaml examples/idtrust-employment.yaml
set -euo pipefail

usage() {
    sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
    exit 1
}

YAML_FILE=""
TENANT_FLAG=""
MERGE_ONLY=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --yaml) YAML_FILE="$2"; shift 2 ;;
        --tenant) TENANT_FLAG="$2"; shift 2 ;;
        --merge-only) MERGE_ONLY=1; shift ;;
        --help|-h) usage ;;
        *) echo "unknown flag: $1" >&2; usage ;;
    esac
done

[[ -n "$YAML_FILE" ]] || usage
[[ $MERGE_ONLY -eq 1 ]] && { echo "--merge-only is reserved (not yet implemented)" >&2; exit 2; }

# shellcheck source=lib/common.sh
source "$(dirname "$0")/lib/common.sh"

TENANT=$(csu_resolve_tenant "$TENANT_FLAG")
YAML_CONTENT=$(csu_read_file "$YAML_FILE")

# The PUT /api/internal/tenant/config endpoint accepts the YAML as a JSON
# string field `config_yaml`. python keeps quoting safe.
BODY=$(YAML_CONTENT="$YAML_CONTENT" python -c "
import json, os
print(json.dumps({'config_yaml': os.environ['YAML_CONTENT']}))
")

csu_log "uploading tenant config_yaml tenant=$TENANT (bytes=${#YAML_CONTENT})"
RESP=$(csu_json_request PUT /api/internal/tenant/config "$BODY")
printf '%s\n' "$RESP" | csu_pretty
