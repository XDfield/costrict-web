#!/usr/bin/env bash
# bootstrap-git-server.sh — create or update a git_server row in @server,
# and optionally bind a tenant to it.
#
# Wraps three server HTTP endpoints (all internal-only, X-Internal-Secret
# gated):
#   POST   /api/internal/git-servers                    — create (server mints server_id)
#   PUT    /api/internal/git-servers/{server_id}        — update (partial)
#   PUT    /api/internal/tenants/{tenant_id}/git-server — upsert tenant binding
#
# Idempotency: this script upserts by `endpoint`. It calls
# GET /api/internal/git-servers, scans for a row whose `endpoint` matches
# --endpoint (normalized — trailing slash stripped, compared case-sensitively).
# If found, it PUTs the new fields onto that server_id; otherwise it POSTs
# to create a new row. Either path then prints the resulting server_id
# (which you can pin via --server-id to skip the lookup and update directly).
#
# Required:
#   --endpoint       Gitea API base URL (e.g. http://gitea.example.com:3000)
#   --display-name   human-readable name shown in ops UI
#   --admin-token    Gitea admin token (goes into config JSONB; will move
#                    to vault once vault integration lands)
# Optional:
#   --server-id      pin the target row by server_id (skip endpoint lookup).
#                    When supplied for a non-existent row, the script errors
#                    out — it does NOT POST a new row with that id, because
#                    the server's POST handler mints server_id itself.
#   --tenant         tenant id/slug to bind to this git_server (1:1).
#   --kind           git server kind (default: gitea; only valid value today)
#   --disabled       create/update with enabled=false
#
# Env (see lib/common.sh): INTERNAL_SECRET, SERVER_BASE_URL, DEFAULT_TENANT_ID.
#
# Example — create + bind in one shot:
#   ./bootstrap-git-server.sh \
#       --endpoint http://gitea.internal:3000 \
#       --display-name "Default Gitea" \
#       --admin-token "$GITEA_ADMIN_TOKEN" \
#       --tenant acme-corp
#
# Example — update an existing row by pinned server_id (no lookup):
#   ./bootstrap-git-server.sh \
#       --server-id gs-1234 \
#       --endpoint http://gitea.internal:3000 \
#       --display-name "Default Gitea (new name)" \
#       --admin-token "$GITEA_ADMIN_TOKEN"
set -euo pipefail

usage() {
    sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
    exit 1
}

ENDPOINT=""
DISPLAY_NAME=""
ADMIN_TOKEN=""
SERVER_ID=""
TENANT_FLAG=""
KIND="gitea"
DISABLED=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --endpoint) ENDPOINT="$2"; shift 2 ;;
        --display-name) DISPLAY_NAME="$2"; shift 2 ;;
        --admin-token) ADMIN_TOKEN="$2"; shift 2 ;;
        --server-id) SERVER_ID="$2"; shift 2 ;;
        --tenant) TENANT_FLAG="$2"; shift 2 ;;
        --kind) KIND="$2"; shift 2 ;;
        --disabled) DISABLED=1; shift ;;
        --help|-h) usage ;;
        *) echo "unknown flag: $1" >&2; usage ;;
    esac
done

[[ -n "$ENDPOINT" && -n "$DISPLAY_NAME" && -n "$ADMIN_TOKEN" ]] || usage

# shellcheck source=lib/common.sh
source "$(dirname "$0")/lib/common.sh"

# Build the config JSONB {"admin_token": "..."}. jq validates + quotes.
CONFIG_JSON=$(jq -nc --arg t "$ADMIN_TOKEN" '{admin_token: $t}')

# Normalize endpoint: server's POST strips trailing slash; mirror that here
# for the lookup comparison.
ENDPOINT_NORM="${ENDPOINT%/}"

# Step 1: resolve target server_id (use pinned id, else lookup by endpoint).
if [[ -z "$SERVER_ID" ]]; then
    srv_log "listing git_servers to find existing row by endpoint=$ENDPOINT_NORM"
    LIST_OUT=$(srv_get /api/internal/git-servers)
    SERVER_ID=$(printf '%s' "$LIST_OUT" \
        | jq -r --arg ep "$ENDPOINT_NORM" \
            '.[] | select(.endpoint == $ep) | .server_id' \
            2>/dev/null | head -n1 || true)
fi

# Step 2: POST (create) or PUT (update).
ENABLED_FIELD=""
[[ $DISABLED -eq 1 ]] && ENABLED_FIELD=',"enabled":false'

if [[ -z "$SERVER_ID" ]]; then
    # No existing row → create. POST body schema requires kind/endpoint/
    # display_name; config + enabled are optional.
    BODY=$(jq -nc \
        --arg k "$KIND" \
        --arg e "$ENDPOINT_NORM" \
        --arg d "$DISPLAY_NAME" \
        --argjson cfg "$CONFIG_JSON" \
        "{kind: \$k, endpoint: \$e, display_name: \$d, config: \$cfg${ENABLED_FIELD}}")
    srv_log "POSTing new git_server kind=$KIND endpoint=$ENDPOINT_NORM"
    OUT=$(srv_json_request POST /api/internal/git-servers "$BODY")
    STATUS=$(srv_status "$OUT")
    BODY_RESP=$(srv_body "$OUT")
    [[ "$STATUS" == "201" ]] || srv_die "POST failed (HTTP $STATUS): $BODY_RESP"
    SERVER_ID=$(printf '%s' "$BODY_RESP" | jq -r '.server_id')
else
    # Existing row → PUT update. All fields optional server-side.
    BODY=$(jq -nc \
        --arg e "$ENDPOINT_NORM" \
        --arg d "$DISPLAY_NAME" \
        --argjson cfg "$CONFIG_JSON" \
        "{endpoint: \$e, display_name: \$d, config: \$cfg${ENABLED_FIELD}}")
    srv_log "PUTting update to git_server server_id=$SERVER_ID"
    OUT=$(srv_json_request PUT "/api/internal/git-servers/${SERVER_ID}" "$BODY")
    STATUS=$(srv_status "$OUT")
    BODY_RESP=$(srv_body "$OUT")
    [[ "$STATUS" == "200" ]] || srv_die "PUT failed (HTTP $STATUS): $BODY_RESP"
fi

printf 'server_id=%s\n' "$SERVER_ID" >&2
printf '%s\n' "$BODY_RESP" | srv_pretty

# Step 3: optional tenant binding.
if [[ -n "$TENANT_FLAG" ]]; then
    TENANT=$(srv_resolve_tenant "$TENANT_FLAG")
    BIND_BODY=$(jq -nc --arg id "$SERVER_ID" '{git_server_id: $id}')
    srv_log "binding tenant=$TENANT → git_server=$SERVER_ID"
    OUT=$(srv_json_request PUT "/api/internal/tenants/${TENANT}/git-server" "$BIND_BODY")
    STATUS=$(srv_status "$OUT")
    BODY_RESP=$(srv_body "$OUT")
    [[ "$STATUS" == "200" ]] || srv_die "bind failed (HTTP $STATUS): $BODY_RESP"
    printf '%s\n' "$BODY_RESP" | srv_pretty
fi

srv_log "OK"
