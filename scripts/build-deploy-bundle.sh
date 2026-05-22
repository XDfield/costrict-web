#!/usr/bin/env bash
# Build a deploy-bundle.tar.gz that contains everything needed on the
# bastion to upgrade api/worker + run the ingest, in a single scp'able
# artifact. Run on YOUR LAPTOP.
#
# Contents of the tarball:
#   migrate-linux-amd64
#   migrate-linux-arm64
#   catalog-bundle.tar.gz
#   deploy/charts/api/...
#   deploy/charts/worker/...
#   scripts/helm-upgrade.sh
#   scripts/probe-env.sh
#   scripts/run-ingest.sh
#   README.txt   (cheat-sheet)
#
# After:
#   scp deploy-bundle.tar.gz <bastion>:/tmp/
#   ssh <bastion>
#   cd /tmp && tar xzf deploy-bundle.tar.gz && cd deploy-bundle
#   IMAGE_TAG=0.0.61-ingest-rc1 bash scripts/helm-upgrade.sh
#   ENVIRONMENT=staging bash scripts/run-ingest.sh

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${OUT:-/tmp/deploy-bundle.tar.gz}"
SKILLS_BUNDLE="${SKILLS_BUNDLE:-/Volumes/Work/Projects/costrict-skills-repo/dist/catalog-bundle.tar.gz}"

log() { printf '[build-bundle] %s\n' "$*"; }

cd "$ROOT"

# 1. Cross-compile migrate binaries
log "compiling server/cmd/migrate for linux/amd64 + linux/arm64 ..."
(cd server && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/migrate-linux-amd64 ./cmd/migrate)
(cd server && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/migrate-linux-arm64 ./cmd/migrate)

# 2. Sanity check the catalog bundle exists
[[ -f "$SKILLS_BUNDLE" ]] || {
  echo "ERROR: catalog bundle missing at $SKILLS_BUNDLE"
  echo "run: cd $(dirname "$SKILLS_BUNDLE")/.. && python3 scripts/build_catalog_bundle.py"
  exit 1
}

# 3. Stage everything into a temp dir, then tar it up
STAGE=$(mktemp -d -t deploy-bundle.XXXXXX)
trap 'rm -rf "$STAGE"' EXIT

mkdir -p "$STAGE/deploy-bundle/scripts" \
         "$STAGE/deploy-bundle/deploy/charts"

cp /tmp/migrate-linux-amd64       "$STAGE/deploy-bundle/"
cp /tmp/migrate-linux-arm64       "$STAGE/deploy-bundle/"
cp "$SKILLS_BUNDLE"               "$STAGE/deploy-bundle/catalog-bundle.tar.gz"
cp -R deploy/charts/api           "$STAGE/deploy-bundle/deploy/charts/"
cp -R deploy/charts/worker        "$STAGE/deploy-bundle/deploy/charts/"
cp scripts/helm-upgrade.sh        "$STAGE/deploy-bundle/scripts/"
cp scripts/probe-env.sh           "$STAGE/deploy-bundle/scripts/"
cp scripts/run-ingest.sh          "$STAGE/deploy-bundle/scripts/"

cat > "$STAGE/deploy-bundle/README.txt" <<'__README__'
costrict-web deploy bundle
==========================

Contents:
  migrate-linux-amd64 / arm64    new migrate binary (with ingest-upstream subcommand)
  catalog-bundle.tar.gz          upstream catalog data to ingest
  deploy/charts/api,worker       Helm charts (synced from feat/catalog-ingest-refactor)
  scripts/helm-upgrade.sh        helm upgrade api+worker to a new image tag
  scripts/probe-env.sh           read-only env probe
  scripts/run-ingest.sh          ingest the catalog bundle

Quickstart (staging):
  1) cd /tmp && tar xzf deploy-bundle.tar.gz && cd deploy-bundle
  2) ENVIRONMENT=staging bash scripts/probe-env.sh    # eyeball cluster
  3) IMAGE_TAG=<built-tag> bash scripts/helm-upgrade.sh
  4) ENVIRONMENT=staging bash scripts/probe-env.sh    # confirm migrate help has 'ingest-upstream'
  5) cp /tmp/deploy-bundle/{migrate-linux-*,catalog-bundle.tar.gz} /tmp/
     ENVIRONMENT=staging bash scripts/run-ingest.sh   # bundle/binary read from /tmp/

For production:
  ENVIRONMENT=production bash <script>   (adds extra confirmation)
__README__

# 4. Pack
log "tarring up ..."
cd "$STAGE"
tar czf "$OUT" deploy-bundle/
log "wrote $OUT ($(du -h "$OUT" | cut -f1))"

# 5. Show what's inside, briefly
tar tzf "$OUT" | head -25
echo "..."
tar tzf "$OUT" | wc -l
echo "files total"
