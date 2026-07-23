#!/usr/bin/env bash
# bootstrap-tenant.sh — create a new tenant in cs-user.
#
# Wraps POST /api/internal/platform/tenants. The platform API mints a UUID
# tenant_id server-side; the tenant slug is the stable human-facing id used
# by all subsequent scripts.
#
# Required:
#   --slug         stable lowercase slug (matches ^[a-z0-9-]{1,64}$)
#   --display-name human-readable name
# Optional:
#   --edition      community | standard | enterprise (default: community)
#   --email-domain email domain to auto-bind to this tenant (repeatable)
#   --config-yaml  path to a YAML file written to tenant_configs.config_yaml
#                  via the follow-up configure-tenant-config.sh script
#                  (this script only creates the tenant row; it does NOT
#                  seed tenant_configs — use configure-tenant-config.sh)
#
# Env (see lib/common.sh): CS_USER_INTERNAL_TOKEN, CS_USER_BASE_URL.
#
# Example:
#   ./bootstrap-tenant.sh \
#       --slug acme-corp \
#       --display-name "Acme Corporation" \
#       --edition enterprise \
#       --email-domain acme.example.com
set -euo pipefail

usage() {
    sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
    exit 1
}

SLUG=""
DISPLAY_NAME=""
EDITION="community"
EMAIL_DOMAINS=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        --slug) SLUG="$2"; shift 2 ;;
        --display-name) DISPLAY_NAME="$2"; shift 2 ;;
        --edition) EDITION="$2"; shift 2 ;;
        --email-domain) EMAIL_DOMAINS+=("$2"); shift 2 ;;
        --help|-h) usage ;;
        *) echo "unknown flag: $1" >&2; usage ;;
    esac
done

[[ -n "$SLUG" && -n "$DISPLAY_NAME" ]] || usage

# shellcheck source=lib/common.sh
source "$(dirname "$0")/lib/common.sh"

# Build payload via jq. Email domains are passed as positional args
# ($ARGS.positional) so any number of --email-domain flags are picked
# up uniformly without per-element jq --arg bindings.
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
