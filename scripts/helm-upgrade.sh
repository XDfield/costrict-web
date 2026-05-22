#!/usr/bin/env bash
# Upgrade api + worker (optionally portal) deployments in costrict-web ns
# to a target image tag built from our branch.
#
# Run ON THE BASTION HOST. Assumes:
#   - kubectl + helm on PATH
#   - kubectl context pointing at the intended cluster
#   - deploy/charts/api and deploy/charts/worker available next to this
#     script (e.g. via the deploy-bundle tarball you scp'd up)
#
# Usage:
#   IMAGE_TAG=0.0.61-ingest-rc1 bash helm-upgrade.sh
#   ENVIRONMENT=production IMAGE_TAG=0.0.61-ingest-rc1 bash helm-upgrade.sh
#   SERVICES=api,worker bash helm-upgrade.sh    # default
#   SERVICES=api         bash helm-upgrade.sh   # only api

set -euo pipefail

ENVIRONMENT="${ENVIRONMENT:-staging}"
NAMESPACE="${NAMESPACE:-costrict-web}"
IMAGE_TAG="${IMAGE_TAG:?IMAGE_TAG required (e.g. 0.0.61-ingest-rc1)}"
SERVICES="${SERVICES:-api,worker}"
CHART_ROOT="${CHART_ROOT:-$(cd "$(dirname "$0")/../deploy/charts" 2>/dev/null && pwd || echo "")}"

# Helm release name pattern in this cluster (verified via probe):
#   chart: api      → release: api      → deploy/api-costrict-web-api
#   chart: worker   → release: worker   → deploy/worker-costrict-web-worker
declare -A RELEASE_OF
RELEASE_OF[api]=api
RELEASE_OF[worker]=worker
RELEASE_OF[portal]=portal

log() { printf '\n[helm-upgrade:%s] %s\n' "$ENVIRONMENT" "$*"; }
die() { printf '\n[helm-upgrade:%s] ERROR: %s\n' "$ENVIRONMENT" "$*" >&2; exit 1; }

command -v kubectl >/dev/null 2>&1 || die "kubectl not on PATH"
command -v helm   >/dev/null 2>&1 || die "helm not on PATH"
[[ -d "$CHART_ROOT" ]] || die "chart root not found: $CHART_ROOT (set CHART_ROOT=)"

log "summary"
echo "  ENVIRONMENT:     $ENVIRONMENT"
echo "  namespace:       $NAMESPACE"
echo "  IMAGE_TAG:       $IMAGE_TAG"
echo "  SERVICES:        $SERVICES"
echo "  CHART_ROOT:      $CHART_ROOT"
echo "  kubectl context: $(kubectl config current-context 2>/dev/null || echo unknown)"
echo "  cluster server:  $(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null || echo unknown)"
echo ""
case "$ENVIRONMENT" in
  prod|production)
    echo "▸▸▸ PRODUCTION TARGET ◀◀◀"
    read -r -p "[helm-upgrade:production] type 'I understand' to continue: " ANSWER
    [[ "$ANSWER" == "I understand" ]] || die "production confirmation failed"
    ;;
esac

# Iterate services
IFS=',' read -r -a SVC_ARR <<< "$SERVICES"
for svc in "${SVC_ARR[@]}"; do
  svc=$(echo "$svc" | xargs)  # trim
  release="${RELEASE_OF[$svc]:-}"
  if [[ -z "$release" ]]; then
    die "unknown service: $svc (expected one of: ${!RELEASE_OF[*]})"
  fi
  chart_dir="${CHART_ROOT}/${svc}"
  [[ -d "$chart_dir" ]] || die "chart dir not found: $chart_dir"

  log "upgrading $svc → tag=$IMAGE_TAG (release=$release, chart=$chart_dir)"

  # --reuse-values keeps every value the cluster currently uses (envFrom
  # secrets, resource limits, ingress hosts …) and only overrides
  # image.tag. Safer than --set <everything> from scratch.
  helm -n "$NAMESPACE" upgrade "$release" "$chart_dir" \
    --reuse-values \
    --set "image.tag=$IMAGE_TAG"

  log "waiting for rollout ..."
  kubectl -n "$NAMESPACE" rollout status "deploy/${release}-costrict-web-${svc}" --timeout=180s
done

log "verifying images on running pods"
kubectl -n "$NAMESPACE" get deploy -o custom-columns=NAME:.metadata.name,IMAGE:.spec.template.spec.containers[*].image

log "done."
