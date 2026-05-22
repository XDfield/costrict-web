#!/usr/bin/env bash
# Read-only reconnaissance of a costrict-web deployment.
# Run on the appropriate bastion (kubectl must already point at the right cluster).
#
# Environments:
#   ENVIRONMENT=staging   (default)   → https://zgsmtest.cn:30443/cloud
#   ENVIRONMENT=production            → https://zgsm.sangfor.com/cloud
#
# The script does NOT change state. It only dumps facts useful for planning.
#
# Usage:
#   bash probe-env.sh > /tmp/probe-staging.txt 2>&1
#   ENVIRONMENT=production bash probe-env.sh > /tmp/probe-prod.txt 2>&1

set -uo pipefail

ENVIRONMENT="${ENVIRONMENT:-staging}"
NAMESPACE="${NAMESPACE:-costrict-web}"
API_LABEL_NAME="${API_LABEL_NAME:-costrict-web-api}"
WORKER_LABEL_NAME="${WORKER_LABEL_NAME:-costrict-web-worker}"
PORTAL_LABEL_NAME="${PORTAL_LABEL_NAME:-costrict-web-portal}"

case "$ENVIRONMENT" in
  staging|prod|production) ;;
  *) echo "ERROR: ENVIRONMENT must be staging|production, got '$ENVIRONMENT'" >&2; exit 2 ;;
esac

section() { printf '\n========== %s ==========\n' "$*"; }

section "environment + kubectl context"
echo "ENVIRONMENT:     $ENVIRONMENT"
echo "namespace:       $NAMESPACE"
echo "host:            $(hostname)"
echo "kubectl context: $(kubectl config current-context 2>/dev/null || echo unknown)"
echo "cluster server:  $(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null || echo unknown)"
echo ""
echo "▸ Verify this is the cluster you intended BEFORE proceeding."

section "nodes (arch / OS / kubelet)"
kubectl get nodes -o custom-columns=NAME:.metadata.name,ARCH:.status.nodeInfo.architecture,OS:.status.nodeInfo.osImage,KUBELET:.status.nodeInfo.kubeletVersion 2>&1

section "helm releases"
helm -n "$NAMESPACE" list 2>&1 || echo "(helm not on PATH)"

section "deployments + image tags"
kubectl -n "$NAMESPACE" get deploy -o custom-columns=NAME:.metadata.name,IMAGE:.spec.template.spec.containers[*].image,READY:.status.readyReplicas,DESIRED:.spec.replicas 2>&1

section "labels on workload pods"
kubectl -n "$NAMESPACE" get pod -o custom-columns=NAME:.metadata.name,LABELS:.metadata.labels 2>&1

# Auto-detect api pod by label, fall back to name prefix.
API_POD=$(kubectl -n "$NAMESPACE" get pod \
  -l "app.kubernetes.io/name=${API_LABEL_NAME}" \
  --field-selector=status.phase=Running \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
if [[ -z "$API_POD" ]]; then
  API_POD=$(kubectl -n "$NAMESPACE" get pod \
    --field-selector=status.phase=Running \
    -o jsonpath='{.items[*].metadata.name}' \
    | tr ' ' '\n' \
    | grep -E "^api-${API_LABEL_NAME}-" \
    | head -1)
fi

section "auto-detected api pod"
if [[ -z "$API_POD" ]]; then
  echo "(no Running api pod found; aborting DB probe)"
  echo "all pods:"
  kubectl -n "$NAMESPACE" get pod 2>&1
  exit 1
fi
echo "api_pod: $API_POD"

section "ingest-relevant env on api pod (secrets masked)"
kubectl -n "$NAMESPACE" exec "$API_POD" -- env 2>/dev/null \
  | grep -E '^(DATABASE_URL|REDIS_URL|CASDOOR_ENDPOINT|CASDOOR_ORGANIZATION|CASDOOR_CALLBACK_URL|COSTRICT_CLOUD_BASE_URL|FRONTEND_URLS|SCAN_ENABLED|USAGE_PROVIDER|SECURITY_SCAN_SHORT_CIRCUIT_DISABLED|PORT)=' \
  | python3 -c '
import sys, re
mask = re.compile(r"(?<=://)([^:@/]+):[^@]+(?=@)")
for line in sys.stdin:
    sys.stdout.write(mask.sub(r"\1:***", line))
' 2>/dev/null || echo "(env dump failed; check python3 availability on bastion)"

section "/app contents + migrate subcommands"
kubectl -n "$NAMESPACE" exec "$API_POD" -- sh -c '
  echo "--- /app contents ---"
  ls -la /app 2>/dev/null
  echo ""
  echo "--- migrate help (look for ingest-upstream subcommand) ---"
  /app/migrate help 2>&1 | head -40
' 2>&1

section "DB probe (piped psql)"
SQL_PROBE=$(cat <<'__PROBE_SQL__'
\echo --- capability_items: type / status counts (public registry) ---
SELECT item_type, status, count(*)
  FROM capability_items
  WHERE registry_id='00000000-0000-0000-0000-000000000001'
  GROUP BY 1,2 ORDER BY 1,2;

\echo
\echo --- source_path samples ---
SELECT item_type, source_path
  FROM capability_items
  WHERE registry_id='00000000-0000-0000-0000-000000000001'
  ORDER BY random() LIMIT 10;

\echo
\echo --- registry rows (public + 5 most recent others) ---
( SELECT id, name, source_type, COALESCE(external_url,'(none)') AS external_url,
         last_synced_at, sync_status
    FROM capability_registries
    WHERE id='00000000-0000-0000-0000-000000000001' )
UNION ALL
( SELECT id, name, source_type, COALESCE(external_url,'(none)') AS external_url,
         last_synced_at, sync_status
    FROM capability_registries
    WHERE id<>'00000000-0000-0000-0000-000000000001'
    ORDER BY last_synced_at DESC NULLS LAST
    LIMIT 5 );

\echo
\echo --- schema check ---
SELECT column_name, data_type FROM information_schema.columns
  WHERE table_name='capability_items'
    AND column_name IN ('content_md5','current_revision','security_status',
                        'last_scan_id','source_sha','experience_score',
                        'metadata','source_path','source')
  ORDER BY column_name;

\echo
\echo --- scan_jobs / security_scans ---
SELECT (SELECT count(*) FROM security_scans) AS security_scans,
       (SELECT count(*) FROM scan_jobs WHERE status='pending') AS pending_scan_jobs,
       (SELECT count(*) FROM scan_jobs WHERE status='running') AS running_scan_jobs;

\echo
\echo --- last 5 sync_logs ---
SELECT id, registry_id, trigger_type, status, added_items, updated_items,
       deleted_items, failed_items, started_at, finished_at
  FROM sync_logs ORDER BY started_at DESC LIMIT 5;

\echo
\echo --- tables in schema ---
SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename;
__PROBE_SQL__
)

echo "$SQL_PROBE" | kubectl -n "$NAMESPACE" exec -i "$API_POD" -- sh -c '
  PSQL=$(command -v psql)
  if [ -z "$PSQL" ]; then
    apk add --no-cache postgresql-client >/dev/null 2>&1 \
      || apt-get install -y postgresql-client >/dev/null 2>&1 \
      || true
    PSQL=$(command -v psql)
  fi
  if [ -z "$PSQL" ]; then
    echo "(no psql in api pod; try kubectl exec into postgres pod manually)"
    exit 13
  fi
  $PSQL "$DATABASE_URL" -P pager=off -f -
' 2>&1

section "DONE — $ENVIRONMENT probe complete"
