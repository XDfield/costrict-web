# E2E Testing — Team Namespace API + Gitea Provisioning

End-to-end integration tests for the team-namespace API surface (Phases 1-2)
against a real, locally-running Gitea fork (v1.27.0). These tests live in
`server/internal/teamns/e2e_test.go` behind the `//go:build e2e` tag and are
**NOT** run by the default `go test` invocation — they require an external
Gitea instance + DB seeds.

## Why a separate suite

Unit tests in `internal/teamns/*_test.go` and `internal/handlers/*_test.go`
exercise the orchestration layer with stub `GitServer` implementations — they
prove the wiring is correct but do NOT validate that:

- Gitea's REST API actually accepts the payloads we send (repo creation,
  branch protection glob `inst-*`, file content base64 encoding, …)
- Round-trip drift detection works against a real `definition_snapshot.json`
  on `main` HEAD
- The bot account → PAT provisioning flow mints a token that can authenticate
  against Gitea for follow-up clone operations
- Member sync resolves UserRef → cs-user RPC → gitea_username end-to-end

The e2e suite covers exactly these gaps.

## Prerequisites

### 1. Local Gitea fork running

Your dev environment already has Gitea v1.27.0 fork running on `http://127.0.0.1:3000`
(see `D:/DEV/gitea`). Confirm:

```bash
curl -sS http://127.0.0.1:3000/api/v1/version
# => {"version":"1.27.0"}
```

If it's not running, start it from your fork's tree (check the fork's own
README for the dev-launch command — typically `make` + `./gitea web`).

### 2. Admin PAT

Sign in as the admin user you created at first-run setup, then:

**UI path**: top-right avatar → Settings → Applications → Generate New Token
**Required scopes** (minimum):
- `write:admin` — for user / org / repo creation
- `write:organization` — for org member ops
- `write:repository` — for repo + branch protection + contents

Copy the PAT — Gitea only shows it once.

### 3. Apply the dev seed

The seed wires up `tenant-e2e`, the `gitea-local` git_servers row, and one
sample user (`usr_e2e_1` / `E001`). It also binds the tenant to the git
server. Edit the file FIRST to drop in your PAT, then apply:

```bash
# Edit the placeholder:
#   cs-user/scripts/dev-seed.sql → line containing <REPLACE_WITH_LOCAL_PAT>
# Replace with the PAT string from step 2.

psql -h 127.0.0.1 -U costrict -d cs_user -f cs-user/scripts/dev-seed.sql
```

Sanity check:

```bash
psql -h 127.0.0.1 -U costrict -d cs_user -c \
  "SELECT t.tenant_id, t.git_server_id, g.endpoint, g.enabled
   FROM tenants t JOIN git_servers g ON t.git_server_id = g.server_id
   WHERE t.tenant_id = 'tenant-e2e';"
# Expect: tenant-e2e | gitea-local | http://127.0.0.1:3000 | t
```

### 4. cs-user running locally

```bash
cd D:/dev/costrict-web/cs-user
go run ./cmd/api
# listens on :8082 (CS_USER_HTTP_PORT=8082 per .env)
```

cs-user must be reachable from the test process; the test reads
`E2E_USER_RPC_URL` (typically `http://127.0.0.1:8082`) and uses
`E2E_USER_RPC_TOKEN` (the cs-user internal token).

### 5. AES key for server-side bot token encryption

Generate a 32-byte base64 key:

```bash
openssl rand -base64 32
# => e.g. "abc123.../xyz=="
```

Export as `CS_BOT_TOKEN_KEY` for the server process AND `E2E_BOT_TOKEN_KEY`
for the test process (must match).

## Running

### Manual

From `server/`:

```bash
export E2E_DB_DSN="postgres://costrict:<pw>@127.0.0.1:5432/costrict?sslmode=disable"
export E2E_USER_RPC_URL="http://127.0.0.1:8082"
export E2E_USER_RPC_TOKEN="<cs-user-internal-token>"
export E2E_BOT_TOKEN_KEY="<same-base64-key-server-uses>"
export E2E_TENANT_ID="tenant-e2e"

make e2e
# Equivalent to:
#   go test -tags=e2e -v -timeout 180s ./internal/teamns/
```

Any missing env var → the relevant test calls `t.Skip` with a clear message.

### Cleanup between runs

Each test cleans up its own Gitea artifacts (org, bot user, repos) on exit via
`t.Cleanup`. If a previous run crashed mid-test and left orphaned resources:

```bash
# Inspect / purge from Gitea admin UI, or via API:
curl -sS -H "Authorization: token <PAT>" \
  http://127.0.0.1:3000/api/v1/admin/orgs | jq '.[] | select(.username | startswith("t-")) | .username'
# Manual DELETE per orphaned org/user/repo.
```

The cs-user DB seed is idempotent — re-running `dev-seed.sql` is always safe.

## What the suite covers

Four active tests (each with own setup + cleanup) + one permanently skipped
placeholder. The skip is intentional — UserRef → cs-user RPC resolution is
covered by `cs-user/cmd/smoke` (which exercises the actual RPC layer) rather
than this server-side e2e suite, which focuses on Gitea provisioning.

| Test | Status | Asserts |
|------|--------|---------|
| `TestE2E_CreateTeam_FullProvisioning` | active | POST /api/internal/teams creates Gitea org `t-<short>`, bot user `bot-t-<short>`, PAT works (clone a probe repo). |
| `TestE2E_SyncTeamMembers_DeltaApply` | **skipped** | Placeholder — UserRef resolution is covered by `cs-user/cmd/smoke`; the server-side e2e suite focuses on git provisioning. |
| `TestE2E_DissolveTeam_RevokesBot` | active | POST /dissolve revokes the bot PAT; subsequent API call with the old token → 401. |
| `TestE2E_RotateBotToken_Gap` | active | POST /bot-token:rotate returns new PAT; old PAT 401s, new PAT authenticates. |
| `TestE2E_EnsureWorkflowRepo_FullPath` | active | POST /api/internal/workflow/init creates `wf-<slug>` repo, writes `definition_snapshot.json` on `main`, applies `main` + `inst-*` branch protection, creates `inst-<short>` branch from main HEAD. Second call is idempotent (Created flags false). |

## Troubleshooting

- **`TEAM_NS_NOT_INITIALIZED` (412) in CreateTeam test** → cs-user's
  `ResolveAdapterForTenant` can't find the git_server binding. Re-run
  `dev-seed.sql`; verify the `UPDATE tenants SET git_server_id='gitea-local'`
  step actually updated the row.
- **`TENANT_GIT_SERVER_UNRESOLVED` (503)** → git_servers row exists but
  `enabled=false`, or `kind` is not `gitea`. Re-check via `SELECT * FROM
  git_servers WHERE server_id='gitea-local'`.
- **Bot PAT returns 401 immediately after provisioning** → most likely the
  PAT scopes are insufficient. Regenerate with `write:admin` +
  `write:organization` + `write:repository`, update `dev-seed.sql`, re-apply.
- **`bot-t-<short>` user creation fails with 409** → a previous run left an
  orphan bot user. Delete it via Gitea admin UI before re-running.
- **Drift 409 on a fresh repo** → the test's first call should land
  `definition_snapshot.json` cleanly; if it doesn't, check that the
  `WriteFile` base64 round-trip matches Gitea's `contents` API expectations
  (this is exactly what the e2e test is designed to catch).

## What's NOT covered

- Cross-tenant isolation (multi-tenant tests need two git_servers rows +
  two tenants; deferred to Phase 3 multi-tenant hardening).
- bot token KMS / vault integration (currently env-injected AES key).
- workflow cancel / status / complete endpoints (not yet implemented).
- Physical GC of dissolved team_ns rows past the 90-day retention window
  (operator runbook, not automated).
