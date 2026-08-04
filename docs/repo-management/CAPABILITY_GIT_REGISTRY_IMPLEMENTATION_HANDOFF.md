# Capability Git Registry implementation handoff

| Field | Value |
|---|---|
| Status | Active implementation checkpoint |
| Recorded | 2026-08-04 |
| Baseline | `CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md`, especially section 10.3 |
| Scope | Git-backed capability discovery, reconciliation, Marketplace projection, and Multica handoff UX |

## Core question

After a Plugin is forked, can Git remain the single owner of the repository files, manifest metadata, and version while the server, Multica, and csc converge on the same Plugin identity?

The current checkpoint closes manifest-set reconciliation and the misleading Hub edit/update flow. Full Git body synchronization is intentionally deferred, so this checkpoint does not yet claim that every DB content cache or revision row is derived continuously from Git.

## Completed in costrict-web

- Gitea push webhook ingestion, idempotent sync jobs, repository-level serialization, and current default-branch HEAD convergence.
- First discovery for unknown repositories, including skill, subagent, command, MCP, and Plugin manifests.
- Stable Git identity based on `git_server_id + git_repo_id + manifest_path + entry_key`.
- Reconciliation of the complete manifest set on every valid push for an already-bound repository:
  - create a capability and initial version for a new manifest;
  - refresh metadata and Git projection fields for an existing manifest while preserving its locked type, path, entry key, and slug;
  - archive a missing manifest without physical deletion;
  - reactivate the same row when the manifest reappears;
  - reconcile individual entries in multi-entry MCP manifests.
- Repository rename, visibility, default branch, owner, Git SHA, and last-sync projection.
- `.plugin.json` now projects its actual top-level `version` instead of the parser default `1.0.0`.
- Git-backed content-derived fields and archive uploads are rejected by the ordinary item update API with HTTP 409. Runtime and administrative fields remain writable.
- `gitLastSyncedAt` is exposed in item responses and Swagger.
- Git-owned registries are excluded from the legacy clone scheduler and reject direct legacy sync, preventing two writers from archiving or rewriting the same capability rows.
- Owner resolution uses the active transaction handle and remains lazy for pre-bound repositories, avoiding a SQLite self-deadlock and unnecessary sync failures.

## Completed in Multica

The companion implementation lives in `/Volumes/Work/Projects/multica`.

- A newly created Git-backed fork lands on its detail page instead of the Hub editor.
- "View my fork" and "Forked from" navigate to detail pages.
- Owners edit Git-backed items through an explicit "Edit in Gitea" action.
- Legacy editor URLs render a dedicated Gitea handoff page with no metadata inputs, file editor, Publish button, or Update button.
- Git-backed details show the current manifest version and last Git sync time, and hide the DB-backed Hub version history.
- The detail page does not show the inherited upstream Marketplace install command for a Git-backed fork.
- Git-backed install commands used elsewhere continue to target the fork repository and shell-quote repository-controlled values.

## Intentionally deferred

- Continuous synchronization or read-through of the full Git body into `capability_items.content`.
- Incrementing `current_revision` and creating a new `capability_versions` row for every later Git push.
- Security Scan enqueue and item-level health / pending-review state from the new Git sync worker.
- Administrator review, type unlock, and repair UI/API.
- Pull-request webhook previews, checks, pollution detection, and PR comments.
- Mirror/seed-specific orchestration and offline Marketplace snapshots.
- Removal of legacy Catalog ingest and remaining DB truth-source paths.

These are follow-up phases, not removed requirements. In particular, body/revision synchronization must define its cache and version semantics before it is added to the reconciliation transaction.

## Verification checkpoint

Focused server checks:

```bash
cd /Volumes/Work/Projects/costrict-web/server
go test -timeout 2m ./internal/services ./internal/handlers ./internal/scheduler
```

Focused Multica checks:

```bash
cd /Volumes/Work/Projects/multica/packages/views
pnpm exec vitest run \
  hub/components/item-detail-content.test.tsx \
  hub/editor/capability-editor-page.test.tsx \
  hub/lib/git-backed.test.ts \
  hub/lib/install-command.test.ts
pnpm --filter @multica/views typecheck
```

The broader server suite currently has an unrelated timing-boundary failure at `server/internal/clawagent/session_lifecycle_test.go:154` (`TestIsStale_ExactBoundary`). Do not treat that failure as a Git Registry regression.

## Manual acceptance flow

1. Open Multica and fork a Plugin.
2. Confirm the fork opens its detail page and offers "Edit in Gitea" without a second Hub Update/Publish action.
3. In Gitea, rename the Plugin in its manifest, increment its manifest version, commit, and push the default branch.
4. Return to Multica and confirm the same capability identity shows the new name, current version, and a newer last-sync time.
5. Subscribe to the fork.
6. In csc, confirm the `costrict-plugins` Marketplace resolves the fork to its Git repository and current synced commit, then confirm the installed/runtime Plugin exposes the updated name/version.

For this flow, the deployment must provide a reachable `GIT_SYSTEM_WEBHOOK_BASE_URL` or an equivalent repository hook. An empty value intentionally disables system-hook reconciliation and leaves newly created repositories without push coverage.
