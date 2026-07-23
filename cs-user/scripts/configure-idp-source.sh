#!/usr/bin/env bash
# configure-idp-source.sh — create or update an IdP source for a tenant.
#
# Wraps POST /api/idp-sources (create) and PUT /api/idp-sources/{tenant}/{provider}
# (update). The script is idempotent: it tries PUT first, falls back to POST
# when the source doesn't exist yet.
#
# Required:
#   --provider      provider key (must match an entry in provider_mapping,
#                   e.g. idtrust / wxwork / github)
#   --config-json   path to JSON file with provider config (client_id,
#                   client_secret, endpoints, scopes, field_map, ...)
# Optional:
#   --tenant        override CS_USER_TENANT_ID
#   --scope         visibility scope (default: tenant-specific)
#   --priority      lower priority sorts first on login page (default: 0)
#   --enabled       "true" / "false" (default: true)
#   --created-by    actor label recorded in audit trail
#
# Env (see lib/common.sh): CS_USER_INTERNAL_TOKEN, CS_USER_BASE_URL,
#                           CS_USER_TENANT_ID.
#
# Example:
#   ./configure-idp-source.sh \
#       --tenant acme-corp \
#       --provider idtrust \
#       --config-json examples/idtrust-idp.json
set -euo pipefail

usage() {
    sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
    exit 1
}

PROVIDER=""
CONFIG_FILE=""
TENANT_FLAG=""
SCOPE="tenant-specific"
PRIORITY="0"
ENABLED="true"
CREATED_BY="${USER:-ops}"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --provider) PROVIDER="$2"; shift 2 ;;
        --config-json) CONFIG_FILE="$2"; shift 2 ;;
        --tenant) TENANT_FLAG="$2"; shift 2 ;;
        --scope) SCOPE="$2"; shift 2 ;;
        --priority) PRIORITY="$2"; shift 2 ;;
        --enabled) ENABLED="$2"; shift 2 ;;
        --created-by) CREATED_BY="$2"; shift 2 ;;
        --help|-h) usage ;;
        *) echo "unknown flag: $1" >&2; usage ;;
    esac
done

[[ -n "$PROVIDER" && -n "$CONFIG_FILE" ]] || usage

# shellcheck source=lib/common.sh
source "$(dirname "$0")/lib/common.sh"

TENANT=$(csu_resolve_tenant "$TENANT_FLAG")
CONFIG_JSON=$(csu_read_file "$CONFIG_FILE")

# Embed the config map into the request payload. python validates JSON syntax
# at this stage so we surface a clean error before hitting the API.
BODY=$(CONFIG_JSON="$CONFIG_JSON" python -c "
import json, os
config = json.loads(os.environ['CONFIG_JSON'])
print(json.dumps({
    'tenant_id': '$TENANT',
    'provider': '$PROVIDER',
    'config': config,
    'scope': '$SCOPE',
    'priority': int('$PRIORITY'),
    'enabled': '$ENABLED'.lower() == 'true',
    'created_by': '$CREATED_BY',
}))
")

# Try PUT first (update). 404 → fall back to POST (create).
csu_log "upserting idp source tenant=$TENANT provider=$PROVIDER"
HTTP_CODE=$(curl -sS -o /tmp/csu-idp-resp.$$ -w "%{http_code}" \
    -X PUT "${CS_USER_BASE_URL}/api/idp-sources/${TENANT}/${PROVIDER}" \
    -H "X-Internal-Token: ${CS_USER_INTERNAL_TOKEN}" \
    -H 'Content-Type: application/json' \
    --data "$BODY" || true)
RESP=$(cat /tmp/csu-idp-resp.$$); rm -f /tmp/csu-idp-resp.$$

if [[ "$HTTP_CODE" == "404" ]]; then
    csu_log "not found, falling back to POST /api/idp-sources"
    RESP=$(csu_json_request POST /api/idp-sources "$BODY")
elif [[ "$HTTP_CODE" != "200" ]]; then
    csu_die "PUT failed (HTTP $HTTP_CODE): $RESP"
fi

printf '%s\n' "$RESP" | csu_pretty
