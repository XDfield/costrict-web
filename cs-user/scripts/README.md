# cs-user ops scripts

Operational scripts for tenant onboarding and IdP / git-server configuration.
All scripts are bash + curl wrappers around the cs-user internal API (and,
for git-server which doesn't have an API yet, direct psql).

## Prerequisites

- `bash` ≥ 4 (macOS users: `brew install bash`)
- `curl`
- `python` ≥ 3.6 (used for safe JSON building + pretty-printing; `jq` works
  too if installed)
- `psql` (only for `bootstrap-git-server.sh`)
- A running cs-user API binary with `CS_USER_INTERNAL_TOKEN` set
- A `.env` file in `cs-user/.env` (copy from `.env.example`) or equivalent
  exported env vars

## Environment

| Variable | Required | Default | Notes |
|---|---|---|---|
| `CS_USER_INTERNAL_TOKEN` | yes | — | Sent as `X-Internal-Token`. Must match the value cs-user was started with. |
| `CS_USER_BASE_URL` | no | `http://localhost:8081` | cs-user API origin. |
| `CS_USER_TENANT_ID` | no | — | Default tenant for scripts that don't take `--tenant`. |
| `CS_USER_DB_DSN` | only for git-server script | — | Postgres DSN, used directly via `psql`. |

Source your `.env` before running:

```bash
set -a; source ../.env; set +a
```

## Scripts

| Script | Purpose | Wraps |
|---|---|---|
| `bootstrap-tenant.sh` | Create a new tenant | `POST /api/internal/platform/tenants` |
| `configure-idp-source.sh` | Upsert an IdP source | `PUT` then `POST /api/idp-sources` |
| `list-idps.sh` | List IdPs (all / enabled-only) | `GET /api/idp-sources/{tenant}` |
| `delete-idp.sh` | Remove an IdP source (interactive confirm) | `DELETE /api/idp-sources/{tenant}/{provider}` |
| `configure-employment-mapping.sh` | Replace `tenant_configs.config_yaml` | `PUT /api/internal/tenant/config` |
| `bootstrap-git-server.sh` | Seed `git_servers` + bind tenant (psql-direct) | DB (no API yet) |

## Typical onboarding sequence

```bash
# 1. Create the tenant.
./bootstrap-tenant.sh \
    --slug acme-corp \
    --display-name "Acme Corporation" \
    --edition enterprise \
    --email-domain acme.example.com

# 2. Bind the tenant to a Gitea server (uses the template row created by
#    bootstrap-git-server.sh --is-template in a prior step).
./bootstrap-git-server.sh \
    --server-id gs-template-default \
    --endpoint http://gitea.internal:3000 \
    --display-name "Default Gitea" \
    --admin-token "$GITEA_ADMIN_TOKEN" \
    --tenant acme-corp

# 3. Upload employment_providers config (field_map + Plan B detection).
./configure-employment-mapping.sh \
    --tenant acme-corp \
    --yaml examples/idtrust-employment.yaml

# 4. Register the IdP source (client credentials, endpoints).
./configure-idp-source.sh \
    --tenant acme-corp \
    --provider idtrust \
    --config-json examples/idtrust-idp.json

# 5. Verify.
./list-idps.sh --tenant acme-corp
```

## Why some scripts use psql instead of curl

cs-user's `git_servers` table landed in Phase E3b.1.1 (migration
`20260721160000`), but the HTTP handler `/api/internal/tenants/:id/git-server`
referenced in `.env.example` has **not been implemented yet**. Until that
lands, `bootstrap-git-server.sh` seeds the table directly via `psql`. When
the API ships, that script will switch to curl like the others — leaving
the script name and flags unchanged.

## Examples

The `examples/` directory contains template JSON / YAML files:

- `idtrust-idp.json` — IdP source config for an idtrust OAuth deployment
- `github-idp.json` — IdP source config for GitHub OAuth
- `idtrust-employment.yaml` — `tenant_configs.config_yaml` with
  employment_providers (field_map + Plan B detection) and provider_mapping

Copy them out of the repo before filling in real secrets.
