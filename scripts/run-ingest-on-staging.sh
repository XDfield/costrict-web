#!/usr/bin/env bash
# Run the catalog ingest against the test environment (namespace=costrict-web).
#
# Run this ON THE BASTION HOST (root@10.20.19.2). Three artifacts must
# already sit in /tmp/ before invoking (scp from your laptop):
#
#   /tmp/migrate-linux-amd64           or migrate-linux-arm64
#   /tmp/migrate-linux-arm64           (only one is needed; script auto-picks)
#   /tmp/catalog-bundle.tar.gz
#
# Defaults assume:
#   - namespace = costrict-web
#   - api pod selector = app.kubernetes.io/name=api
#
# Usage:
#   bash run-ingest-on-staging.sh                  # full ingest (dry-run first, then real)
#   DRY_RUN_ONLY=1 bash run-ingest-on-staging.sh   # stop after dry-run
#   NAMESPACE=foo bash run-ingest-on-staging.sh    # override namespace

set -euo pipefail

NAMESPACE="${NAMESPACE:-costrict-web}"
# Verified on staging 2026-05-21: the chart uses the full app name as the label.
API_SELECTOR="${API_SELECTOR:-app.kubernetes.io/name=costrict-web-api}"
BUNDLE_LOCAL="${BUNDLE_LOCAL:-/tmp/catalog-bundle.tar.gz}"
DRY_RUN_ONLY="${DRY_RUN_ONLY:-0}"

log() { printf '\n[ingest-staging] %s\n' "$*"; }
die() { printf '\n[ingest-staging] ERROR: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# 0. Pre-flight
# ---------------------------------------------------------------------------
command -v kubectl >/dev/null 2>&1 || die "kubectl not on PATH"
[[ -f "$BUNDLE_LOCAL" ]] || die "bundle not found: $BUNDLE_LOCAL (scp it first)"

# Pick the right migrate binary for the cluster's arch.
NODE_ARCH=$(kubectl -n "$NAMESPACE" get nodes \
  -o jsonpath='{.items[0].status.nodeInfo.architecture}' 2>/dev/null || echo "")
if [[ -z "$NODE_ARCH" ]]; then
  die "could not detect node arch — is kubectl context pointing at the right cluster?"
fi
case "$NODE_ARCH" in
  amd64|x86_64) MIGRATE_LOCAL="/tmp/migrate-linux-amd64" ;;
  arm64|aarch64) MIGRATE_LOCAL="/tmp/migrate-linux-arm64" ;;
  *) die "unsupported node arch: $NODE_ARCH" ;;
esac
[[ -f "$MIGRATE_LOCAL" ]] || die "migrate binary not found for $NODE_ARCH: $MIGRATE_LOCAL"
log "node arch: $NODE_ARCH → using $MIGRATE_LOCAL"

# Pick a healthy api pod.
API_POD=$(kubectl -n "$NAMESPACE" get pod -l "$API_SELECTOR" \
  -o jsonpath='{.items[?(@.status.phase=="Running")].metadata.name}' \
  | awk '{print $1}')
[[ -n "$API_POD" ]] || die "no Running api pod matched selector $API_SELECTOR in ns=$NAMESPACE"
log "target pod: $NAMESPACE/$API_POD"

# Confirm DATABASE_URL is set in the pod (otherwise migrate will hit the
# default localhost connection string and silently no-op or hang).
kubectl -n "$NAMESPACE" exec "$API_POD" -- sh -c 'test -n "$DATABASE_URL"' \
  || die "DATABASE_URL not set on $API_POD — make sure you picked the right deployment"
log "DATABASE_URL is set on pod"

# ---------------------------------------------------------------------------
# 1. Stage files into the pod (/tmp is emptyDir, scrubbed on pod restart)
# ---------------------------------------------------------------------------
log "copying migrate + bundle into pod ..."
kubectl -n "$NAMESPACE" cp "$MIGRATE_LOCAL"  "${API_POD}:/tmp/migrate-ingest"
kubectl -n "$NAMESPACE" cp "$BUNDLE_LOCAL"   "${API_POD}:/tmp/catalog-bundle.tar.gz"
kubectl -n "$NAMESPACE" exec "$API_POD" -- chmod +x /tmp/migrate-ingest

# ---------------------------------------------------------------------------
# 2. Dry-run first — always.
# ---------------------------------------------------------------------------
log "dry-run (no DB writes) ..."
kubectl -n "$NAMESPACE" exec "$API_POD" -- \
  /tmp/migrate-ingest ingest-upstream \
  --source=/tmp/catalog-bundle.tar.gz --dry-run \
  | tee /tmp/ingest-dryrun.out

# Sanity check: bail if dry-run reports failed > 0
if grep -qE "failed=[1-9]" /tmp/ingest-dryrun.out; then
  die "dry-run reported failed > 0 — STOP and investigate before real ingest"
fi

if [[ "$DRY_RUN_ONLY" == "1" ]]; then
  log "DRY_RUN_ONLY=1, stopping here. Pod still has /tmp/migrate-ingest + bundle."
  exit 0
fi

# Ask for explicit confirmation before mutating the DB.
read -r -p "[ingest-staging] dry-run OK. Proceed with REAL ingest into ns=$NAMESPACE? (yes/no) " ANSWER
[[ "$ANSWER" == "yes" ]] || die "aborted by user"

# ---------------------------------------------------------------------------
# 3. Real ingest + capture incomplete log for review.
# ---------------------------------------------------------------------------
log "real ingest ..."
kubectl -n "$NAMESPACE" exec "$API_POD" -- \
  sh -c 'INGEST_ERROR_LOG=/tmp/ingest-errors.log /tmp/migrate-ingest ingest-upstream --source=/tmp/catalog-bundle.tar.gz' \
  | tee /tmp/ingest-real.out

# Pull the error log back even on success — useful to see the incomplete tail.
if kubectl -n "$NAMESPACE" exec "$API_POD" -- test -f /tmp/ingest-errors.log 2>/dev/null; then
  kubectl -n "$NAMESPACE" cp "${API_POD}:/tmp/ingest-errors.log" /tmp/ingest-errors.log
  log "incomplete/failed lines copied to /tmp/ingest-errors.log on bastion"
fi

# ---------------------------------------------------------------------------
# 4. Quick DB sanity check via the pod's own DATABASE_URL.
# ---------------------------------------------------------------------------
log "DB type distribution (active items in public registry):"
kubectl -n "$NAMESPACE" exec "$API_POD" -- sh -c '
  # try common psql client locations / install paths
  PSQL=$(command -v psql || true)
  if [ -z "$PSQL" ]; then
    apk add --no-cache postgresql-client >/dev/null 2>&1 \
      || apt-get install -y postgresql-client >/dev/null 2>&1 \
      || true
    PSQL=$(command -v psql || true)
  fi
  if [ -z "$PSQL" ]; then
    echo "(psql not available in pod, skip; run kubectl exec into postgres pod to verify)"
    exit 0
  fi
  $PSQL "$DATABASE_URL" -P pager=off -c "
    SELECT item_type, status, count(*)
      FROM capability_items
      WHERE registry_id = '\''00000000-0000-0000-0000-000000000001'\''
      GROUP BY 1, 2
      ORDER BY 1, 2;
  "
'

# ---------------------------------------------------------------------------
# 5. Cleanup pod /tmp so nothing lingers
# ---------------------------------------------------------------------------
log "cleaning up files in pod ..."
kubectl -n "$NAMESPACE" exec "$API_POD" -- rm -f \
  /tmp/migrate-ingest /tmp/catalog-bundle.tar.gz /tmp/ingest-errors.log || true

log "done."
log "  dry-run log:  /tmp/ingest-dryrun.out"
log "  real-run log: /tmp/ingest-real.out"
log "  errors:       /tmp/ingest-errors.log (if any)"
