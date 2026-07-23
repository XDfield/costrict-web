#!/usr/bin/env bash
# list-idps.sh — list IdP sources configured for a tenant.
#
# Wraps GET /api/idp-sources/{tenant_id}. Secrets are redacted server-side;
# for the secret-included view use the internal RPC (server-side only).
#
# Optional:
#   --tenant     override CS_USER_TENANT_ID
#   --enabled    show only enabled sources (calls /enabled endpoint)
#
# Env (see lib/common.sh): CS_USER_INTERNAL_TOKEN, CS_USER_BASE_URL,
#                           CS_USER_TENANT_ID.
set -euo pipefail

usage() {
    sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
    exit 1
}

TENANT_FLAG=""
ENABLED_ONLY=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --tenant) TENANT_FLAG="$2"; shift 2 ;;
        --enabled) ENABLED_ONLY=1; shift ;;
        --help|-h) usage ;;
        *) echo "unknown flag: $1" >&2; usage ;;
    esac
done

# shellcheck source=lib/common.sh
source "$(dirname "$0")/lib/common.sh"

TENANT=$(csu_resolve_tenant "$TENANT_FLAG")

if [[ $ENABLED_ONLY -eq 1 ]]; then
    csu_log "enabled idps tenant=$TENANT"
    csu_get "/api/idp-sources/${TENANT}/enabled" | csu_pretty
else
    csu_log "all idps tenant=$TENANT"
    csu_get "/api/idp-sources/${TENANT}" | csu_pretty
fi
