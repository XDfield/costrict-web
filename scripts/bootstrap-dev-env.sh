#!/usr/bin/env bash
# bootstrap-dev-env.sh — one-shot dev environment bootstrap.
#
# Wires up a runnable local stack with the four pieces every developer
# needs before they can log in via Casdoor (with enterprise IdP brokered
# behind it) and push to a per-user Gitea account:
#
#   1. Default tenant row in cs-user.tenants.
#   2. git_server row in server.git_servers + tenant_git_server_binding
#      tying the default tenant to a local Gitea instance.
#   3. idtrust employment_providers config_yaml written to cs-user
#      .tenantConfigs (Plan B detection + field_map — claim mapping,
#      NOT OAuth credentials).
#   4. Sanity check that server's Casdoor env is populated (Casdoor is
#      configured via static env, NOT a runtime API — this step only
#      warns if CASDOOR_* vars are missing in server/.env).
#
# OAuth is brokered exclusively by Casdoor. Per-provider OAuth creds
# (idtrust client_id/secret, ...) live inside Casdoor — this script
# deliberately does NOT collect or upload any provider credentials.
#
# This is a thin orchestration layer. The actual work is delegated to
# the per-service scripts (cs-user/scripts/*, server/scripts/*) which
# already encode the idempotent upsert logic. Re-running this script
# is safe — each step is a PUT-or-upsert.
#
# =====================================================================
# Prerequisites (run BEFORE this script):
# =====================================================================
#
#   * cs-user API running on $CS_USER_BASE_URL (default localhost:8082)
#     with $CS_USER_INTERNAL_TOKEN set.
#   * server API running on $SERVER_BASE_URL (default localhost:8080)
#     with $INTERNAL_SECRET set.
#   * Gitea running on $DEFAULT_GITEA_ENDPOINT (default http://127.0.0.1:3001)
#     with an admin token available.
#
# Required env vars (copy-paste block — run before invoking this script):
#
#     # --- BEGIN dev-env bootstrap env ---
#     # Source per-service .env files so CS_USER_INTERNAL_TOKEN /
#     # INTERNAL_SECRET / CASDOOR_* etc. flow through unchanged.
#     set -a
#     source cs-user/.env
#     source server/.env
#     set +a
#
#     # cs-user reachable URL (default http://localhost:8082)
#     export CS_USER_BASE_URL="http://localhost:8082"
#     # server reachable URL (default http://localhost:8080)
#     export SERVER_BASE_URL="http://localhost:8080"
#
#     # Gitea reachable URL (default http://127.0.0.1:3001)
#     export DEFAULT_GITEA_ENDPOINT="http://127.0.0.1:3001"
#
#     # default_tenant identity — most dev envs leave these at defaults.
#     # Override only if you want a different bootstrap tenant slug / display
#     # / edition. The slug doubles as the key into tenant_configs.config_yaml.
#     export DEFAULT_TENANT_SLUG="default"
#     export DEFAULT_TENANT_DISPLAY="Default Tenant"
#     # free | team | enterprise | on_premise — enterprise needed for IdP mapping
#     export DEFAULT_TENANT_EDITION="enterprise"
#
#     # CS_USER_INTERNAL_TOKEN / INTERNAL_SECRET are picked up by the
#     # `source` lines above; they MUST byte-match the values each service
#     # was started with. Re-declare here only if you want to override:
#     # export CS_USER_INTERNAL_TOKEN="dev-internal-secret-change-me"
#     # export INTERNAL_SECRET="dev-internal-secret-change-me"
#
#     # Gitea admin token — NOT in any .env file; generate one in Gitea UI
#     # (Profile → Settings → Applications → Generate New Token).
#     export DEFAULT_GITEA_ADMIN_TOKEN="change-me-to-a-real-gitea-token"
#     # --- END dev-env bootstrap env ---
#
#     ./scripts/bootstrap-dev-env.sh
#
# =====================================================================
# Flags
# =====================================================================
#
#   --tenant                default tenant slug (default: "default", env:
#                           DEFAULT_TENANT_SLUG)
#   --tenant-display        display name (default: "Default Tenant", env:
#                           DEFAULT_TENANT_DISPLAY)
#   --tenant-edition        free | team | enterprise | on_premise
#                           (default: enterprise — needed for IdP mapping;
#                           env: DEFAULT_TENANT_EDITION)
#   --gitea-endpoint        Gitea base URL (default: http://127.0.0.1:3001,
#                           overridable via $DEFAULT_GITEA_ENDPOINT env var)
#   --gitea-display         display name (default: "Local Gitea (dev)")
#   --employment-yaml       path to employment mapping YAML (default:
#                           scripts/examples/idtrust-employment-dev.yaml)
#   --skip-git-server       skip the server-side git_server step (e.g.
#                           when running before server is up)
#   --skip-idtrust          skip the employment mapping step
#   --update-if-exists      step 1 default is create-or-skip; pass this to
#                           switch to create-or-update (PATCH mutable fields
#                           on an existing tenant)
#   --dry-run               print what would run without invoking sub-scripts
#
# Required env:
#   CS_USER_INTERNAL_TOKEN    — cs-user X-Internal-Token (from cs-user/.env)
#   INTERNAL_SECRET           — server X-Internal-Secret (from server/.env)
#   DEFAULT_GITEA_ADMIN_TOKEN — Gitea admin token (generate via Gitea UI)
#
# Optional env (sane defaults if unset):
#   CS_USER_BASE_URL        — defaults to http://localhost:8082
#   SERVER_BASE_URL         — defaults to http://localhost:8080
#   DEFAULT_TENANT_SLUG     — default-tenant identity slug (defaults to
#                             "default"). Conceptually the "default_tenant"
#                             bootstrap row — most dev envs leave this as-is.
#   DEFAULT_TENANT_DISPLAY  — display name (defaults to "Default Tenant")
#   DEFAULT_TENANT_EDITION  — free | team | enterprise | on_premise
#                             (defaults to "enterprise" — needed for IdP
#                             mapping to engage)
#   DEFAULT_GITEA_ENDPOINT  — defaults to http://127.0.0.1:3001
#
# =====================================================================

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CS_USER_SCRIPTS="$REPO_ROOT/cs-user/scripts"
SERVER_SCRIPTS="$REPO_ROOT/server/scripts"

# ---- defaults -------------------------------------------------------------
TENANT_SLUG="${DEFAULT_TENANT_SLUG:-default}"
TENANT_DISPLAY="${DEFAULT_TENANT_DISPLAY:-Default Tenant}"
TENANT_EDITION="${DEFAULT_TENANT_EDITION:-enterprise}"
DEFAULT_GITEA_ENDPOINT="${DEFAULT_GITEA_ENDPOINT:-http://127.0.0.1:3001}"
GITEA_DISPLAY="Local Gitea (dev)"
EMPLOYMENT_YAML="$REPO_ROOT/scripts/examples/idtrust-employment-dev.yaml"
SKIP_GIT_SERVER=0
SKIP_IDTRUST=0
UPDATE_IF_EXISTS=0
DRY_RUN=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --tenant)            TENANT_SLUG="$2"; shift 2 ;;
        --tenant-display)    TENANT_DISPLAY="$2"; shift 2 ;;
        --tenant-edition)    TENANT_EDITION="$2"; shift 2 ;;
        --gitea-endpoint)    DEFAULT_GITEA_ENDPOINT="$2"; shift 2 ;;
        --gitea-display)     GITEA_DISPLAY="$2"; shift 2 ;;
        --employment-yaml)   EMPLOYMENT_YAML="$2"; shift 2 ;;
        --skip-git-server)   SKIP_GIT_SERVER=1; shift ;;
        --skip-idtrust)      SKIP_IDTRUST=1; shift ;;
        --update-if-exists)  UPDATE_IF_EXISTS=1; shift ;;
        --dry-run)           DRY_RUN=1; shift ;;
        --help|-h)
            sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *) echo "unknown flag: $1" >&2; exit 1 ;;
    esac
done

log()  { printf '[bootstrap-dev-env] %s\n' "$*" >&2; }
warn() { printf '[bootstrap-dev-env] WARN: %s\n' "$*" >&2; }
die()  { printf '[bootstrap-dev-env] ERROR: %s\n' "$*" >&2; exit 1; }

# ---- env presence checks --------------------------------------------------
[[ -n "${CS_USER_INTERNAL_TOKEN:-}" ]] || die "CS_USER_INTERNAL_TOKEN not set — source cs-user/.env"
[[ -f "$CS_USER_SCRIPTS/bootstrap-tenant.sh" ]] || die "missing $CS_USER_SCRIPTS/bootstrap-tenant.sh"
[[ -f "$CS_USER_SCRIPTS/configure-employment-mapping.sh" ]] || die "missing configure-employment-mapping.sh"

if [[ $SKIP_GIT_SERVER -eq 0 ]]; then
    [[ -n "${INTERNAL_SECRET:-}" ]] || die "INTERNAL_SECRET not set — source server/.env"
    [[ -n "${DEFAULT_GITEA_ADMIN_TOKEN:-}" ]] || die "DEFAULT_GITEA_ADMIN_TOKEN not set"
    [[ -f "$SERVER_SCRIPTS/bootstrap-git-server.sh" ]] || die "missing server/scripts/bootstrap-git-server.sh"
fi

if [[ $SKIP_IDTRUST -eq 0 ]]; then
    [[ -f "$EMPLOYMENT_YAML" ]] || die "employment yaml not found: $EMPLOYMENT_YAML"
fi

# Run a sub-script (or just echo it under --dry-run). Args: cmd-array.
run() {
    if [[ $DRY_RUN -eq 1 ]]; then
        printf '[dry-run] %s\n' "$*" >&2
    else
        "$@"
    fi
}

# ===========================================================================
# Step 1 — Default tenant
# ===========================================================================
log "step 1/4: create tenant slug=$TENANT_SLUG edition=$TENANT_EDITION"
TENANT_ARGS=(
    --slug "$TENANT_SLUG"
    --display-name "$TENANT_DISPLAY"
    --edition "$TENANT_EDITION"
)
[[ $UPDATE_IF_EXISTS -eq 1 ]] && TENANT_ARGS+=(--update-if-exists)
run "$CS_USER_SCRIPTS/bootstrap-tenant.sh" "${TENANT_ARGS[@]}"

# ===========================================================================
# Step 2 — git_server + tenant binding (server side)
# ===========================================================================
if [[ $SKIP_GIT_SERVER -eq 1 ]]; then
    log "step 2/4: SKIPPED (--skip-git-server)"
else
    log "step 2/4: upsert git_server endpoint=$DEFAULT_GITEA_ENDPOINT + bind tenant=$TENANT_SLUG"
    run "$SERVER_SCRIPTS/bootstrap-git-server.sh" \
        --endpoint "$DEFAULT_GITEA_ENDPOINT" \
        --display-name "$GITEA_DISPLAY" \
        --admin-token "$DEFAULT_GITEA_ADMIN_TOKEN" \
        --tenant "$TENANT_SLUG"
fi

# ===========================================================================
# Step 3 — idtrust employment_providers + provider_mapping YAML
# ===========================================================================
if [[ $SKIP_IDTRUST -eq 1 ]]; then
    log "step 3/4: SKIPPED (--skip-idtrust)"
else
    log "step 3/4: upload employment mapping yaml=$EMPLOYMENT_YAML"
    run "$CS_USER_SCRIPTS/configure-employment-mapping.sh" \
        --tenant "$TENANT_SLUG" \
        --yaml "$EMPLOYMENT_YAML"
fi

# ===========================================================================
# Step 4 — Casdoor env sanity check (server/.env)
# ===========================================================================
log "step 4/4: verify Casdoor env in server/.env"
SERVER_ENV="$REPO_ROOT/server/.env"
if [[ ! -f "$SERVER_ENV" ]]; then
    warn "server/.env not found at $SERVER_ENV — skipping Casdoor check"
    warn "Casdoor cannot be configured via runtime API; populate server/.env before starting @server:"
    warn "  CASDOOR_ENDPOINT, CASDOOR_CLIENT_ID, CASDOOR_CLIENT_SECRET, CASDOOR_CALLBACK_URL"
else
    missing=()
    for var in CASDOOR_ENDPOINT CASDOOR_CLIENT_ID CASDOOR_CLIENT_SECRET CASDOOR_CALLBACK_URL; do
        val=$(grep -E "^${var}=" "$SERVER_ENV" 2>/dev/null | head -n1 | cut -d= -f2- | tr -d '"'\''[:space:]' || true)
        if [[ -z "$val" || "$val" == your-client-id || "$val" == your-client-secret ]]; then
            missing+=("$var")
        fi
    done
    if [[ ${#missing[@]} -gt 0 ]]; then
        warn "server/.env has missing/placeholder Casdoor vars: ${missing[*]}"
        warn "Login via Casdoor will fail until these are populated."
    else
        log "  server/.env Casdoor vars look populated"
    fi
fi

# ===========================================================================
log "DONE — dev environment bootstrap complete"
if [[ $DRY_RUN -eq 1 ]]; then
    log "(dry-run: no API calls were made)"
fi
