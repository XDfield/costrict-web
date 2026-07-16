# cs-user

User identity service for the costrict-cloud platform.

**Phase 1 status**: skeleton (ADR [`docs/identity-tenant/ADR_CS_USER_PHASE1_DECISIONS.md`](../docs/identity-tenant/ADR_CS_USER_PHASE1_DECISIONS.md)).

## Scope (Phase 1)

- User data ownership: `users` / `user_auth_identities` CRUD (independent PostgreSQL)
- REST only (no gRPC — deferred per ADR D5)
- Auth: shared-secret `X-Internal-Token` header for `/api/internal/*` (ADR D8)
- Read-through RPC consumed by `costrict-web/server` (CachedUserService reused)

**Out of scope**: JWT self-signing, OAuth callback, `employment_identities`, `tenant_configs`, webhook.

## Local development

```bash
# 1. Set required env vars (shared secret + DB creds)
export CS_USER_INTERNAL_TOKEN=dev-shared-secret-change-me
export CS_USER_POSTGRES_USER=postgres
export CS_USER_POSTGRES_PASSWORD=postgres

# 2a. Standalone migration (recommended — explicit, no auto-magic)
make migrate

# 2b. Or run the API with auto-migrate enabled in dev
export CS_USER_AUTO_MIGRATE=1
go run ./cmd/api

# 3. Health check (unauthenticated)
curl http://localhost:8081/healthz
curl http://localhost:8081/readyz

# 4. Internal route (requires shared secret)
curl -H "X-Internal-Token: dev-shared-secret-change-me" http://localhost:8081/api/internal/ping

# 5. Swagger UI (regenerate first — see "API documentation" below)
make swagger
# then open http://localhost:8081/swagger/index.html
```

## Configuration

All config via env vars (prefix `CS_USER_`):

| Var | Default | Notes |
|---|---|---|
| `CS_USER_HTTP_PORT` | `8081` | |
| `CS_USER_HTTP_MODE` | `debug` | gin mode |
| `CS_USER_POSTGRES_HOST` | `localhost` | |
| `CS_USER_POSTGRES_PORT` | `5432` | |
| `CS_USER_POSTGRES_DATABASE` | `cs_user` | |
| `CS_USER_POSTGRES_USER` | — | **required** |
| `CS_USER_POSTGRES_PASSWORD` | — | **required** |
| `CS_USER_POSTGRES_SSLMODE` | `disable` | |
| `CS_USER_INTERNAL_TOKEN` | — | **required** — shared secret for costrict-web calls |
| `CS_USER_AUTO_MIGRATE` | unset | When truthy (`1`/`true`/`yes`), API binary runs pending migrations on boot. **Dev only** — prod uses the standalone `cs-user-migrate` binary (Helm pre-deploy hook). |
| `DB_MAX_OPEN_CONNS` | `25` | gorm pool limit |
| `DB_MAX_IDLE_CONNS` | `5` | gorm idle pool limit |
| `DB_CONN_MAX_LIFETIME_MINUTES` | `60` | gorm connection max age |

## API documentation

OpenAPI / Swagger spec is generated from inline annotations via [swaggo](https://github.com/swaggo/swag) (same toolchain as `server/`). The generated artifacts live under `docs/` and are committed to the repo so the spec ships with the binary.

```bash
# Regenerate spec after editing any @Router / @Summary / @Param annotation
make swagger

# Format annotation columns (run before pushing annotation changes)
make swagger-check
```

When you run the service, the Swagger UI is served at `http://localhost:8081/swagger/index.html`. The blank import `_ "github.com/costrict/costrict-web/cs-user/docs"` in `cmd/api/main.go` triggers `swag.Register` at process start — without it the UI loads but shows an empty spec.

Annotation convention (matches `server/`):

- Package-level annotations (`@title`, `@version`, `@BasePath`, `@securityDefinitions.apikey`) live at the top of `cmd/api/main.go`.
- Handler annotations (`@Summary`, `@Router`, `@Param`, `@Success`, etc.) live directly above each handler function. Anonymous closures cannot be annotated — extract them to named functions first (see `PingHandler`).
- `@Security InternalToken` is applied only on `/api/internal/*` routes; `/healthz` and `/readyz` are intentionally unauthenticated.

## Build

```bash
docker build -f Dockerfile -t cs-user:dev .
```

## Testing

```bash
# Full local gate (fmt + vet + race tests) — run before pushing
make check

# Individual targets
make test              # go test ./...
make test-race         # with -race
make test-coverage     # writes coverage.out
make vet
make fmt
```

CI runs automatically on every PR via [`.github/workflows/test.yml`](../.github/workflows/test.yml) — `go build` + `go vet` + `go test -race` for every Go module in the monorepo. Coverage artifacts are uploaded per-module.

### Test layout

Tests are colocated with source (`foo_test.go` next to `foo.go`), matching the `server/` convention. SQLite-dependent tests are tagged `//go:build cgo` so the package still compiles on hosts without a C toolchain (Linux CI runs them with cgo enabled). Current coverage:

| Package | What's covered |
|---|---|
| `internal/config` | env parsing, defaults, required-field validation, DSN rendering |
| `internal/middleware` | `X-Internal-Token` gating (missing / empty / wrong / correct / prefix-attack / empty-config defense) |
| `internal/app` | `/healthz` + `/readyz` (OK + failing checker), internal route auth-gating, `nil` config panics, swagger UI route registration |
| `internal/storage` | env-driven pool sizing (defaults + overrides + invalid input), DSN format, nil-config rejection, Ping OK / closed-DB fail / nil-Pool paths *(cgo-tagged sqlite tests)*, idempotent Close |
| `internal/migration` | NewRunner validation (nil db / empty dialect / nil fs / unknown dialect), Up idempotency + Version advance + Down rollback *(cgo-tagged sqlite tests using synthetic fstest.MapFS — real Postgres-only migrations are verified via the standalone binary against a real DB)* |

## Deployment

Helm chart at [`deploy/charts/cs-user/`](../deploy/charts/cs-user/). Service is cluster-internal only (no public exposure).

## Phase progression

| Phase | Scope | Status |
|---|---|---|
| P0-1 | Skeleton (gin + /healthz + config + Dockerfile + Helm chart) | ✅ |
| P0-2 | Postgres connection + goose migrations | ✅ this PR |
| P0-3 | User / UserAuthIdentity models + CRUD | 🔜 next |
| P0-4 | Internal auth middleware | ✅ |
| P0-5 | Helm chart | ✅ |
| P0-6 | ETL script (dry-run + idempotent UPSERT) | 🔜 |
| P0-7 | read-through RPC client in costrict-web | 🔜 |
| P0-8 | costrict-web users table READONLY cutover | 🔜 |

## Database migrations

Migrations are goose SQL files under [`migrations/`](./migrations/), embedded into the binary via `//go:embed *.sql`. The same set ships in both the API and `cs-user-migrate` binaries.

```bash
# Standalone binary — recommended for prod (Helm pre-deploy hook)
make build-migrate           # produces bin/cs-user-migrate
./bin/cs-user-migrate up     # apply pending migrations
./bin/cs-user-migrate version # inspect current schema version
./bin/cs-user-migrate help

# Dev auto-migrate (convenient, runs at API boot)
CS_USER_AUTO_MIGRATE=1 go run ./cmd/api
```

The migrate binary acquires a PostgreSQL advisory lock (`pg_advisory_lock`) before running, so two replicas starting simultaneously cannot race. Lock keys intentionally differ from `server/`'s to avoid false-positive contention if both services ever share a host.
