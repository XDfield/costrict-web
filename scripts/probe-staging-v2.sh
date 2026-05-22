#!/usr/bin/env bash
# Read-only probe v2: auto-detect the api pod by name pattern instead of
# relying on the standard `app.kubernetes.io/name` label that this chart
# doesn't seem to set. Also dumps labels so we can pin a stable selector
# for the ingest runbook.

set -uo pipefail

NAMESPACE="${NAMESPACE:-costrict-web}"

section() { printf '\n========== %s ==========\n' "$*"; }

section "labels on api pod (so we know the right selector)"
kubectl -n "$NAMESPACE" get pod -o custom-columns=NAME:.metadata.name,LABELS:.metadata.labels 2>&1

section "auto-detect api pod by name pattern"
API_POD=$(kubectl -n "$NAMESPACE" get pod \
  --field-selector=status.phase=Running \
  -o jsonpath='{.items[*].metadata.name}' \
  | tr ' ' '\n' \
  | grep -E '^api-costrict-web-api-' \
  | head -1)
if [[ -z "$API_POD" ]]; then
  echo "still no api pod found; dumping all pods for debugging:"
  kubectl -n "$NAMESPACE" get pod 2>&1
  exit 1
fi
echo "api_pod: $API_POD"

section "filtered env on api pod (ingest-relevant only, secrets masked)"
kubectl -n "$NAMESPACE" exec "$API_POD" -- env 2>/dev/null \
  | grep -E '^(DATABASE_URL|REDIS_URL|CASDOOR_ENDPOINT|CASDOOR_ORGANIZATION|CASDOOR_CALLBACK_URL|COSTRICT_CLOUD_BASE_URL|FRONTEND_URLS|SCAN_ENABLED|USAGE_PROVIDER|SECURITY_SCAN_SHORT_CIRCUIT_DISABLED|PORT)=' \
  | sed -E 's/(://[^:]+:)[^@]+(@)/\1***\2/g'

section "/app contents and migrate help"
kubectl -n "$NAMESPACE" exec "$API_POD" -- sh -c '
  echo "--- /app contents ---"
  ls -la /app 2>/dev/null
  echo ""
  echo "--- migrate help / subcommands (this tells us if ingest-upstream is already deployed) ---"
  /app/migrate help 2>&1 | head -40 || /app/migrate --help 2>&1 | head -40 || /app/migrate -h 2>&1 | head -40
' 2>&1

section "DB probe via psql piped through kubectl exec -i"
SQL_PROBE=$(cat <<'__PROBE_SQL__'
\echo --- capability_items: type / status counts (public registry) ---
SELECT item_type, status, count(*)
  FROM capability_items
  WHERE registry_id='00000000-0000-0000-0000-000000000001'
  GROUP BY 1,2 ORDER BY 1,2;

\echo
\echo --- source_path samples per type (shape check) ---
SELECT item_type, source_path
  FROM capability_items
  WHERE registry_id='00000000-0000-0000-0000-000000000001'
  ORDER BY random() LIMIT 10;

\echo
\echo --- registry rows ---
SELECT id, name, source_type,
       COALESCE(external_url,'(none)') AS external_url,
       COALESCE(last_sync_sha,'(none)') AS last_sync_sha,
       last_synced_at, sync_status
  FROM capability_registries
  ORDER BY created_at;

\echo
\echo --- schema check: do critical columns exist? ---
SELECT column_name, data_type FROM information_schema.columns
  WHERE table_name='capability_items'
    AND column_name IN ('content_md5','current_revision','security_status',
                        'last_scan_id','source_sha','experience_score',
                        'metadata','source_path','source')
  ORDER BY column_name;

\echo
\echo --- security_scans + scan_jobs queue ---
SELECT (SELECT count(*) FROM security_scans) AS security_scans,
       (SELECT count(*) FROM scan_jobs WHERE status='pending') AS pending_scan_jobs,
       (SELECT count(*) FROM scan_jobs WHERE status='running') AS running_scan_jobs;

\echo
\echo --- last 5 sync_logs ---
SELECT id, registry_id, trigger_type, status, added_items, updated_items,
       deleted_items, failed_items, started_at, finished_at
  FROM sync_logs ORDER BY started_at DESC LIMIT 5;

\echo
\echo --- table list (so we know which auto-migrate rows already exist) ---
SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename;
__PROBE_SQL__
)

# api pod ships its own binaries but probably no psql client; install if missing.
echo "$SQL_PROBE" | kubectl -n "$NAMESPACE" exec -i "$API_POD" -- sh -c '
  PSQL=$(command -v psql)
  if [ -z "$PSQL" ]; then
    apk add --no-cache postgresql-client >/dev/null 2>&1 \
      || apt-get install -y postgresql-client >/dev/null 2>&1 \
      || true
    PSQL=$(command -v psql)
  fi
  if [ -z "$PSQL" ]; then
    echo "(no psql client; falling back to postgres pod)"
    exit 13
  fi
  $PSQL "$DATABASE_URL" -P pager=off -f -
' 2>&1
RC=$?
if [[ "$RC" == "13" ]]; then
  section "fallback: psql inside postgres pod"
  POSTGRES_POD=$(kubectl -n "$NAMESPACE" get pod \
    --field-selector=status.phase=Running \
    -o jsonpath='{.items[*].metadata.name}' \
    | tr ' ' '\n' | grep '^postgres-' | head -1)
  echo "postgres_pod: $POSTGRES_POD"
  # Need DATABASE_URL value; try to read it from api pod
  DBURL=$(kubectl -n "$NAMESPACE" exec "$API_POD" -- sh -c 'echo "$DATABASE_URL"' 2>/dev/null)
  if [[ -n "$POSTGRES_POD" && -n "$DBURL" ]]; then
    echo "$SQL_PROBE" | kubectl -n "$NAMESPACE" exec -i "$POSTGRES_POD" \
      -- psql "$DBURL" -P pager=off -f - 2>&1
  else
    echo "postgres pod or DATABASE_URL unavailable"
  fi
fi

section "deploy yaml for api (for the runbook to know the selector + envFrom)"
kubectl -n "$NAMESPACE" get deploy api-costrict-web-api -o yaml 2>&1 \
  | grep -E "  (name|labels|selector|image|envFrom|matchLabels):" \
  | head -30

section "DONE"
