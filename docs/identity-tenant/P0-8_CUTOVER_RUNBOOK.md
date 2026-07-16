# P0-8 READONLY Cutover Runbook

Operational runbook for cutting the costrict-web `users` table over to read-only
after the cs-user microservice takes over user data ownership. P0-7 shipped the
read-through RPC client (`USER_SERVICE_BACKEND`); P0-8a shipped the write-gate
kill switch (`USER_SERVICE_WRITE_MODE`); P0-8b is the operational sequence
below.

## Status

- **P0-7 (merged)**: read-through RPC client, env-gated, default `local`.
- **P0-8a (merged)**: application-layer write gate, default `local`. Zero
  production behavior change until an operator flips the env. Cannot be safely
  enabled in production yet — see "Blocker" below.
- **P0-8b (this runbook, deferred)**: operational cutover. **Blocked on
  cs-user Phase 2 write API.**

## Blocker: cs-user has no write API

Phase 1 cs-user ships only 4 GET endpoints. There is no `POST /users`, no
bind/unbind/transfer endpoint. Until Phase 2 ships a write API and login is
re-routed through it, enabling `USER_SERVICE_WRITE_MODE=readonly` will break
every login attempt. The P0-8a boot validation makes this explicit:

```
user.NewWithConfig: USER_SERVICE_WRITE_MODE=readonly with USER_SERVICE_BACKEND=rpc
  — cs-user has no write API yet (Phase 2 pending); writes have nowhere to go.
  Keep USER_SERVICE_WRITE_MODE=local until cs-user Phase 2 ships.
```

Process exits non-zero. Do not attempt to enable readonly mode until this
fatal is removed in a future PR.

## Env-var matrix

| `USER_SERVICE_BACKEND` | `USER_SERVICE_WRITE_MODE` | Behaviour | Boot |
|---|---|---|---|
| `local` (default) | `local` (default) | Reads + writes both hit costrict-web DB. Zero change. | OK |
| `local` | `readonly` | Writes blocked at the gate, reads stay local — login is broken. | **Fatal** |
| `rpc` | `local` | Split-brain: reads from cs-user RPC, writes still local. | **WARN** (canary only) |
| `rpc` | `readonly` | Reads from cs-user, writes blocked — login is broken (no cs-user write API yet). | **Fatal** |

The split-brain combination is the canary posture. The two fatal combinations
block boot. The default is fully backward-compatible.

## Prerequisites (before P0-8b cutover)

1. **cs-user Phase 2 write API shipped and load-tested.** Login, identity
   bind/unbind/transfer, and user upsert all need RPC write paths in cs-user
   before P0-8b can begin.
2. **Login path re-routed through cs-user writes.** Until OAuth callback calls
   `cs-user.POST /users` instead of `UserService.GetOrCreateUser`, the readonly
   gate stays fatal.
3. **P0-6 ETL convergence verified.** Both DBs agree on every active user
   (run the diff tool, expect 0 divergence over 24h).
4. **Backup taken.** Snapshot costrict-web DB; retain 30 days for rollback.

## Canary sequence (read-only RPC validation)

Goal: validate the RPC read path under live traffic without touching writes.

1. Ensure `USER_SERVICE_BACKEND=local USER_SERVICE_WRITE_MODE=local` (baseline).
2. Roll one replica with `USER_SERVICE_BACKEND=rpc USER_SERVICE_WRITE_MODE=local`
   + `USER_SERVICE_URL=http://cs-user:8080` + `USER_SERVICE_INTERNAL_TOKEN=...`.
3. Boot will log the split-brain warning. Expected.
4. Monitor `/api/auth/me` 500-rate for 1 hour. Acceptable threshold: < 0.1%.
5. Monitor cs-user logs for 4xx/5xx. RPC timeouts should be near zero.
6. After canary passes, roll remaining replicas.

**Rollback**: set env back to `local`/`local`, restart. One-line revert.

## Cutover sequence (P0-8b, blocked)

Goal: costrict-web `users` table becomes read-only; cs-user is the write
authority.

1. Complete prerequisites above.
2. Ship the application change that re-routes login writes to cs-user (separate
   PR — adds `RPCWriter` alongside `RPCClient`, removes the readonly+rpc fatal).
3. Roll replicas to `USER_SERVICE_BACKEND=rpc USER_SERVICE_WRITE_MODE=local`
   (cs-user writes flow, costrict-web still writes locally as belt-and-suspenders).
4. Verify dual-write convergence for 24h.
5. Roll replicas to `USER_SERVICE_WRITE_MODE=readonly` — local writes blocked.
6. Apply the DB trigger migration (P0-8b scope) that rejects any direct write
   to the `users` table outside the application session.
7. Monitor for 1 hour. Cache hit rate on `CachedUserService` should be > 90%.

**Rollback**:
- Set `USER_SERVICE_WRITE_MODE=local`, restart. Writes resume.
- Drop the DB trigger.
- (Last resort) restore DB snapshot. cs-user DB is authoritative after cutover;
  restoring costrict-web DB alone will not roll back user state.

## Failure modes and mitigation

- **RPC outage during canary**: `/api/auth/me` returns 500. Mitigation: keep
  canary to a small replica subset; one-line revert.
- **Write-then-read skew during dual-write**: `server` writes locally, but
  cs-user DB updates on the next ETL tick (default 15min). The 10-min cache TTL
  partially masks this. Worst case: a freshly-bound identity isn't visible via
  `/api/users/:id/auth-identities` for up to ETL interval. Mitigation: tighten
  cache TTL during canary, or skip cache on identity-listing endpoints.
- **Misconfigured env at boot**: process exits with the documented fatal.
  Resolution: fix the env, restart. No data loss.

## Verification checklist

After P0-8a deployment (default config):
- [ ] `make check` passes locally and in CI.
- [ ] Boot with no env: process starts normally.
- [ ] Boot with `USER_SERVICE_WRITE_MODE=readonly USER_SERVICE_BACKEND=local`:
      process exits with the documented fatal.
- [ ] Boot with `USER_SERVICE_WRITE_MODE=readonly USER_SERVICE_BACKEND=rpc`:
      process exits with the no-write-API fatal.
- [ ] `go test -race -count=1 ./internal/user/...`: readonly_test passes.
