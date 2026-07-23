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
| Gitea | Up on `$GITEA_ENDPOINT` (default http://127.0.0.1:3000), admin token available. |

Required env (source both `.env` files first):

```bash
set -a
source cs-user/.env
source server/.env
set +a
export GITEA_ADMIN_TOKEN=...   # Gitea admin token
```

## Usage

```bash
./scripts/bootstrap-dev-env.sh \
    --tenant default \
    --tenant-display "Default Tenant" \
    --tenant-edition enterprise \
    --gitea-endpoint http://127.0.0.1:3000 \
    --gitea-display "Local Gitea (dev)" \
    --employment-yaml scripts/examples/idtrust-employment-dev.yaml
```

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
