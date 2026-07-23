# dev-env bootstrap scripts

One-shot orchestration for the local development stack. The frontend
(opencode/app-ai-native), server, cs-user, Casdoor, and Gitea all need to
agree on a tenant + git-server binding + idtrust employment mapping before
login → enterprise-identity sync works end-to-end.

`bootstrap-dev-env.sh` runs all four pieces in order. Each step is a thin
wrapper around a per-service script (idempotent — re-running is safe).

## Architecture (where OAuth creds live)

```
┌─────────────┐    OAuth    ┌──────────┐   callback   ┌────────┐
│   browser   │ ──────────▶ │ Casdoor  │ ───────────▶ │ server │
└─────────────┘             └──────────┘              └────┬───┘
                                 ▲                         │ JWT
                                 │                         ▼
                          ┌──────┴──────┐            ┌──────────┐
                          │  idtrust    │            │ cs-user  │
                          │  (provider) │            │ field_map│
                          └─────────────┘            └──────────┘
```

- **Casdoor** is the only OAuth entry point. Per-provider creds (idtrust
  `client_id` / `client_secret` / OAuth endpoints) live INSIDE Casdoor —
  this script does NOT collect or upload any provider credentials.
- **cs-user** stores per-tenant claim-mapping rules in
  `tenant_configs.config_yaml` (`employment_providers` section). Plan B
  detection picks the provider from the Casdoor JWT's `signupApplication`
  field; `field_map` then walks `properties.oauth_Custom.*` to extract
  enterprise columns.
- **server** binds the tenant to a Gitea instance and surfaces Casdoor
  env (`CASDOOR_ENDPOINT` / `CASDOOR_CLIENT_ID` / etc.) via `server/.env`.

## Prerequisites

| Component | Expected state |
|---|---|
| Casdoor | Running and fully configured (idtrust provider attached to your Casdoor Application, redirect URI pointing at server). |
| server | API up on `$SERVER_BASE_URL` (default localhost:8080), `INTERNAL_SECRET` set. |
| cs-user | API up on `$CS_USER_BASE_URL` (default localhost:8081), `CS_USER_INTERNAL_TOKEN` set. |
| Gitea | Up on `$DEFAULT_GITEA_ENDPOINT` (default http://127.0.0.1:3001), admin token available. |

Required env (source both `.env` files first):

```bash
# --- Copy-paste block: dev-env bootstrap env vars ---

# Pull secrets/endpoints already defined in the per-service .env files
# (CS_USER_INTERNAL_TOKEN, INTERNAL_SECRET, CASDOOR_*, etc.) so we don't
# duplicate them here. set -a makes every sourced var exported.
set -a
source cs-user/.env
source server/.env
set +a

# cs-user reachable URL. Default http://localhost:8081 — override only if
# you run cs-user on a different port/host.
export CS_USER_BASE_URL="http://localhost:8081"

# server reachable URL. Default http://localhost:8080.
export SERVER_BASE_URL="http://localhost:8080"

# Gitea reachable URL. Default http://127.0.0.1:3001.
export DEFAULT_GITEA_ENDPOINT="http://127.0.0.1:3001"

# default_tenant identity — most dev envs leave these at defaults.
# Override only if you want a different bootstrap tenant slug / display
# / edition. The slug doubles as the key into tenant_configs.config_yaml.
export DEFAULT_TENANT_SLUG="default"
export DEFAULT_TENANT_DISPLAY="Default Tenant"
# free | team | enterprise | on_premise — enterprise needed for IdP mapping
export DEFAULT_TENANT_EDITION="enterprise"

# cs-user X-Internal-Token. MUST match cs-user/.env's CS_USER_INTERNAL_TOKEN
# byte-for-byte — sent as `X-Internal-Token` header on every RPC call.
# (Picked up by the `source cs-user/.env` above; this line is a no-op
# reminder. Re-declare here only if you want to override.)
# export CS_USER_INTERNAL_TOKEN="dev-internal-secret-change-me"

# server X-Internal-Secret. MUST match server/.env's INTERNAL_SECRET
# byte-for-byte — sent as `X-Internal-Secret` header.
# (Picked up by the `source server/.env` above.)
# export INTERNAL_SECRET="dev-internal-secret-change-me"

# Gitea admin token. NOT in any .env file — generate one via Gitea UI
# (Profile → Settings → Applications → Generate New Token) and paste below.
export DEFAULT_GITEA_ADMIN_TOKEN="change-me-to-a-real-gitea-token"

# --- End copy-paste block ---
```

| Variable | Required | Source | Default | Notes |
|---|---|---|---|---|
| `CS_USER_INTERNAL_TOKEN` | yes | `cs-user/.env` | — | Sent as `X-Internal-Token`. 401 on mismatch. |
| `INTERNAL_SECRET` | yes | `server/.env` | — | Sent as `X-Internal-Secret`. 401 on mismatch. |
| `DEFAULT_GITEA_ADMIN_TOKEN` | yes | Gitea UI | — | Persisted into `git_servers.config.admin_token`. |
| `CS_USER_BASE_URL` | no | — | `http://localhost:8081` | Override on non-default port. |
| `SERVER_BASE_URL` | no | — | `http://localhost:8080` | Override on non-default port. |
| `DEFAULT_GITEA_ENDPOINT` | no | — | `http://127.0.0.1:3001` | Override for remote/non-default Gitea. |
| `DEFAULT_TENANT_SLUG` | no | — | `default` | Bootstrap tenant identity slug. |
| `DEFAULT_TENANT_DISPLAY` | no | — | `Default Tenant` | Bootstrap tenant display name. |
| `DEFAULT_TENANT_EDITION` | no | — | `enterprise` | `enterprise` needed for IdP mapping to engage. |

## Usage

```bash
./scripts/bootstrap-dev-env.sh \
    --gitea-display "Local Gitea (dev)" \
    --employment-yaml scripts/examples/idtrust-employment-dev.yaml
```

All tenant / Gitea-endpoint defaults come from the env block above; flags
override per-run. Plain `./scripts/bootstrap-dev-env.sh` works once the
env vars are exported.

All flags have sane defaults; plain `./scripts/bootstrap-dev-env.sh` works
once the env vars above are exported.

### What it does (4 steps)

1. **Default tenant** — `cs-user/scripts/bootstrap-tenant.sh` creates the
   tenant row (`tenants` table) for the bootstrapped slug.
2. **Gitea binding** — `server/scripts/bootstrap-git-server.sh` upserts a
   `git_servers` row + ties it to the tenant via
   `tenant_git_server_binding`.
3. **idtrust employment mapping** — `cs-user/scripts/configure-employment-mapping.sh`
   uploads `scripts/examples/idtrust-employment-dev.yaml` to
   `tenant_configs.config_yaml`. This carries:
   - `employment_providers.enabled: [idtrust]` — declares idtrust as a
     valid enterprise identity source for this tenant.
   - `employment_providers.provider_detection` — Plan B rule
     (`signup_application: "idtrust"` → `provider: idtrust`) so cs-user
     recognizes idtrust from the Casdoor JWT.
   - `employment_providers.field_map.idtrust` — dotted-path map from
     `properties.oauth_Custom.*` to internal enterprise columns.
   - `provider_mapping.idtrust` — rank + sync interval hints.
4. **Casdoor env sanity check** — verifies `server/.env` has
   `CASDOOR_ENDPOINT` / `CASDOOR_CLIENT_ID` / `CASDOOR_CLIENT_SECRET` /
   `CASDOOR_CALLBACK_URL` populated. Warns (does not fail) on missing or
   placeholder values.

### Useful flags

- `--skip-git-server` — skip step 2 when server isn't up yet.
- `--skip-idtrust` — skip step 3 (e.g. for a tenant without idtrust).
- `--dry-run` — print each sub-command without invoking it.

## Customizing the idtrust mapping

Edit `scripts/examples/idtrust-employment-dev.yaml` (or pass your own via
`--employment-yaml`). The template covers all 11 mappable enterprise
columns; commented entries mark fields idtrust typically does NOT return.
If your Casdoor Application `name` differs from `"idtrust"`, update the
`signup_application` value AND check
`server/internal/authidentity/normalize.go` — it has a hard-coded fallback
for `signupApplication == "idtrust"`.

For production deployments, copy `cs-user/scripts/examples/idtrust-employment.yaml`
out of the repo and edit there.
