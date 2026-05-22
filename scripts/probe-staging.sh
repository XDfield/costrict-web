#!/usr/bin/env bash
# Read-only reconnaissance of the staging environment (namespace=costrict-web).
# DO NOT MODIFY STATE. Just dump everything we need to know before running ingest.
#
# Run on the bastion (root@10.20.19.2), pipe output back to me:
#   bash probe-staging.sh > /tmp/staging-probe.txt 2>&1
#   then scp /tmp/staging-probe.txt back to the laptop.

set -uo pipefail   # intentionally NOT using -e: we want all sections to run

NAMESPACE="${NAMESPACE:-costrict-web}"

section() { printf '\n========== %s ==========\n' "$*"; }

section "context"
echo "date:    $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "host:    $(hostname)"
echo "ns:      $NAMESPACE"
echo "kubectl: $(kubectl version --client -o yaml 2>/dev/null | grep gitVersion | head -1)"

section "nodes (arch matters for cross-compiled migrate binary)"
kubectl get nodes -o custom-columns=NAME:.metadata.name,ARCH:.status.nodeInfo.architecture,OS:.status.nodeInfo.osImage,KUBELET:.status.nodeInfo.kubeletVersion 2>&1

section "helm releases in namespace"
helm -n "$NAMESPACE" list 2>&1 || echo "(helm not on PATH or no releases)"

section "deployments & their image tags"
kubectl -n "$NAMESPACE" get deploy -o custom-columns=NAME:.metadata.name,IMAGE:.spec.template.spec.containers[*].image,READY:.status.readyReplicas,DESIRED:.spec.replicas 2>&1

section "all pods + restart counts"
kubectl -n "$NAMESPACE" get pod -o wide 2>&1

section "api pod env (filtered to ingest-relevant vars only — no secrets dumped)"
API_POD=$(kubectl -n "$NAMESPACE" get pod -l app.kubernetes.io/name=api \
  -o jsonpath='{.items[?(@.status.phase=="Running")].metadata.name}' \
  | awk '{print $1}')
if [[ -n "$API_POD" ]]; then
  echo "api_pod: $API_POD"
  kubectl -n "$NAMESPACE" exec "$API_POD" -- env 2>/dev/null \
    | grep -E '^(DATABASE_URL|REDIS_URL|CASDOOR_ENDPOINT|CASDOOR_ORGANIZATION|CASDOOR_CALLBACK_URL|COSTRICT_CLOUD_BASE_URL|FRONTEND_URLS|SCAN_ENABLED|USAGE_PROVIDER)=' \
    | sed -E 's/(password|PASSWORD|secret|SECRET|TOKEN|token)=[^&]*/\1=***/g'
else
  echo "no Running api pod found under selector app.kubernetes.io/name=api"
  kubectl -n "$NAMESPACE" get pod -l app.kubernetes.io/name=api 2>&1
fi

section "what's inside the api pod (binary versions / dates)"
if [[ -n "$API_POD" ]]; then
  kubectl -n "$NAMESPACE" exec "$API_POD" -- sh -c '
    echo "--- /app contents ---"
    ls -la /app 2>/dev/null
    echo ""
    echo "--- existing migrate help (shows which subcommands the deployed binary supports) ---"
    /app/migrate --help 2>/dev/null || /app/migrate help 2>/dev/null || echo "(migrate --help unavailable, trying direct invocation)"
  ' 2>&1
fi

section "DB state — capability_items distribution"
if [[ -n "$API_POD" ]]; then
  # Build the SQL probe as a heredoc on the bastion, then pipe via stdin to
  # `psql` running INSIDE the pod. This sidesteps the multi-layer quoting
  # nightmare of writing inline SQL inside `sh -c '...'`.
  SQL_PROBE=$(cat <<'__PROBE_SQL__'
\echo --- type / status counts (public registry) ---
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
SELECT id, name, source_type, COALESCE(external_url,'(none)') AS external_url,
       COALESCE(last_sync_sha,'(none)') AS last_sync_sha,
       last_synced_at, sync_status
  FROM capability_registries
  ORDER BY created_at;

\echo
\echo --- schema check: do critical columns exist? ---
SELECT column_name, data_type FROM information_schema.columns
  WHERE table_name='capability_items'
    AND column_name IN ('content_md5','current_revision','security_status',
                        'last_scan_id','source_sha','experience_score')
  ORDER BY column_name;

\echo
\echo --- security_scans count + scan_jobs queue ---
SELECT (SELECT count(*) FROM security_scans) AS security_scans,
       (SELECT count(*) FROM scan_jobs WHERE status='pending') AS pending_scan_jobs,
       (SELECT count(*) FROM scan_jobs WHERE status='running') AS running_scan_jobs;

\echo
\echo --- sync_logs (last 5) ---
SELECT id, registry_id, trigger_type, status, added_items, updated_items,
       deleted_items, failed_items, started_at, finished_at
  FROM sync_logs ORDER BY started_at DESC LIMIT 5;
__PROBE_SQL__
)
  # Pipe the heredoc into psql inside the pod via -i (interactive stdin).
  # The wrapper installs psql client if it's missing.
  echo "$SQL_PROBE" | kubectl -n "$NAMESPACE" exec -i "$API_POD" -- sh -c '
    PSQL=$(command -v psql)
    if [ -z "$PSQL" ]; then
      apk add --no-cache postgresql-client >/dev/null 2>&1 \
        || apt-get install -y postgresql-client >/dev/null 2>&1 \
        || true
      PSQL=$(command -v psql)
    fi
    if [ -z "$PSQL" ]; then
      echo "(no psql client available in api pod — run kubectl exec into the postgres pod for these queries)"
      exit 0
    fi
    $PSQL "$DATABASE_URL" -P pager=off -f -
  ' 2>&1
fi

section "worker deployment status (would consume new scan_jobs we create)"
kubectl -n "$NAMESPACE" get deploy worker -o yaml 2>/dev/null \
  | grep -E "replicas:|image:" | head -10 || true

section "ingress / service endpoints"
kubectl -n "$NAMESPACE" get svc 2>&1
echo "---"
kubectl -n "$NAMESPACE" get ingress 2>&1 || true

section "git refs of the in-cluster image (if any annotation set)"
kubectl -n "$NAMESPACE" get deploy api -o yaml 2>/dev/null \
  | grep -E "(git-commit|git-sha|app\.kubernetes\.io/version|image:|costrict\.ai/)" \
  | head -20

section "DONE"
echo "Pipe this output back to me to plan the upgrade & ingest steps."
