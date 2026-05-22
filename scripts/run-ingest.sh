#!/usr/bin/env bash
# Run catalog-bundle ingest against a costrict-web cluster.
# Runs ON THE BASTION HOST (kubectl already points at the target cluster).
#
# Environments — picks up sensible defaults + adds prod safety prompt:
#   ENVIRONMENT=staging     (default)   → https://zgsmtest.cn:30443/cloud
#   ENVIRONMENT=production              → https://zgsm.sangfor.com/cloud
#
# Required files in /tmp/ on the bastion (scp from your laptop first):
#   /tmp/catalog-bundle.tar.gz
#
# Optional (only used when the api pod's bundled /app/migrate doesn't
# support `ingest-upstream` yet — e.g. running against an old image):
#   /tmp/migrate-linux-amd64   AND/OR   /tmp/migrate-linux-arm64
#
# Usage:
#   bash run-ingest.sh                              # staging, full flow
#   ENVIRONMENT=production bash run-ingest.sh       # prod, extra confirm
#   DRY_RUN_ONLY=1 bash run-ingest.sh               # stop after dry-run
#   FORCE_LOCAL_MIGRATE=1 bash run-ingest.sh        # always use local binary, skip pod-side detection
#   NAMESPACE=foo API_LABEL_NAME=bar bash …         # overrides

set -euo pipefail

ENVIRONMENT="${ENVIRONMENT:-staging}"
NAMESPACE="${NAMESPACE:-costrict-web}"
API_LABEL_NAME="${API_LABEL_NAME:-costrict-web-api}"
BUNDLE_LOCAL="${BUNDLE_LOCAL:-/tmp/catalog-bundle.tar.gz}"
DRY_RUN_ONLY="${DRY_RUN_ONLY:-0}"
FORCE_LOCAL_MIGRATE="${FORCE_LOCAL_MIGRATE:-0}"

case "$ENVIRONMENT" in
  staging|prod|production) ;;
  *) echo "ERROR: ENVIRONMENT must be staging|production, got '$ENVIRONMENT'" >&2; exit 2 ;;
esac

log() { printf '\n[ingest:%s] %s\n' "$ENVIRONMENT" "$*"; }
die() { printf '\n[ingest:%s] ERROR: %s\n' "$ENVIRONMENT" "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# 0. Pre-flight
# ---------------------------------------------------------------------------
command -v kubectl >/dev/null 2>&1 || die "kubectl not on PATH"
[[ -f "$BUNDLE_LOCAL" ]] || die "bundle not found: $BUNDLE_LOCAL (scp it first)"

log "environment summary"
echo "  ENVIRONMENT:     $ENVIRONMENT"
echo "  namespace:       $NAMESPACE"
echo "  api label:       app.kubernetes.io/name=$API_LABEL_NAME"
echo "  kubectl context: $(kubectl config current-context 2>/dev/null || echo unknown)"
echo "  cluster server:  $(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null || echo unknown)"
echo "  bundle:          $BUNDLE_LOCAL ($(wc -c < "$BUNDLE_LOCAL") bytes)"
echo ""
case "$ENVIRONMENT" in
  prod|production)
    echo "▸▸▸ PRODUCTION TARGET ◀◀◀"
    echo "▸ Frontend:        https://zgsm.sangfor.com/cloud"
    echo "▸ This will MUTATE production capability_items. Pause and confirm."
    read -r -p "[ingest:production] type 'I understand' to continue: " ANSWER
    [[ "$ANSWER" == "I understand" ]] || die "production confirmation failed"
    ;;
  staging)
    echo "▸ Frontend: https://zgsmtest.cn:30443/cloud"
    ;;
esac

# Pick a Running api pod.
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
[[ -n "$API_POD" ]] || die "no Running api pod found in ns=$NAMESPACE"
log "target pod: $NAMESPACE/$API_POD"

# Confirm DATABASE_URL exists in the pod (avoid silently hitting localhost).
kubectl -n "$NAMESPACE" exec "$API_POD" -- sh -c 'test -n "$DATABASE_URL"' \
  || die "DATABASE_URL not set on $API_POD — wrong deployment?"
log "DATABASE_URL is set on pod"

# ---------------------------------------------------------------------------
# 1. Decide which migrate binary to use
# ---------------------------------------------------------------------------
# Preference order:
#   1. Pod's bundled /app/migrate if it advertises `ingest-upstream` in
#      its help output — this is the normal post-upgrade path and avoids
#      the cross-compile + scp + cp round-trip entirely.
#   2. Local binary from /tmp/migrate-linux-{amd64,arm64} — fallback for
#      "the cluster image is older than this script" or when forced via
#      FORCE_LOCAL_MIGRATE=1.
MIGRATE_IN_POD="/app/migrate"
if [[ "$FORCE_LOCAL_MIGRATE" == "1" ]]; then
  POD_HAS_INGEST=0
  log "FORCE_LOCAL_MIGRATE=1 — skipping pod migrate detection"
elif kubectl -n "$NAMESPACE" exec "$API_POD" -- /app/migrate help 2>&1 \
       | grep -q 'ingest-upstream'; then
  POD_HAS_INGEST=1
  log "pod's /app/migrate already supports ingest-upstream — using it directly"
else
  POD_HAS_INGEST=0
  log "pod's /app/migrate is too old (no ingest-upstream subcommand) — will upload local binary"
fi

if [[ "$POD_HAS_INGEST" == "0" ]]; then
  NODE_ARCH=$(kubectl get nodes \
    -o jsonpath='{.items[0].status.nodeInfo.architecture}' 2>/dev/null || echo "")
  [[ -n "$NODE_ARCH" ]] || die "could not detect node arch — kubectl context wrong?"
  case "$NODE_ARCH" in
    amd64|x86_64) MIGRATE_LOCAL="/tmp/migrate-linux-amd64" ;;
    arm64|aarch64) MIGRATE_LOCAL="/tmp/migrate-linux-arm64" ;;
    *) die "unsupported node arch: $NODE_ARCH" ;;
  esac
  [[ -f "$MIGRATE_LOCAL" ]] || die "migrate binary not found for $NODE_ARCH: $MIGRATE_LOCAL — scp it to the bastion or upgrade the api image first"
  log "node arch: $NODE_ARCH → uploading $MIGRATE_LOCAL"

  MIGRATE_IN_POD="/tmp/migrate-ingest"
  kubectl -n "$NAMESPACE" cp "$MIGRATE_LOCAL" "${API_POD}:${MIGRATE_IN_POD}"
  kubectl -n "$NAMESPACE" exec "$API_POD" -- chmod +x "$MIGRATE_IN_POD"
fi

# ---------------------------------------------------------------------------
# 2. Stage the bundle into the pod (/tmp is emptyDir, scrubbed on restart)
# ---------------------------------------------------------------------------
log "copying bundle into pod ..."
kubectl -n "$NAMESPACE" cp "$BUNDLE_LOCAL" "${API_POD}:/tmp/catalog-bundle.tar.gz"

# ---------------------------------------------------------------------------
# 2. Dry-run first — always.
# ---------------------------------------------------------------------------
log "dry-run (no DB writes) ..."
kubectl -n "$NAMESPACE" exec "$API_POD" -- \
  "$MIGRATE_IN_POD" ingest-upstream \
  --source=/tmp/catalog-bundle.tar.gz --dry-run \
  | tee /tmp/ingest-dryrun-${ENVIRONMENT}.out

if grep -qE "failed=[1-9]" /tmp/ingest-dryrun-${ENVIRONMENT}.out; then
  die "dry-run reported failed > 0 — STOP and investigate"
fi

if [[ "$DRY_RUN_ONLY" == "1" ]]; then
  log "DRY_RUN_ONLY=1, stopping here. Pod still has /tmp/migrate-ingest + bundle."
  exit 0
fi

# Confirm before mutating the DB. Prod already confirmed once above.
read -r -p "[ingest:$ENVIRONMENT] dry-run OK. Proceed with REAL ingest? (yes/no) " ANSWER
[[ "$ANSWER" == "yes" ]] || die "aborted by user"

# ---------------------------------------------------------------------------
# 3. Real ingest + capture error log for review.
# ---------------------------------------------------------------------------
log "real ingest ..."
kubectl -n "$NAMESPACE" exec "$API_POD" -- \
  env INGEST_ERROR_LOG=/tmp/ingest-errors.log "$MIGRATE_IN_POD" ingest-upstream --source=/tmp/catalog-bundle.tar.gz \
  | tee /tmp/ingest-real-${ENVIRONMENT}.out

if kubectl -n "$NAMESPACE" exec "$API_POD" -- test -f /tmp/ingest-errors.log 2>/dev/null; then
  kubectl -n "$NAMESPACE" cp "${API_POD}:/tmp/ingest-errors.log" /tmp/ingest-errors-${ENVIRONMENT}.log
  log "error/incomplete lines copied to /tmp/ingest-errors-${ENVIRONMENT}.log"
fi

# ---------------------------------------------------------------------------
# 4. Quick DB sanity check via the pod's own DATABASE_URL.
# ---------------------------------------------------------------------------
log "DB type distribution (active items in public registry):"
kubectl -n "$NAMESPACE" exec "$API_POD" -- sh -c '
  PSQL=$(command -v psql || true)
  if [ -z "$PSQL" ]; then
    apk add --no-cache postgresql-client >/dev/null 2>&1 \
      || apt-get install -y postgresql-client >/dev/null 2>&1 \
      || true
    PSQL=$(command -v psql || true)
  fi
  if [ -z "$PSQL" ]; then
    echo "(psql unavailable in pod; verify via postgres pod manually)"
    exit 0
  fi
  $PSQL "$DATABASE_URL" -P pager=off -c "
    SELECT item_type, status, count(*)
      FROM capability_items
      WHERE registry_id='\''00000000-0000-0000-0000-000000000001'\''
      GROUP BY 1, 2 ORDER BY 1, 2;"
'

# ---------------------------------------------------------------------------
# 5. Cleanup pod /tmp
# ---------------------------------------------------------------------------
log "cleaning up files in pod ..."
# /tmp/migrate-ingest only exists when we uploaded a local binary; rm -f is
# a no-op when the pod ran /app/migrate directly. Safe either way.
kubectl -n "$NAMESPACE" exec "$API_POD" -- rm -f \
  /tmp/migrate-ingest /tmp/catalog-bundle.tar.gz /tmp/ingest-errors.log || true

log "done. logs on bastion:"
echo "  dry-run:  /tmp/ingest-dryrun-${ENVIRONMENT}.out"
echo "  real:     /tmp/ingest-real-${ENVIRONMENT}.out"
echo "  errors:   /tmp/ingest-errors-${ENVIRONMENT}.log"
