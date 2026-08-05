#!/usr/bin/env bash
# scripts/import-bundle-to-gitea.sh — import a costrict marketplace bundle's
# plugin repositories into a Gitea instance, in the shape the platform can
# actually consume.
#
# The bundle (costrict-marketplace-bundle-vX.Y.Z.tar.gz, built by
# costrict-plugin-marketplace/scripts/build.py) carries one bare repo per
# catalog plugin under repos/plugins/<id>.git. This script pushes them to
# <gitea>/<owner>/<id>.git — the coordinate the fork path probes as candidate 2
# (handlers/capability_item_fork_git.go: pluginGitMirrorOwner() + item slug),
# because the bundle's bare-repo basename IS the catalog id IS the DB slug.
#
# WHY NOT bundle-assets/import.sh + scripts/mirror-to-gitea.sh (the tools that
# already exist in costrict-plugin-marketplace):
#
#   1. They push EVERY bare repo. A bundle repo whose root manifest names a
#      different plugin than the DB row expects does not fall back to the DB
#      channel — locateGiteaSourceRepo returns errGiteaMirrorManifestInvalid and
#      the user gets HTTP 409 GIT_SOURCE_MANIFEST_INVALID. Importing unfiltered
#      turns "works via the old editor" into "fork is broken" for every
#      mismatched plugin (measured at 23.5% of the intersection on the v0.1.0
#      bundle — monorepo entries collapsed onto their repository root).
#      This script imports only repos whose manifest name matches the DB row.
#   2. mirror-to-gitea.sh PRE-CREATES all repositories and only then pushes.
#      An empty repository under the mirror owner is worse than no repository:
#      the per-item-mirror candidate exists, has no readable manifest, and that
#      verdict is an immediate 409 (it does not fall through to the next
#      candidate). This script creates each repository immediately before its
#      own push and removes the ones it created but could not fill.
#   3. mirror-to-gitea.sh hardcodes https:// in three places, needs bash >= 4,
#      and its /repos/search?owner=... listing is not actually filtered by
#      owner (Gitea ignores the parameter), so repositories owned by somebody
#      else read as "already present". This script takes a full base URL, runs
#      on bash 3.2, and lists the owner's repositories via /users/<owner>/repos.
#
# Everything it writes is verified by reading the manifest back THROUGH THE
# GITEA API on the repository's default branch — the exact call probeRepoManifest
# makes. A push that lands on a branch the repository does not default to is a
# push the platform cannot see, and this is the only check that catches it.
#
# Usage (plan is the default; nothing is written without --apply):
#
#   ./scripts/import-bundle-to-gitea.sh --bundle <dir|tar.gz> [options]
#   ./scripts/import-bundle-to-gitea.sh --bundle <dir> --apply --limit 30
#
# See docs/repo-management/BUNDLE_TO_GITEA_IMPORT.md for the full handbook.

set -euo pipefail

# ---------------------------------------------------------------------------
# defaults
# ---------------------------------------------------------------------------
GITEA_URL="${GITEA_URL:-http://localhost:3001}"
GITEA_OWNER="${GITEA_OWNER:-costrict-plugins-repo}"
GITEA_TOKEN="${GITEA_TOKEN:-}"
PSQL_CMD="${PSQL_CMD:-}"
BUNDLE=""
EXPECT_FILE=""
LIMIT=0
ONLY=""
JOBS="${JOBS:-4}"
APPLY=0
ENSURE_OWNER=0
ALLOW_UNMATCHED=0
REQUIRE_VERSION=0
MARKETPLACE_INDEX=1
FORCE_MARKETPLACE_INDEX=0
HTTP_TIMEOUT="${HTTP_TIMEOUT:-60}"
# Seconds to wait for a freshly pushed repository to become readable through
# the contents API (Gitea clears is_empty asynchronously — see
# probe_manifest_name_settled).
VERIFY_SETTLE_SECONDS="${VERIFY_SETTLE_SECONDS:-30}"
# STATE_DIR / KEEP_EMPTY_ON_FAILURE / PLUGINS_DIR / PUSH_BASE are read from the
# environment because the per-repository worker is a re-invocation of this same
# file: an unconditional assignment here would wipe what the parent exported.
STATE_DIR="${STATE_DIR:-}"
KEEP_EMPTY_ON_FAILURE="${KEEP_EMPTY_ON_FAILURE:-0}"

# Absolute path to this script — the worker is spawned as "$SELF __push-one".
SELF="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"

log() { printf '[bundle-import] %s\n' "$*" >&2; }
die() { printf '[bundle-import] ERROR: %s\n' "$*" >&2; exit 3; }

usage() {
    cat >&2 <<'EOF'
Usage: import-bundle-to-gitea.sh --bundle <dir|tar.gz> [options]

Required:
  --bundle PATH            extracted bundle directory (containing repos/plugins/)
                           or the bundle .tar.gz (extracted next to itself)

Target:
  --gitea-url URL          Gitea base URL          (env GITEA_URL,   default http://localhost:3001)
  --owner NAME             mirror namespace        (env GITEA_OWNER, default costrict-plugins-repo)
                           MUST equal the api process's PLUGIN_GIT_MIRROR_OWNER
  --token TOKEN            Gitea admin PAT         (env GITEA_TOKEN; required with --apply)
  --ensure-owner           create the owner account if it does not exist

Expectations (which plugin each repository must contain):
  --expect-file FILE       TSV "<slug>\t<plugin_name>"; skip DB access entirely
  --psql-cmd CMD           psql invocation used to build it, e.g.
                           'docker exec -i costrict-postgres psql -U costrict -d costrict_db'
                           (env PSQL_CMD; falls back to `psql "$DATABASE_URL"`)

Scope:
  --limit N                only the first N bare repos (byte-sorted)
  --only a,b,c             only these repo ids (or @file with one id per line)
  --jobs N                 parallel pushes (default 4; use 1 behind a flaky proxy)

Behaviour:
  --apply                  actually create/push (default: plan only, no writes)
  --allow-unmatched        also import repos that have no DB row (default: skip)
  --require-version        skip repos whose manifest has no "version"
  --keep-empty-on-failure  keep repositories this run created but failed to fill
                           (default: delete them — an empty mirror is a hard 409)
  --no-marketplace-index   do not render/push .claude-plugin/marketplace.json
  --force-marketplace-index
                           push the index even for a partial run (--limit/--only)
  --state-dir DIR          plan/resume state (default <bundle>/.costrict-import/<owner>)

Exit codes: 0 ok · 3 usage/preflight · 5 some repositories failed · 6 index push failed
EOF
}

# ---------------------------------------------------------------------------
# small helpers
# ---------------------------------------------------------------------------

# resp_body / resp_code split the "<body>\n<http_code>" shape api() prints.
resp_body() { printf '%s' "$1" | sed '$d'; }
resp_code() { printf '%s' "$1" | tail -n1; }

# api METHOD PATH [json-body] — authenticated call against $GITEA_API.
api() {
    local method="$1" path="$2" body="${3:-}"
    local -a args
    args=(-sS -X "$method" "${GITEA_API}${path}"
        -H 'Accept: application/json'
        --max-time "$HTTP_TIMEOUT"
        -w '\n%{http_code}')
    if [ -n "$GITEA_TOKEN" ]; then
        args+=(-H "Authorization: token ${GITEA_TOKEN}")
    fi
    if [ -n "$body" ]; then
        args+=(-H 'Content-Type: application/json' --data "$body")
    fi
    curl "${args[@]}" 2>/dev/null || printf '\n000'
}

# json_get FILTER — read a value out of JSON on stdin. FILTER is a python
# expression over `d`; missing values print empty rather than raising.
json_get() {
    python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(1)
try:
    v = eval(sys.argv[1], {"__builtins__": {}}, {"d": d})
except Exception:
    v = None
sys.stdout.write("" if v is None else str(v))
' "$1"
}

# gitea_read_file OWNER REPO REF PATH — print a repository file's bytes.
# Uses the contents API, i.e. the same read probeRepoManifest performs, so a
# file this cannot see is a file the platform cannot see either.
gitea_read_file() {
    local owner="$1" repo="$2" ref="$3" file="$4" out code
    local -a args
    args=(-sS -G "${GITEA_API}/repos/${owner}/${repo}/contents/${file}"
        --data-urlencode "ref=${ref}"
        -H 'Accept: application/json'
        --max-time "$HTTP_TIMEOUT"
        -w '\n%{http_code}')
    if [ -n "$GITEA_TOKEN" ]; then
        args+=(-H "Authorization: token ${GITEA_TOKEN}")
    fi
    out="$(curl "${args[@]}" 2>/dev/null)" || return 1
    code="$(resp_code "$out")"
    [ "$code" = "200" ] || return 1
    resp_body "$out" | python3 -c '
import sys, json, base64
d = json.load(sys.stdin)
if d.get("encoding") != "base64":
    sys.exit(2)
sys.stdout.buffer.write(base64.b64decode(d.get("content") or ""))
'
}

# manifest_name_of — plugin name declared by a JSON manifest on stdin.
# Mirrors probeRepoManifest: only the top-level "name" counts, and an
# unparsable manifest is not a name.
manifest_name_of() {
    python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(1)
n = d.get("name") if isinstance(d, dict) else None
sys.stdout.write((n or "").strip())
'
}

manifest_version_of() {
    python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(1)
v = d.get("version") if isinstance(d, dict) else None
sys.stdout.write(str(v or "").strip())
'
}

# PLUGIN_MANIFEST_PATHS — the root paths a mirrored plugin may keep its
# manifest at, in probe order.
#
# This list is handlers/capability_item_fork_git.go's pluginManifestPaths, and
# every entry is also classified as a plugin manifest by
# services.classifyGitCapabilityManifest, so a repository accepted here is
# both fork-probeable and discoverable. Note what is NOT here: root
# plugin-manifest.json is recognised by discovery but NOT by the fork probe,
# and <slug>.json / CLAUDE.md are recognised by neither. A repository whose
# only manifest is one of those has been uploaded for nothing.
PLUGIN_MANIFEST_PATHS=".claude-plugin/plugin.json .plugin.json plugin.json"

# ---------------------------------------------------------------------------
# worker: one repository, end to end
# ---------------------------------------------------------------------------
#
# Runs in its own process (re-invoked as `$0 __push-one <id>`) so --jobs can
# fan out with xargs -P on bash 3.2, which has no `wait -n`.
#
# Order: verify what is there → create if absent → push → verify again.
# The pre-push check is the lineage guard: --force --mirror onto a repository
# that already holds a DIFFERENT plugin would destroy somebody else's content
# and rebind this slug to it. Refuse instead.
push_one() {
    local id="$1"
    local bare="${PLUGINS_DIR}/${id}.git"
    local want branch out code created=0 found_name

    # Column 4 of a MATCH row is the plugin name the repository must declare.
    # Reading it back from the plan (rather than passing it through argv) keeps
    # the worker bound to the same verdict the plan was reviewed under.
    want="$(awk -F'\t' -v k="$id" '$1==k && $2=="MATCH"{print $4; exit}' "${STATE_DIR}/plan.tsv")"
    if [ -z "$want" ]; then
        fail_one "$id" plan "no MATCH row for this id in plan.tsv"
        return 1
    fi
    if [ ! -d "$bare" ]; then
        fail_one "$id" bundle "bare repo missing: $bare"
        return 1
    fi

    out="$(api GET "/repos/${GITEA_OWNER}/${id}")"
    code="$(resp_code "$out")"
    case "$code" in
    200)
        branch="$(resp_body "$out" | json_get 'd.get("default_branch")')"
        [ -n "$branch" ] || branch="main"
        # An existing repository is only ours to force-push over if it is empty
        # or already holds THIS plugin. Anything else is somebody's content, and
        # --force --mirror would delete it and then record the result as this
        # item's content truth. Only a repository the API reports as non-empty is
        # worth probing: an empty one has nothing to protect, and waiting on
        # every one of them would add a stall per repository.
        if [ -n "$(resp_body "$out" | json_get '"1" if d.get("empty") is False else ""')" ]; then
            found_name="$(probe_manifest_name_settled "$id" "$branch" 5)" || found_name=""
            if [ -z "$found_name" ]; then
                fail_one "$id" conflict "existing repo is not empty and exposes no plugin manifest; not overwritten (inspect ${GITEA_OWNER}/${id}, then remove it to retry)"
                return 1
            fi
            if ! equal_fold "$found_name" "$want"; then
                fail_one "$id" conflict "existing repo holds plugin ${found_name}, expected ${want} (not overwritten)"
                return 1
            fi
        fi
        ;;
    404)
        out="$(api POST "/admin/users/${GITEA_OWNER}/repos" \
            "$(printf '{"name":%s,"private":false,"auto_init":false,"default_branch":"main"}' \
                "$(json_quote "$id")")")"
        code="$(resp_code "$out")"
        case "$code" in
        201) created=1 ;;
        409)
            # Lost a race with a concurrent worker; treat as present.
            created=0
            ;;
        *)
            fail_one "$id" create "HTTP ${code} $(resp_body "$out" | tr '\n' ' ' | cut -c1-160)"
            return 1
            ;;
        esac
        printf '%s\n' "$id" >>"${STATE_DIR}/created.txt"
        branch="main"
        ;;
    *)
        fail_one "$id" lookup "HTTP ${code}"
        return 1
        ;;
    esac

    # HTTP guards, straight from bundle-assets/import.sh: bail on a transfer
    # that stalls below 1000 B/s for 60s instead of hanging on a wedged reverse
    # proxy, and give git room to buffer a large pack. Auth rides on the
    # host-scoped extraHeader in GIT_CONFIG_GLOBAL — never in the URL.
    if ! git -c http.lowSpeedLimit=1000 -c http.lowSpeedTime=60 -c http.postBuffer=524288000 \
        -C "$bare" push --force --mirror "${PUSH_BASE}/${id}.git" \
        >"${STATE_DIR}/logs/${id}.log" 2>&1; then
        fail_one "$id" push "$(tail -n 2 "${STATE_DIR}/logs/${id}.log" | tr '\n' ' ' | cut -c1-160)"
        cleanup_created "$id" "$created"
        return 1
    fi

    # Read the manifest back through the API on the repository's CURRENT
    # default branch. Pushing a bare repo whose HEAD is `master` into a
    # repository defaulting to `main` succeeds and leaves the platform blind;
    # this is where that shows up.
    branch="$(repo_default_branch "$id")"
    found_name="$(probe_manifest_name_settled "$id" "$branch" "$VERIFY_SETTLE_SECONDS")" || found_name=""
    if [ -z "$found_name" ]; then
        # Try the branch the bare repo itself points at before giving up: an
        # otherwise good import only needs its default branch corrected.
        local head_branch
        head_branch="$(git -C "$bare" symbolic-ref --short HEAD 2>/dev/null || true)"
        if [ -n "$head_branch" ] && [ "$head_branch" != "$branch" ]; then
            found_name="$(probe_manifest_name_settled "$id" "$head_branch" 5)" || found_name=""
            if [ -n "$found_name" ]; then
                api PATCH "/repos/${GITEA_OWNER}/${id}" \
                    "$(printf '{"default_branch":%s}' "$(json_quote "$head_branch")")" >/dev/null
                branch="$head_branch"
            fi
        fi
    fi
    if [ -z "$found_name" ]; then
        fail_one "$id" verify "no readable plugin manifest on branch ${branch} after push"
        cleanup_created "$id" "$created"
        return 1
    fi
    if ! equal_fold "$found_name" "$want"; then
        fail_one "$id" verify "served manifest names ${found_name}, expected ${want}"
        cleanup_created "$id" "$created"
        return 1
    fi

    rm -f "${STATE_DIR}/logs/${id}.log"
    printf '%s\n' "$id" >>"${STATE_DIR}/pushed.txt"
    printf '[bundle-import]   ok      %s (%s @ %s)\n' "$id" "$want" "$branch" >&2
    return 0
}

# fail_one records one repository's failure and says so on the console. Failures
# are per repository by construction: one bad bare repo must never stop a batch
# of 1300, so nothing here propagates upward except this worker's exit code.
fail_one() {
    printf '%s\t%s\t%s\n' "$1" "$2" "$3" >>"${STATE_DIR}/failed.tsv"
    printf '[bundle-import]   FAILED  %s [%s] %s\n' "$1" "$2" "$3" >&2
}

# cleanup_created — delete a repository this run created but could not fill.
#
# Not tidiness: an existing-but-manifest-less repository under the mirror owner
# makes every fork of that plugin fail with 409 instead of falling back to the
# DB channel. Leaving the shell behind converts a transient push failure into a
# permanent product regression. Only ever touches repositories created by THIS
# invocation (created=1); a pre-existing repository is never deleted.
cleanup_created() {
    local id="$1" created="$2" code
    [ "$created" = "1" ] || return 0
    if [ "$KEEP_EMPTY_ON_FAILURE" = "1" ]; then
        log "  kept empty repository ${GITEA_OWNER}/${id} (--keep-empty-on-failure); it will 409 every fork of that plugin"
        return 0
    fi
    code="$(resp_code "$(api DELETE "/repos/${GITEA_OWNER}/${id}")")"
    case "$code" in
    204 | 404) : ;;
    *) log "  WARN: could not remove empty repository ${GITEA_OWNER}/${id} (HTTP $code) — it will 409 every fork of that plugin" ;;
    esac
}

repo_default_branch() {
    local out branch
    out="$(api GET "/repos/${GITEA_OWNER}/$1")"
    [ "$(resp_code "$out")" = "200" ] || {
        printf 'main'
        return 0
    }
    branch="$(resp_body "$out" | json_get 'd.get("default_branch")')"
    [ -n "$branch" ] || branch="main"
    printf '%s' "$branch"
}

# probe_manifest_name — replay probeRepoManifest against a live repository.
probe_manifest_name() {
    local id="$1" branch="$2" p raw name
    for p in $PLUGIN_MANIFEST_PATHS; do
        raw="$(gitea_read_file "$GITEA_OWNER" "$id" "$branch" "$p" 2>/dev/null || true)"
        [ -n "$raw" ] || continue
        name="$(printf '%s' "$raw" | manifest_name_of 2>/dev/null || true)"
        printf '%s' "$name"
        return 0
    done
    return 1
}

# probe_manifest_name_settled — probe_manifest_name, but tolerant of Gitea's
# post-push lag.
#
# Gitea clears a repository's is_empty flag ASYNCHRONOUSLY, and its contents
# API short-circuits to `[]` while the flag is still set. Measured locally:
# ~1.8s between "git push returned" and "the manifest is readable through the
# API". Without this wait every fresh import would verify as "no manifest",
# and — because a mirror that exists without a readable manifest is an
# immediate 409 — the cleanup would then delete a perfectly good repository.
#
# The same lag applies to the platform: a fork issued in the first couple of
# seconds after an import can still see the empty state.
probe_manifest_name_settled() {
    local id="$1" branch="$2" budget="$3" waited=0 name
    while :; do
        name="$(probe_manifest_name "$id" "$branch")" || name=""
        if [ -n "$name" ]; then
            printf '%s' "$name"
            return 0
        fi
        [ "$waited" -lt "$budget" ] || return 1
        sleep 1
        waited=$((waited + 1))
    done
}

# equal_fold — strings.EqualFold, the comparison probeRepoManifest uses.
equal_fold() {
    [ "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" = "$(printf '%s' "$2" | tr '[:upper:]' '[:lower:]')" ]
}

json_quote() { printf '%s' "$1" | python3 -c 'import sys, json; sys.stdout.write(json.dumps(sys.stdin.read()))'; }

# ---------------------------------------------------------------------------
# worker dispatch — must come after every function the worker calls
# ---------------------------------------------------------------------------
if [ "${1:-}" = "__push-one" ]; then
    GITEA_API="${GITEA_URL%/}/api/v1"
    [ -n "${STATE_DIR:-}" ] && [ -n "${PLUGINS_DIR:-}" ] && [ -n "${PUSH_BASE:-}" ] ||
        die "__push-one is an internal worker mode; it needs STATE_DIR/PLUGINS_DIR/PUSH_BASE in the environment"
    push_one "$2"
    exit $?
fi

# ---------------------------------------------------------------------------
# argument parsing
# ---------------------------------------------------------------------------
while [ $# -gt 0 ]; do
    case "$1" in
    --bundle)
        BUNDLE="${2:-}"
        shift 2
        ;;
    --gitea-url)
        GITEA_URL="${2:-}"
        shift 2
        ;;
    --owner)
        GITEA_OWNER="${2:-}"
        shift 2
        ;;
    --token)
        GITEA_TOKEN="${2:-}"
        shift 2
        ;;
    --expect-file)
        EXPECT_FILE="${2:-}"
        shift 2
        ;;
    --psql-cmd)
        PSQL_CMD="${2:-}"
        shift 2
        ;;
    --state-dir)
        STATE_DIR="${2:-}"
        shift 2
        ;;
    --limit)
        LIMIT="${2:-}"
        shift 2
        ;;
    --only)
        ONLY="${2:-}"
        shift 2
        ;;
    --jobs)
        JOBS="${2:-}"
        shift 2
        ;;
    --apply)
        APPLY=1
        shift
        ;;
    --dry-run)
        APPLY=0
        shift
        ;;
    --ensure-owner)
        ENSURE_OWNER=1
        shift
        ;;
    --allow-unmatched)
        ALLOW_UNMATCHED=1
        shift
        ;;
    --require-version)
        REQUIRE_VERSION=1
        shift
        ;;
    --keep-empty-on-failure)
        KEEP_EMPTY_ON_FAILURE=1
        shift
        ;;
    --no-marketplace-index)
        MARKETPLACE_INDEX=0
        shift
        ;;
    --force-marketplace-index)
        FORCE_MARKETPLACE_INDEX=1
        shift
        ;;
    -h | --help)
        usage
        exit 0
        ;;
    *) die "unknown argument: $1 (try --help)" ;;
    esac
done

[ -n "$BUNDLE" ] || {
    usage
    exit 3
}
case "$LIMIT" in '' | *[!0-9]*) die "--limit must be a non-negative integer" ;; esac
case "$JOBS" in '' | *[!0-9]* | 0) die "--jobs must be a positive integer" ;; esac

for bin in git curl python3 awk sort find; do
    command -v "$bin" >/dev/null 2>&1 || die "required tool not on PATH: $bin"
done

GITEA_URL="${GITEA_URL%/}"
GITEA_API="${GITEA_URL}/api/v1"
PUSH_BASE="${GITEA_URL}/${GITEA_OWNER}"
export GITEA_URL GITEA_OWNER GITEA_TOKEN GITEA_API PUSH_BASE HTTP_TIMEOUT KEEP_EMPTY_ON_FAILURE VERIFY_SETTLE_SECONDS

# ---------------------------------------------------------------------------
# 1. resolve the bundle
# ---------------------------------------------------------------------------
BUNDLE_DIR=""
case "$BUNDLE" in
*.tar.gz | *.tgz)
    [ -f "$BUNDLE" ] || die "bundle archive not found: $BUNDLE"
    BUNDLE_DIR="$(cd "$(dirname "$BUNDLE")" && pwd)/$(basename "${BUNDLE%.tar.gz}")"
    BUNDLE_DIR="${BUNDLE_DIR%.tgz}"
    if [ -d "${BUNDLE_DIR}/repos/plugins" ]; then
        log "reusing already-extracted bundle at ${BUNDLE_DIR}"
    else
        log "extracting $(basename "$BUNDLE") ($(wc -c <"$BUNDLE" | tr -d ' ') bytes) → ${BUNDLE_DIR}"
        mkdir -p "$BUNDLE_DIR"
        # Bundles are packed with a single top-level directory; strip it so the
        # layout is always <BUNDLE_DIR>/repos/plugins regardless of the tarball's
        # internal naming.
        tar -xzf "$BUNDLE" -C "$BUNDLE_DIR" --strip-components=1
    fi
    ;;
*)
    [ -d "$BUNDLE" ] || die "bundle directory not found: $BUNDLE"
    BUNDLE_DIR="$(cd "$BUNDLE" && pwd)"
    ;;
esac

PLUGINS_DIR="${BUNDLE_DIR}/repos/plugins"
[ -d "$PLUGINS_DIR" ] || die "$PLUGINS_DIR not found — is $BUNDLE_DIR a marketplace bundle? (a CATALOG bundle has catalog-download/ instead and carries no repository content)"
export PLUGINS_DIR

[ -n "$STATE_DIR" ] || STATE_DIR="${BUNDLE_DIR}/.costrict-import/${GITEA_OWNER}"
mkdir -p "${STATE_DIR}/logs"
export STATE_DIR
touch "${STATE_DIR}/pushed.txt" "${STATE_DIR}/created.txt"

BUNDLE_VERSION="unknown"
if [ -f "${BUNDLE_DIR}/manifest.json" ]; then
    BUNDLE_VERSION="$(json_get 'd.get("bundle_version")' <"${BUNDLE_DIR}/manifest.json" || echo unknown)"
fi

log "bundle:     ${BUNDLE_DIR} (version ${BUNDLE_VERSION:-unknown})"
log "target:     ${GITEA_URL} owner=${GITEA_OWNER}"
log "state:      ${STATE_DIR}"
log "mode:       $([ "$APPLY" = 1 ] && echo APPLY || echo 'PLAN (no writes; pass --apply to import)')"

# ---------------------------------------------------------------------------
# 2. expectations: which plugin must each repository contain
# ---------------------------------------------------------------------------
#
# The comparison is the one probeRepoManifest performs: the repository's root
# manifest `name` against the DB row's metadata.install.plugin_name, case
# insensitively. Everything the platform does with a mirror hangs off this, so
# the expectation set is built from the DB rather than from the bundle's own
# manifest.json (which is what the bundle CLAIMS, not what the platform wants).
EXPECT_TSV="${STATE_DIR}/expect.tsv"
if [ -n "$EXPECT_FILE" ]; then
    [ -f "$EXPECT_FILE" ] || die "--expect-file not found: $EXPECT_FILE"
    # Re-running with the state dir's own expect.tsv is a normal way to repeat a
    # run against the exact expectation set it was planned under; copying a file
    # onto itself is not.
    if [ "$(cd "$(dirname "$EXPECT_FILE")" && pwd)/$(basename "$EXPECT_FILE")" != "$EXPECT_TSV" ]; then
        cp "$EXPECT_FILE" "$EXPECT_TSV"
    fi
    log "expectations: ${EXPECT_FILE} ($(grep -c . "$EXPECT_TSV" || true) rows)"
else
    if [ -z "$PSQL_CMD" ]; then
        [ -n "${DATABASE_URL:-}" ] || die "no expectation source: pass --expect-file, or --psql-cmd, or set DATABASE_URL"
        PSQL_CMD="psql ${DATABASE_URL}"
    fi
    log "expectations: querying capability_items via: ${PSQL_CMD%% *} ..."
    # -tA + \t separator keeps this parseable; the query is the exact field
    # pluginNameOf() reads.
    if ! $PSQL_CMD -v ON_ERROR_STOP=1 -tA -F$'\t' -c "
        SELECT slug, btrim(metadata->'install'->>'plugin_name')
          FROM capability_items
         WHERE item_type = 'plugin'
           AND slug IS NOT NULL AND btrim(slug) <> ''
           AND btrim(coalesce(metadata->'install'->>'plugin_name','')) <> ''
      " 2>"${STATE_DIR}/psql.err" >"${EXPECT_TSV}.raw"; then
        sed 's/^/  /' "${STATE_DIR}/psql.err" >&2
        die "expectation query failed (see ${STATE_DIR}/psql.err)"
    fi
    grep -v '^$' "${EXPECT_TSV}.raw" >"$EXPECT_TSV" || true
    rm -f "${EXPECT_TSV}.raw"
    log "expectations: $(grep -c . "$EXPECT_TSV" || true) plugin rows from the DB"
fi
[ -s "$EXPECT_TSV" ] || die "expectation set is empty — every repository would be classified NO_DB_ROW"

# ---------------------------------------------------------------------------
# 3. select bare repos
# ---------------------------------------------------------------------------
# LC_ALL=C sort keeps --limit byte-stable across runs and across tools.
CANDIDATES="${STATE_DIR}/candidates.txt"
find "$PLUGINS_DIR" -maxdepth 1 -name '*.git' -type d |
    sed -e 's#.*/##' -e 's#\.git$##' |
    LC_ALL=C sort >"$CANDIDATES"

if [ -n "$ONLY" ]; then
    ONLY_FILE="${STATE_DIR}/only.txt"
    case "$ONLY" in
    @*)
        [ -f "${ONLY#@}" ] || die "--only file not found: ${ONLY#@}"
        grep -v '^$' "${ONLY#@}" | LC_ALL=C sort -u >"$ONLY_FILE"
        ;;
    *) printf '%s' "$ONLY" | tr ',' '\n' | grep -v '^$' | LC_ALL=C sort -u >"$ONLY_FILE" ;;
    esac
    LC_ALL=C comm -12 "$CANDIDATES" "$ONLY_FILE" >"${CANDIDATES}.tmp"
    missing="$(LC_ALL=C comm -13 "$CANDIDATES" "$ONLY_FILE" | tr '\n' ' ')"
    [ -z "$missing" ] || log "WARN: --only ids not present in the bundle: ${missing}"
    mv "${CANDIDATES}.tmp" "$CANDIDATES"
fi
if [ "$LIMIT" -gt 0 ]; then
    head -n "$LIMIT" "$CANDIDATES" >"${CANDIDATES}.tmp"
    mv "${CANDIDATES}.tmp" "$CANDIDATES"
fi

TOTAL="$(grep -c . "$CANDIDATES" || true)"
[ "$TOTAL" -gt 0 ] || die "no bare repositories selected"
log "selected:   ${TOTAL} bare repositories$([ "$LIMIT" -gt 0 ] && echo " (--limit $LIMIT)")"

# ---------------------------------------------------------------------------
# 4. plan — classify every selected repository
# ---------------------------------------------------------------------------
#
# Verdicts, and why each is what it is:
#
#   MATCH            root manifest names exactly the plugin the DB row expects
#                    → import
#   NAME_MISMATCH    manifest names a DIFFERENT plugin (the monorepo-collapse
#                    failure mode) → SKIP. Importing it replaces a working DB
#                    fallback with HTTP 409 GIT_SOURCE_MANIFEST_INVALID.
#   NO_MANIFEST      no readable manifest at any probe path → SKIP. The mirror
#                    candidate would exist and be unverifiable, which is an
#                    immediate 409 (it does not fall through).
#   INVALID_MANIFEST manifest is not JSON, or has no "name" → SKIP, same reason.
#   NO_DB_ROW        no capability_items row claims this slug → SKIP by default
#                    (--allow-unmatched imports it anyway; nothing probes it,
#                    but nothing uses it either).
#   NO_VERSION       manifest has no "version" → imported with a warning; the
#                    device's update check has nothing to compare against.
#                    --require-version turns it into a skip.
PLAN="${STATE_DIR}/plan.tsv"
: >"$PLAN"

log "planning ..."
plan_i=0
while IFS= read -r id; do
    [ -n "$id" ] || continue
    plan_i=$((plan_i + 1))
    if [ $((plan_i % 200)) = 0 ]; then log "  planned ${plan_i}/${TOTAL} ..."; fi
    bare="${PLUGINS_DIR}/${id}.git"
    expected="$(awk -F'\t' -v k="$id" '$1==k{print $2; exit}' "$EXPECT_TSV")"

    mpath=""
    raw=""
    for p in $PLUGIN_MANIFEST_PATHS; do
        if raw="$(git -C "$bare" show "HEAD:${p}" 2>/dev/null)" && [ -n "$raw" ]; then
            mpath="$p"
            break
        fi
        raw=""
    done

    if [ -z "$mpath" ]; then
        printf '%s\tNO_MANIFEST\t\t%s\t\t\n' "$id" "$expected" >>"$PLAN"
        continue
    fi
    mname="$(printf '%s' "$raw" | manifest_name_of 2>/dev/null || true)"
    mversion="$(printf '%s' "$raw" | manifest_version_of 2>/dev/null || true)"
    if [ -z "$mname" ]; then
        printf '%s\tINVALID_MANIFEST\t%s\t%s\t\t\n' "$id" "$mpath" "$expected" >>"$PLAN"
        continue
    fi
    if [ -z "$expected" ]; then
        if [ "$ALLOW_UNMATCHED" = 1 ]; then
            printf '%s\tMATCH\t%s\t%s\t%s\tno-db-row\n' "$id" "$mpath" "$mname" "$mversion" >>"$PLAN"
        else
            printf '%s\tNO_DB_ROW\t%s\t\t%s\t\n' "$id" "$mpath" "$mversion" >>"$PLAN"
        fi
        continue
    fi
    if ! equal_fold "$mname" "$expected"; then
        printf '%s\tNAME_MISMATCH\t%s\t%s\t%s\trepo holds %s\n' "$id" "$mpath" "$expected" "$mversion" "$mname" >>"$PLAN"
        continue
    fi
    if [ -z "$mversion" ]; then
        if [ "$REQUIRE_VERSION" = 1 ]; then
            printf '%s\tNO_VERSION\t%s\t%s\t\t\n' "$id" "$mpath" "$expected" >>"$PLAN"
            continue
        fi
        printf '%s\tMATCH\t%s\t%s\t\tno-version\n' "$id" "$mpath" "$expected" >>"$PLAN"
        continue
    fi
    printf '%s\tMATCH\t%s\t%s\t%s\t\n' "$id" "$mpath" "$expected" "$mversion" >>"$PLAN"
done <"$CANDIDATES"

count_verdict() { awk -F'\t' -v v="$1" '$2==v{n++} END{print n+0}' "$PLAN"; }
N_MATCH="$(count_verdict MATCH)"
N_MISMATCH="$(count_verdict NAME_MISMATCH)"
N_NOMANIFEST="$(count_verdict NO_MANIFEST)"
N_INVALID="$(count_verdict INVALID_MANIFEST)"
N_NODB="$(count_verdict NO_DB_ROW)"
N_NOVER="$(count_verdict NO_VERSION)"
N_NOVER_WARN="$(awk -F'\t' '$2=="MATCH" && $6=="no-version"{n++} END{print n+0}' "$PLAN")"

{
    echo "bundle:            ${BUNDLE_DIR} (version ${BUNDLE_VERSION})"
    echo "target:            ${GITEA_URL} owner=${GITEA_OWNER}"
    echo "selected:          ${TOTAL}"
    echo "MATCH (import):    ${N_MATCH}   (of which no version field: ${N_NOVER_WARN})"
    echo "NAME_MISMATCH:     ${N_MISMATCH}   would 409 — not imported"
    echo "NO_MANIFEST:       ${N_NOMANIFEST}   would 409 — not imported"
    echo "INVALID_MANIFEST:  ${N_INVALID}   would 409 — not imported"
    echo "NO_DB_ROW:         ${N_NODB}   no capability_items row — not imported"
    echo "NO_VERSION (skip): ${N_NOVER}"
} >"${STATE_DIR}/plan-summary.txt"

echo
cat "${STATE_DIR}/plan-summary.txt"
echo
log "full plan: ${PLAN} (id, verdict, manifest path, expected plugin, version, note)"

if [ "$APPLY" != 1 ]; then
    log "PLAN ONLY — nothing was written. Re-run with --apply to import the ${N_MATCH} MATCH repositories."
    exit 0
fi

# ---------------------------------------------------------------------------
# 5. apply
# ---------------------------------------------------------------------------
[ -n "$GITEA_TOKEN" ] || die "--apply needs a Gitea admin token (--token or GITEA_TOKEN)"

# Auth for the API is a header (above); auth for git is a HOST-SCOPED
# extraHeader in a private GIT_CONFIG_GLOBAL, so the token never reaches a URL,
# an argv, a log line or a push-failure stderr. The scheme is taken from
# --gitea-url rather than hardcoded, which is what makes this work against a
# plain-http self-hosted Gitea.
GITCONFIG_TMP="$(mktemp -t costrict-gitea-gitconfig.XXXXXX)"
trap 'rm -f "$GITCONFIG_TMP"' EXIT INT TERM
git config -f "$GITCONFIG_TMP" "http.${GITEA_URL}/.extraHeader" "AUTHORIZATION: token ${GITEA_TOKEN}"
export GIT_CONFIG_GLOBAL="$GITCONFIG_TMP"

# Token sanity — a 401 here is far cheaper than 1300 failed pushes.
whoami_out="$(api GET "/user")"
[ "$(resp_code "$whoami_out")" = "200" ] || die "Gitea token rejected (HTTP $(resp_code "$whoami_out")) at ${GITEA_URL}"
log "authenticated as: $(resp_body "$whoami_out" | json_get 'd.get("login")')"

# The owner namespace must exist AND must match the api process's
# PLUGIN_GIT_MIRROR_OWNER: candidate 2 is <that owner>/<slug>, so importing
# under any other name imports into a namespace nothing ever probes.
owner_code="$(resp_code "$(api GET "/users/${GITEA_OWNER}")")"
if [ "$owner_code" != "200" ]; then
    if [ "$ENSURE_OWNER" != 1 ]; then
        die "owner ${GITEA_OWNER} does not exist on ${GITEA_URL} (HTTP ${owner_code}) — create it or pass --ensure-owner"
    fi
    log "creating owner account ${GITEA_OWNER} ..."
    owner_pw="$(python3 -c 'import secrets; print(secrets.token_urlsafe(24))')"
    create_out="$(api POST "/admin/users" "$(python3 -c '
import json, sys
print(json.dumps({
    "username": sys.argv[1],
    "email": sys.argv[1] + "@costrict.local",
    "password": sys.argv[2],
    "must_change_password": False,
}))' "$GITEA_OWNER" "$owner_pw")")"
    case "$(resp_code "$create_out")" in
    201 | 409) log "owner ${GITEA_OWNER} ready" ;;
    *) die "failed to create owner ${GITEA_OWNER}: HTTP $(resp_code "$create_out") $(resp_body "$create_out" | head -c 200)" ;;
    esac
fi

# Resume: anything already pushed AND verified in an earlier run is skipped.
WORK="${STATE_DIR}/work.txt"
awk -F'\t' '$2=="MATCH"{print $1}' "$PLAN" | LC_ALL=C sort >"${STATE_DIR}/match.txt"
LC_ALL=C sort -u "${STATE_DIR}/pushed.txt" >"${STATE_DIR}/pushed.sorted"
LC_ALL=C comm -23 "${STATE_DIR}/match.txt" "${STATE_DIR}/pushed.sorted" >"$WORK"
SKIPPED_DONE=$((N_MATCH - $(grep -c . "$WORK" || true)))
WORK_N="$(grep -c . "$WORK" || true)"
log "to import: ${WORK_N} (resumed/skipped as already imported: ${SKIPPED_DONE})"

: >"${STATE_DIR}/failed.tsv"
: >"${STATE_DIR}/created.txt"

# run_pass LIST JOBS — import every id in LIST, JOBS at a time.
#
# xargs -P over a re-invocation of this script: bash 3.2 has no `wait -n`, and
# a separate process per repository is also what keeps one repository's failure
# (or a hung push killed by the HTTP guards) from taking the batch down. `|| true`
# because xargs exits 123 when any invocation failed — which is the normal case
# for a partially broken bundle, not a reason to abort the run.
run_pass() {
    local list="$1" jobs="$2" n
    n="$(grep -c . "$list" || true)"
    [ "$n" -gt 0 ] || return 0
    log "  pass: ${n} repositories, ${jobs} at a time"
    tr '\n' '\0' <"$list" | xargs -0 -n1 -P "$jobs" "$SELF" __push-one || true
}

log "importing ${WORK_N} repositories (jobs=${JOBS}) ..."
run_pass "$WORK" "$JOBS"

# Serial retry of the failures: a flaky reverse proxy (504, stalled transfer)
# usually clears on a single non-parallel retry. Conflicts and verification
# failures are NOT retried — they are data problems and will fail identically.
if [ -s "${STATE_DIR}/failed.tsv" ]; then
    awk -F'\t' '$2=="push" || $2=="lookup" || $2=="create"{print $1}' "${STATE_DIR}/failed.tsv" |
        LC_ALL=C sort -u >"${STATE_DIR}/retry.txt"
    RETRY_N="$(grep -c . "${STATE_DIR}/retry.txt" || true)"
    if [ "$RETRY_N" -gt 0 ]; then
        log "retrying ${RETRY_N} transport failure(s) serially ..."
        # Drop the retried ids' old rows so the summary reflects the retry's
        # verdict, not the first attempt's. Exact id match, not a substring
        # filter: ids share prefixes (…-agent-skills, …-agent-skills-ci-cd).
        awk -F'\t' 'NR==FNR{retry[$1]=1; next} !($1 in retry)' \
            "${STATE_DIR}/retry.txt" "${STATE_DIR}/failed.tsv" >"${STATE_DIR}/failed.keep"
        mv "${STATE_DIR}/failed.keep" "${STATE_DIR}/failed.tsv"
        run_pass "${STATE_DIR}/retry.txt" 1
    fi
fi

IMPORTED="$(LC_ALL=C sort -u "${STATE_DIR}/pushed.txt" | grep -c . || true)"
CREATED="$(LC_ALL=C sort -u "${STATE_DIR}/created.txt" | grep -c . || true)"
FAILED="$(awk -F'\t' '{print $1}' "${STATE_DIR}/failed.tsv" | LC_ALL=C sort -u | grep -c . || true)"

# ---------------------------------------------------------------------------
# 6. marketplace index
# ---------------------------------------------------------------------------
#
# import.sh's last step, and the one its `set -e` has silently skipped before —
# so this always prints what it did, including when it decided not to run.
#
# The index is filtered to repositories that actually exist: publishing an entry
# whose clone URL 404s is how `csc plugin install` fails on a plugin the index
# claims to have. And a PARTIAL run does not publish at all by default: a
# --limit smoke test would otherwise replace a complete index with 30 entries.
INDEX_STATUS="skipped (--no-marketplace-index)"
INDEX_RC=0
if [ "$MARKETPLACE_INDEX" = 1 ]; then
    TMPL="${BUNDLE_DIR}/marketplace.json.tmpl"
    if [ ! -f "$TMPL" ]; then
        INDEX_STATUS="skipped (no marketplace.json.tmpl in the bundle)"
    elif { [ "$LIMIT" -gt 0 ] || [ -n "$ONLY" ]; } && [ "$FORCE_MARKETPLACE_INDEX" != 1 ]; then
        INDEX_STATUS="skipped (partial run: --limit/--only; pass --force-marketplace-index to publish anyway)"
    else
        MP_DIR="$(mktemp -d -t costrict-marketplace.XXXXXX)"
        mkdir -p "${MP_DIR}/.claude-plugin"
        LC_ALL=C sort -u "${STATE_DIR}/pushed.txt" >"${STATE_DIR}/present.txt"
        if sed "s|{{BASE_URL}}|${PUSH_BASE}|g" "$TMPL" |
            python3 -c '
import sys, json, os
present = set()
with open(sys.argv[1]) as fh:
    for line in fh:
        line = line.strip()
        if line:
            present.add(line)
doc = json.load(sys.stdin)
plugins = doc.get("plugins") or []
kept, dropped = [], 0
for p in plugins:
    url = ((p.get("source") or {}).get("url") or "")
    repo = os.path.basename(url)
    if repo.endswith(".git"):
        repo = repo[:-4]
    if repo in present:
        kept.append(p)
    else:
        dropped += 1
doc["plugins"] = kept
sys.stderr.write("[bundle-import] marketplace index: %d entries kept, %d dropped (repository not present)\n" % (len(kept), dropped))
# An index with no entries is not a valid state to publish: it would tell
# every csc client that the channel has nothing, which is worse than leaving
# the previous index in place.
if not kept:
    sys.exit(3)
json.dump(doc, sys.stdout, ensure_ascii=False, indent=2)
sys.stdout.write("\n")
' "${STATE_DIR}/present.txt" >"${MP_DIR}/.claude-plugin/marketplace.json"; then
            idx_code="$(resp_code "$(api GET "/repos/${GITEA_OWNER}/marketplace")")"
            if [ "$idx_code" = "404" ]; then
                api POST "/admin/users/${GITEA_OWNER}/repos" \
                    '{"name":"marketplace","private":false,"auto_init":false,"default_branch":"main"}' >/dev/null
            fi
            if (
                cd "$MP_DIR"
                git init -b main --quiet 2>/dev/null || {
                    git init --quiet
                    git checkout -q -b main
                }
                git config user.name costrict-import
                git config user.email import@costrict.local
                git add -A
                git commit --quiet -m "costrict-plugins marketplace v${BUNDLE_VERSION}"
                git -c http.lowSpeedLimit=1000 -c http.lowSpeedTime=60 -c http.postBuffer=524288000 \
                    push --force --mirror "${PUSH_BASE}/marketplace.git"
            ) >"${STATE_DIR}/logs/marketplace.log" 2>&1; then
                INDEX_STATUS="pushed to ${PUSH_BASE}/marketplace.git"
            else
                INDEX_STATUS="FAILED (see ${STATE_DIR}/logs/marketplace.log)"
                INDEX_RC=6
            fi
        elif [ ! -s "${MP_DIR}/.claude-plugin/marketplace.json" ]; then
            INDEX_STATUS="skipped (no imported repository matched a template entry — refusing to publish an empty index)"
        else
            INDEX_STATUS="FAILED to render (template not valid JSON after substitution)"
            INDEX_RC=6
        fi
        rm -rf "$MP_DIR"
    fi
fi

# ---------------------------------------------------------------------------
# 7. summary
# ---------------------------------------------------------------------------
echo
echo "=== import summary ==="
echo "bundle:              ${BUNDLE_DIR} (version ${BUNDLE_VERSION})"
echo "target:              ${GITEA_URL}/${GITEA_OWNER}"
echo "planned MATCH:       ${N_MATCH}"
echo "imported (verified): ${IMPORTED}   [cumulative for this state dir]"
echo "repositories created this run: ${CREATED}"
echo "failed this run:     ${FAILED}"
echo "marketplace index:   ${INDEX_STATUS}"
echo "not imported:        NAME_MISMATCH=${N_MISMATCH} NO_MANIFEST=${N_NOMANIFEST} INVALID=${N_INVALID} NO_DB_ROW=${N_NODB} NO_VERSION=${N_NOVER}"
if [ "$FAILED" -gt 0 ]; then
    echo
    echo "failures (id / stage / detail):"
    LC_ALL=C sort -u "${STATE_DIR}/failed.tsv" | sed 's/^/  - /'
fi
echo
echo "state dir: ${STATE_DIR}"
echo "  plan.tsv    every selected repository and its verdict"
echo "  pushed.txt  imported + verified (resume source; delete to force a full re-verify)"
echo "  failed.tsv  this run's failures"

if [ "$INDEX_RC" != 0 ]; then exit "$INDEX_RC"; fi
if [ "$FAILED" -gt 0 ]; then exit 5; fi
exit 0
