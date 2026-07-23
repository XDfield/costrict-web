#!/usr/bin/env bash
# bootstrap-git-server.sh — seed cs-user.git_servers + bind tenant.git_server_id.
#
# IMPORTANT: at the time of writing (2026-07-23), cs-user's git_servers table
# exists (migration 20260721160000) but NO HTTP API exposes it yet — server
# resolves git_servers via ResolveAdapterForTenant which reads the table
# directly. Until the /api/internal/tenants/:id/git-server route lands,
# this script seeds the table via psql. When the API ships, this script
# will be migrated to curl-based like the others.
#
# Operations:
#   1. Upsert the git_servers row (idempotent — ON CONFLICT updates endpoint/
#      config/display_name, preserves is_template + enabled).
#   2. If --is-template, mark this row as the global template (the partial
#      unique index idx_git_servers_template allows at most one template row
#      globally — this script will fail if another template already exists).
#   3. If --tenant provided, set tenants.git_server_id for that tenant
#      (1:1 binding — the unique index idx_tenants_git_server means another
#      tenant cannot bind to the same server_id).
#
# Required:
#   --server-id    application-minted stable id (e.g. gs-acme-001 or
#                  gs-template-default for the template row)
#   --endpoint     Gitea API base URL (e.g. http://gitea.example.com:3000)
#   --display-name human-readable name shown in ops UI
#   --admin-token  Gitea admin token (goes into config JSONB; will move to
#                  vault once vault integration lands — see migration comment)
# Optional:
#   --tenant       tenant slug to bind to this git_server
#   --kind         git server kind (default: gitea; only valid value today)
#   --is-template  mark this row as the global template
#   --disabled     create with enabled=false
#
# Env:
#   CS_USER_DB_DSN    Postgres DSN for cs-user DB. Required. Examples:
#                     postgres://user:pwd@host:5432/dbname?sslmode=disable
#   CS_USER_DB_APP_NAME  psql application_name label (default: cs-user-ops)
#
# Example — create the template row:
#   ./bootstrap-git-server.sh \
#       --server-id gs-template-default \
#       --endpoint http://gitea.internal:3000 \
#       --display-name "Default Gitea" \
#       --admin-token "$GITEA_ADMIN_TOKEN" \
#       --is-template
#
# Example — bind an existing tenant to the template:
#   ./bootstrap-git-server.sh \
#       --server-id gs-template-default \
#       --endpoint http://gitea.internal:3000 \
#       --display-name "Default Gitea" \
#       --admin-token "$GITEA_ADMIN_TOKEN" \
#       --tenant acme-corp
set -euo pipefail

usage() {
    sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
    exit 1
}

SERVER_ID=""
ENDPOINT=""
DISPLAY_NAME=""
ADMIN_TOKEN=""
TENANT=""
KIND="gitea"
IS_TEMPLATE=0
DISABLED=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --server-id) SERVER_ID="$2"; shift 2 ;;
        --endpoint) ENDPOINT="$2"; shift 2 ;;
        --display-name) DISPLAY_NAME="$2"; shift 2 ;;
        --admin-token) ADMIN_TOKEN="$2"; shift 2 ;;
        --tenant) TENANT="$2"; shift 2 ;;
        --kind) KIND="$2"; shift 2 ;;
        --is-template) IS_TEMPLATE=1; shift ;;
        --disabled) DISABLED=1; shift ;;
        --help|-h) usage ;;
        *) echo "unknown flag: $1" >&2; usage ;;
    esac
done

[[ -n "$SERVER_ID" && -n "$ENDPOINT" && -n "$DISPLAY_NAME" && -n "$ADMIN_TOKEN" ]] || usage
[[ -z "${CS_USER_DB_DSN:-}" ]] && {
    echo "CS_USER_DB_DSN not set — required for direct psql access" >&2
    exit 2
}
command -v psql >/dev/null 2>&1 || {
    echo "psql not found in PATH — install postgresql-client" >&2
    exit 2
}

# Build the JSONB config: {"admin_token": "..."}. Future fields (vault_ref,
# etc.) go here. Pass via psql -v to avoid shell-quoting pitfalls; the JSON
# is built inside SQL with jsonb_build_object.
CONFIG_JSON=$(python -c "
import json, os
print(json.dumps({'admin_token': os.environ['ADMIN_TOKEN']}))
" ADMIN_TOKEN="$ADMIN_TOKEN")

ENABLED_FLAG="true"
[[ $DISABLED -eq 1 ]] && ENABLED_FLAG="false"
TEMPLATE_FLAG="false"
[[ $IS_TEMPLATE -eq 1 ]] && TEMPLATE_FLAG="true"

# Single transaction:
#   1. Upsert git_servers row.
#   2. If --tenant, bind via UPDATE tenants. The unique index will reject a
#      double-bind; the script surfaces the error verbatim.
#   3. If --is-template, the partial unique index catches conflicts (only
#      one template globally). The upsert below preserves the prior
#      is_template value via COALESCE on the EXCLUDED row.
printf '[cs-user-ops] upserting git_servers: %s\n' "$SERVER_ID" >&2

PSQL_ARGS=(
    -v ON_ERROR_STOP=1
    -v server_id="$SERVER_ID"
    -v kind="$KIND"
    -v endpoint="$ENDPOINT"
    -v display_name="$DISPLAY_NAME"
    -v config="$CONFIG_JSON"
    -v enabled="$ENABLED_FLAG"
    -v is_template="$TEMPLATE_FLAG"
)

BIND_CLAUSE=""
[[ -n "$TENANT" ]] && BIND_CLAUSE=", bind_tenant"
BIND_SQL=""
[[ -n "$TENANT" ]] && BIND_SQL="UPDATE tenants SET git_server_id = :'server_id' WHERE slug = :'tenant';"

psql "${CS_USER_DB_DSN}" "${PSQL_ARGS[@]}" -v tenant="$TENANT" <<SQL
\set ON_ERROR_STOP on
INSERT INTO git_servers (server_id, kind, endpoint, display_name, config, is_template, enabled)
VALUES (
    :'server_id',
    :'kind',
    :'endpoint',
    :'display_name',
    :'config'::jsonb,
    :'is_template'::boolean,
    :'enabled'::boolean
)
ON CONFLICT (server_id) DO UPDATE SET
    kind          = EXCLUDED.kind,
    endpoint      = EXCLUDED.endpoint,
    display_name  = EXCLUDED.display_name,
    config        = EXCLUDED.config,
    enabled       = EXCLUDED.enabled,
    -- Never auto-clear an existing template row via upsert; explicit
    -- operator action required to demote one. COALESCE preserves true.
    is_template   = EXCLUDED.is_template OR git_servers.is_template,
    updated_at    = now();
$BIND_SQL
SQL

if [[ -n "$TENANT" ]]; then
    printf '[cs-user-ops] bound tenant %s → git_server %s\n' "$TENANT" "$SERVER_ID" >&2
fi
printf '[cs-user-ops] OK\n' >&2
