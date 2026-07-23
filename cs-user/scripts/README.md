# cs-user ops scripts

Operational scripts for tenant onboarding and IdP configuration. All
scripts are bash + curl wrappers around the cs-user internal API
(`X-Internal-Token`-gated).

**Note**: git-server configuration lives on the `@server` side (the
`git_servers` + `tenant_git_server_binding` tables and their HTTP API are
owned by `server`). For git-server bootstrap see `server/scripts/`, not
here.

## Prerequisites

- `bash` ≥ 4 (macOS users: `brew install bash`)
- `curl`
- `jq`
- A running cs-user API binary with `CS_USER_INTERNAL_TOKEN` set
- A `.env` file in `cs-user/.env` (copy from `.env.example`) or equivalent
  exported env vars

These scripts are intended to be runnable both from the host and from
inside the cs-user container (the Dockerfile installs `curl` + `jq`).

## Environment

| Variable | Required | Default | Notes |
|---|---|---|---|
| `CS_USER_INTERNAL_TOKEN` | yes | — | Sent as `X-Internal-Token`. Must match the value cs-user was started with. |
| `CS_USER_BASE_URL` | no | `http://localhost:8081` | cs-user API origin. |
| `CS_USER_TENANT_ID` | no | — | Default tenant for scripts that don't take `--tenant`. |

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

## Typical onboarding sequence

```bash
# 1. Create the tenant.
./bootstrap-tenant.sh \
    --slug acme-corp \
    --display-name "Acme Corporation" \
    --edition enterprise \
    --email-domain acme.example.com

# 2. (server side) Bind the tenant to a Gitea server.
#    See server/scripts/bootstrap-git-server.sh for this step.

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

## Examples

The `examples/` directory contains template JSON / YAML files:

- `idtrust-idp.json` — IdP source config for an idtrust OAuth deployment
- `github-idp.json` — IdP source config for GitHub OAuth
- `idtrust-employment.yaml` — `tenant_config.config_yaml` with
  employment_providers (field_map + Plan B detection) and provider_mapping

Copy them out of the repo before filling in real secrets.
