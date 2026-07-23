# cs-user ops scripts

Operational scripts for tenant onboarding and employment-mapping upload.
All scripts are bash + curl wrappers around the cs-user internal API
(`X-Internal-Token`-gated).

**Note**: OAuth / IdP configuration is brokered exclusively by Casdoor.
cs-user does NOT store per-provider OAuth credentials — provider-specific
claim mapping is configured via `employment_providers` (Plan B detection
+ `field_map`) under tenant_configs. For git-server bootstrap (server-side
concern) see `server/scripts/`.

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
| `configure-employment-mapping.sh` | Replace `tenant_configs.config_yaml` (employment_providers + provider_mapping) | `PUT /api/internal/tenant/config` |

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
#    This is how cs-user learns to map idtrust claims from a Casdoor JWT.
./configure-employment-mapping.sh \
    --tenant acme-corp \
    --yaml examples/idtrust-employment.yaml

# 4. (server side) Configure Casdoor as the OAuth entry point.
#    Per-provider OAuth creds live in Casdoor, not here. See server/.env
#    for CASDOOR_* fields.
```

## Examples

The `examples/` directory contains template YAML files:

- `idtrust-employment.yaml` — `tenant_config.config_yaml` with
  employment_providers (field_map + Plan B detection) and provider_mapping

This is claim-mapping config (no secrets). Copy out of the repo before
editing for your deployment.
