# Gitea lifecycle webhook fixtures

Captured from a **real Gitea 1.24.6** deployment (`GET /api/v1/version` → `{"version":"1.24.6"}`)
on 2026-08-06, by attaching a throw-away **system webhook** (`send_everything`) to a
capture-only HTTP receiver and performing each lifecycle operation against throw-away
`fixture-probe-*` repositories.

Each `*.json` file is the **exact request body Gitea sent, byte for byte** (Gitea emits
1-space-indented JSON). Do not reformat them: `X-Gitea-Signature` is
`hex(HMAC_SHA256(webhook_secret, raw_body_bytes))`, so reformatting invalidates any
signature test that recomputes the MAC over the file contents.

No credential material is present in these bodies (verified: no access token, no webhook
secret, no password). Emails are Gitea's synthetic `*@noreply.localhost` form.

| File | `X-Gitea-Event` | `action` | Triggering operation |
|---|---|---|---|
| `repository_created.json` | `repository` | `created` | `POST /api/v1/user/repos` (user-owned) |
| `repository_deleted_user_owned.json` | `repository` | `deleted` | `DELETE /api/v1/repos/{user}/{repo}` |
| `repository_deleted_org_owned.json` | `repository` | `deleted` | `DELETE /api/v1/repos/{org}/{repo}` |
| `push_default_branch.json` | `push` | *(absent)* | `git push origin main` |
| `push_new_branch.json` | `push` | *(absent)* | branch creation; `before` is 40 zeros |
| `create_branch.json` | `create` | *(absent)* | branch creation (`ref_type: branch`) |
| `delete_branch.json` | `delete` | *(absent)* | non-default branch deletion |

`headers_system_delivery.json` records the header set per fixture with per-delivery
values (signatures, delivery UUIDs) replaced by placeholders.

## Operations that emit NO webhook at all on 1.24.6

Verified through **both** the REST API and the web UI, with a system webhook subscribed to
every event type. There is no fixture for these because Gitea sends nothing:

- repository rename
- repository transfer (to an org and to a user)
- visibility change (public ↔ private, both directions)
- default branch change
- repository archive / unarchive

Deleting the default branch is refused outright (REST `403 can not delete default branch`;
`git push origin :main` is declined by the pre-receive hook), so it produces no event either.

## Fidelity caveat

`repository`/`deleted` is delivered **only to system-level (and owner-level) webhooks**. A
repository-level webhook is destroyed together with the repository and observes nothing.
Deliveries to a system webhook carry `X-Gitea-Hook-Installation-Target-Type: system`;
repository-level deliveries carry `repository`.

See `.trellis/tasks/08-06-git-lifecycle-history/research/gitea-lifecycle-fixtures.md` for
the full capture log, field paths, and the reconcile implications.
