# cs-user smoke

Manual / CI end-to-end probe for the identity-tenant integration surface.
**Not a unit test** — both services (cs-user and either @server or the Gitea
fork) must already be running.

## What it covers

| Layer | Coverage |
|---|---|
| `-layer=rpc` (default) | cs-user `/api/internal/ping` w/ `X-Internal-Token` • `/.well-known/jwks` advertises the kid derived from the signing key • `POST /api/internal/users/get-or-create` (intentionally **no** `X-Tenant-Id` — verifies default-tenant fallback) • @server accepts a cs-user-signed JWT (verifies the JWKS round-trip end-to-end) |
| `-layer=gitea` | Gitea fork `/api/internal/costrict/healthz` • pre-create `u-<name>` via Gitea admin API (simulates what cs-user `giteasync.Service` does) • Gitea authenticates a cs-user-signed JWT via its `CoStrictJWT` auth method |
| `-layer=all` | both, sequentially |

Exit code is 0 on all-green, 1 on any step failure, 2 on fatal config error.

## Env vars

| Var | Required | Notes |
|---|---|---|
| `CS_USER_URL` | yes | cs-user base URL (e.g. `http://localhost:8081`) |
| `CS_USER_INTERNAL_TOKEN` | yes | must match cs-user's `CS_USER_INTERNAL_TOKEN` |
| `CS_USER_JWT_SIGNING_KEY` | yes | path to PEM (PKCS#1 or PKCS#8) of the RSA key cs-user signs with — smoke loads + uses the same key so the kid matches |
| `CS_USER_JWT_ISSUER` | no | defaults to `cs-user`; must match Gitea's `[costrict] JWT_ISSUER` |
| `SERVER_URL` | rpc / all | @server base URL (e.g. `http://localhost:8080`). If unset, the @server step is skipped |
| `GITEA_URL` | gitea / all | Gitea fork base URL (e.g. `http://localhost:3000`) |
| `GITEA_ADMIN_USER` / `GITEA_ADMIN_PASS` | gitea / all | Gitea admin credentials for the user-pre-create step |

## Usage

### Layer 1 only (cs-user ↔ @server, fastest)

```bash
# Terminal 1: Postgres
docker compose up -d postgres

# Terminal 2: cs-user
cd cs-user
CS_USER_HTTP_PORT=8081 \
CS_USER_POSTGRES_USER=postgres \
CS_USER_POSTGRES_PASSWORD=xxx \
CS_USER_POSTGRES_DATABASE=cs_user \
CS_USER_INTERNAL_TOKEN=dev-internal-secret \
CS_USER_JWT_SIGNING_KEY_PATH=./testdata/jwt.pem \
CS_USER_AUTO_MIGRATE=1 \
go run ./cmd/api

# Terminal 3: @server
cd ../server
USER_SERVICE_BACKEND=rpc \
CS_USER_RPC_BASE_URL=http://localhost:8081 \
CS_USER_RPC_INTERNAL_TOKEN=dev-internal-secret \
CS_USER_JWT_JWKS_URL=http://localhost:8081/.well-known/jwks \
go run ./cmd/api

# Terminal 4: smoke
cd ../cs-user
CS_USER_URL=http://localhost:8081 \
CS_USER_INTERNAL_TOKEN=dev-internal-secret \
CS_USER_JWT_SIGNING_KEY=./testdata/jwt.pem \
SERVER_URL=http://localhost:8080 \
go run ./cmd/smoke -layer=rpc
```

### Layer 2 (cs-user → Gitea fork)

Requires the Gitea fork running locally with `[costrict]` configured. See
[`D:/DEV/gitea/docs/costrict.md`](../../../../D:/DEV/gitea/docs/costrict.md)
for app.ini setup.

```bash
CS_USER_URL=http://localhost:8081 \
CS_USER_INTERNAL_TOKEN=dev-internal-secret \
CS_USER_JWT_SIGNING_KEY=./testdata/jwt.pem \
GITEA_URL=http://localhost:3000 \
GITEA_ADMIN_USER=gitea-admin \
GITEA_ADMIN_PASS=xxx \
go run ./cmd/smoke -layer=gitea
```

## What it does NOT cover

- **Audit write verification**: requires DB access to read `audit_logs` rows.
  Out of scope for a CLI smoke; verify manually via `psql` after a write step.
- **Quota rejection**: deliberately disabled to avoid leaving test repos in
  Gitea. Run manually per [`gitea/docs/costrict.md` §1.1](../../../../D:/DEV/gitea/docs/costrict.md).
- **Cross-tenant isolation**: covered by `cs-user/internal/handlers/*_test.go`
  unit tests — not a smoke concern.

## Test key generation

If `./testdata/jwt.pem` doesn't exist:

```bash
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out cs-user/testdata/jwt.pem
# Public key is derived automatically; JWKS exposes it.
```
