#!/usr/bin/env bash
# bootstrap-tenant.sh — idempotent create-or-skip for a cs-user tenant.
#
# Wraps GET/POST/PATCH on /api/internal/platform/tenants[/{slug}]. The
# platform API mints a UUID tenant_id server-side; the tenant slug is the
# stable human-facing id used by all subsequent scripts.
#
# Behavior:
#   * slug not found (HTTP 404)  → POST create
#   * slug found     (HTTP 200)  → skip by default; with --update-if-exists,
#                                  PATCH syncs display_name / edition /
#                                  email_domains to the values passed here.
#                                  slug + tenant_id are immutable upstream.
#
# Required:
#   --slug              stable lowercase slug (matches ^[a-z0-9-]{1,64}$)
#   --display-name      human-readable name
# Optional:
#   --edition           community | standard | enterprise (default: community)
#   --email-domain      email domain to auto-bind to this tenant (repeatable)
#   --update-if-exists  if the tenant already exists, PATCH its mutable
#                       fields instead of skipping (default: skip)
#
# Env (see lib/common.sh): CS_USER_INTERNAL_TOKEN, CS_USER_BASE_URL.
#
# Example:
#   ./bootstrap-tenant.sh \
#       --slug acme-corp \
#       --display-name "Acme Corporation" \
#       --edition enterprise \
#       --email-domain acme.example.com \
#       --update-if-exists
set -euo pipefail

usage() {
    sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
    exit 1
}

SLUG=""
DISPLAY_NAME=""
EDITION="community"
EMAIL_DOMAINS=()
UPDATE_IF_EXISTS=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --slug) SLUG="$2"; shift 2 ;;
        --display-name) DISPLAY_NAME="$2"; shift 2 ;;
        --edition) EDITION="$2"; shift 2 ;;
        --email-domain) EMAIL_DOMAINS+=("$2"); shift 2 ;;
        --update-if-exists) UPDATE_IF_EXISTS=1; shift ;;
        --help|-h) usage ;;
        *) echo "unknown flag: $1" >&2; usage ;;
    esac
done

[[ -n "$SLUG" && -n "$DISPLAY_NAME" ]] || usage

# shellcheck source=lib/common.sh
source "$(dirname "$0")/lib/common.sh"

# ---- lookup tenant by slug -----------------------------------------------
LOOKUP_RESP=$(csu_json_request GET "/api/internal/platform/tenants/${SLUG}" "")
LOOKUP_STATUS=$(csu_status "$LOOKUP_RESP")
LOOKUP_BODY=$(csu_body "$LOOKUP_RESP")

if [[ "$LOOKUP_STATUS" == "200" ]]; then
    EXISTING_ID=$(printf '%s' "$LOOKUP_BODY" | jq -r '.tenant_id // "?"')
    if [[ "$UPDATE_IF_EXISTS" == "1" ]]; then
        csu_log "tenant slug=$SLUG exists (id=$EXISTING_ID); PATCHing mutable fields (--update-if-exists)"
        UPDATE_BODY=$(jq -nc \
            --arg name "$DISPLAY_NAME" \
            --arg edition "$EDITION" \
            '{display_name:$name, edition:$edition, email_domains:($ARGS.positional | map(.))}' \
            "${EMAIL_DOMAINS[@]}")
        UPDATE_RESP=$(csu_json_request PATCH "/api/internal/platform/tenants/${SLUG}" "$UPDATE_BODY")
        UPDATE_STATUS=$(csu_status "$UPDATE_RESP")
        UPDATE_BODY_OUT=$(csu_body "$UPDATE_RESP")
        [[ "$UPDATE_STATUS" == "200" ]] || csu_die "PATCH failed (HTTP $UPDATE_STATUS): $UPDATE_BODY_OUT"
        printf '%s\n' "$UPDATE_BODY_OUT" | csu_pretty
    else
        csu_log "tenant slug=$SLUG already exists (id=$EXISTING_ID); skipping (use --update-if-exists to sync)"
        printf '%s\n' "$LOOKUP_BODY" | csu_pretty
    fi
    exit 0
fi

if [[ "$LOOKUP_STATUS" != "404" ]]; then
    csu_die "GET tenant lookup failed (HTTP $LOOKUP_STATUS): $LOOKUP_BODY"
fi

# ---- create --------------------------------------------------------------
BODY=$(jq -nc \
    --arg slug "$SLUG" \
    --arg name "$DISPLAY_NAME" \
    --arg edition "$EDITION" \
    '{slug:$slug, display_name:$name, edition:$edition, email_domains:($ARGS.positional | map(.))}' \
    "${EMAIL_DOMAINS[@]}")

csu_log "creating tenant slug=$SLUG edition=$EDITION"
RESP=$(csu_json_request POST /api/internal/platform/tenants "$BODY")
STATUS=$(csu_status "$RESP")
BODY_RESP=$(csu_body "$RESP")
[[ "$STATUS" == "200" || "$STATUS" == "201" ]] || csu_die "POST failed (HTTP $STATUS): $BODY_RESP"
printf '%s\n' "$BODY_RESP" | csu_pretty
