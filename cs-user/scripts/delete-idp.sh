#!/usr/bin/env bash
# delete-idp.sh — remove an IdP source.
#
# Wraps DELETE /api/idp-sources/{tenant}/{provider}. Destructive — there is
# no undo. Existing user_auth_identities rows are NOT cascade-deleted (users
# keep their auth methods); only the source config is removed, so logins
# through this provider will fail going forward.
#
# Required:
#   --provider    provider key to delete
# Optional:
#   --tenant      override CS_USER_TENANT_ID
#   --yes         skip the interactive confirmation prompt
#
# Env (see lib/common.sh).
set -euo pipefail

usage() {
    sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
    exit 1
}

PROVIDER=""
TENANT_FLAG=""
ASSUME_YES=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --provider) PROVIDER="$2"; shift 2 ;;
        --tenant) TENANT_FLAG="$2"; shift 2 ;;
        --yes|-y) ASSUME_YES=1; shift ;;
        --help|-h) usage ;;
        *) echo "unknown flag: $1" >&2; usage ;;
    esac
done

[[ -n "$PROVIDER" ]] || usage

# shellcheck source=lib/common.sh
source "$(dirname "$0")/lib/common.sh"

TENANT=$(csu_resolve_tenant "$TENANT_FLAG")

if [[ $ASSUME_YES -ne 1 ]]; then
    echo "About to DELETE IdP source:" >&2
    echo "  tenant:   $TENANT" >&2
    echo "  provider: $PROVIDER" >&2
    printf 'Confirm? [y/N] ' >&2
    read -r ans
    [[ "$ans" =~ ^[yY]([eE][sS])?$ ]] || { echo "aborted" >&2; exit 1; }
fi

csu_log "deleting idp source tenant=$TENANT provider=$PROVIDER"
OUT=$(csu_json_request DELETE "/api/idp-sources/${TENANT}/${PROVIDER}")
STATUS=$(csu_status "$OUT")
RESP=$(csu_body "$OUT")
[[ "$STATUS" == "200" || "$STATUS" == "204" ]] || csu_die "DELETE failed (HTTP $STATUS): $RESP"
[[ -z "$RESP" ]] && echo "(deleted)" || printf '%s\n' "$RESP" | csu_pretty
