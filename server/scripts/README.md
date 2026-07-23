# server ops scripts

Operational scripts for `@server` configuration that lives outside the
HTTP user surface — git-server bootstrap, tenant binding, etc. All scripts
are bash + curl wrappers around the server's internal-only API
(`/api/internal/*`), authenticated via `X-Internal-Secret`.

## Prerequisites

- `bash` ≥ 4 (macOS users: `brew install bash`)
- `curl`
- `jq`
- A running server binary with `INTERNAL_SECRET` set
- A `.env` file in `server/.env` (copy from `.env.example`) or equivalent
  exported env vars

These scripts are intended to be runnable both from the host and from
inside the server container (the Dockerfile installs `curl` + `jq`).

## Environment

| Variable | Required | Default | Notes |
|---|---|---|---|
| `INTERNAL_SECRET` | yes | — | Sent as `X-Internal-Secret`. Must match the value the server was started with. |
| `SERVER_BASE_URL` | no | `http://localhost:8080` | server API origin. |
| `DEFAULT_TENANT_ID` | no | — | Default tenant for scripts that don't take `--tenant`. |

Source your `.env` before running:

```bash
set -a; source ../.env; set +a
```

## Scripts

| Script | Purpose | Wraps |
|---|---|---|
| `bootstrap-git-server.sh` | Upsert a git_server row + optional tenant bind | `POST`/`PUT /api/internal/git-servers`, `PUT /api/internal/tenants/:id/git-server` |

## Typical onboarding sequence

```bash
# 1. Create the git_server row. Server mints its own server_id (gs-<uuid>);
#    the script prints it back. To run idempotently on the same endpoint,
#    the script first lists existing rows and PUTs updates if a matching
#    endpoint is found.
./bootstrap-git-server.sh \
    --endpoint http://gitea.internal:3000 \
    --display-name "Default Gitea" \
    --admin-token "$GITEA_ADMIN_TOKEN" \
    --tenant acme-corp
```

## Notes

- The server's `POST /api/internal/git-servers` handler mints `server_id`
  itself (format `gs-<uuid>`); the API does not accept an externally
  provided id. To update an existing row by id, pass `--server-id`.
- `is_template` is not exposed via HTTP today — every row created here
  starts with `is_template=false`. If you need a template row, edit the
  DB directly until the API ships that surface.
- The `config` JSONB is opaque to the server; this script writes
  `{"admin_token": "<token>"}` into it. When vault integration lands the
  schema will grow, but the script flag (`--admin-token`) stays the same.
