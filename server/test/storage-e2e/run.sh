#!/usr/bin/env bash

set -Eeuo pipefail

readonly MINIO_IMAGE="quay.io/minio/minio:RELEASE.2024-06-13T22-53-53Z"
readonly MC_IMAGE="quay.io/minio/mc:RELEASE.2024-06-12T14-34-03Z"
readonly MINIO_ROOT_USER="costrict-e2e-root"
readonly MINIO_ROOT_PASSWORD="costrict-e2e-root-secret"
readonly APP_ACCESS_KEY="costrict-e2e-app"
readonly APP_SECRET_KEY="costrict-e2e-app-secret"
readonly BUCKET="costrict-e2e"
readonly REGION="us-east-1"
readonly TEST_NAME="TestS3CatalogSkillDistribution"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
SERVER_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
readonly SERVER_DIR
readonly POLICY_FILE="${SCRIPT_DIR}/minio-policy.json"

readonly RUN_SUFFIX="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-$$"
readonly NETWORK_NAME="costrict-s3e2e-${RUN_SUFFIX}"
readonly MINIO_CONTAINER="costrict-s3e2e-minio-${RUN_SUFFIX}"
readonly INIT_CONTAINER="costrict-s3e2e-init-${RUN_SUFFIX}"
readonly PERMISSION_CONTAINER="costrict-s3e2e-permission-${RUN_SUFFIX}"

cleanup() {
  local status=$?
  trap - EXIT INT TERM

  if (( status != 0 )) &&
    docker inspect "${MINIO_CONTAINER}" >/dev/null 2>&1; then
    echo "Storage E2E failed; MinIO logs follow:" >&2
    docker logs "${MINIO_CONTAINER}" >&2 2>/dev/null || true
  fi

  docker rm -f "${INIT_CONTAINER}" >/dev/null 2>&1 || true
  docker rm -f "${PERMISSION_CONTAINER}" >/dev/null 2>&1 || true
  docker rm -f "${MINIO_CONTAINER}" >/dev/null 2>&1 || true
  docker network rm "${NETWORK_NAME}" >/dev/null 2>&1 || true
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required to run the S3 E2E test" >&2
  exit 1
fi
if [[ ! -f "${POLICY_FILE}" ]]; then
  echo "MinIO policy not found: ${POLICY_FILE}" >&2
  exit 1
fi

cd "${SERVER_DIR}"
if ! go test -tags=s3e2e ./internal/handlers -list "^${TEST_NAME}$" |
  grep -x "${TEST_NAME}" >/dev/null; then
  echo "tagged E2E test not found: ${TEST_NAME}" >&2
  exit 1
fi

docker network create "${NETWORK_NAME}" >/dev/null

docker run --detach \
  --name "${MINIO_CONTAINER}" \
  --network "${NETWORK_NAME}" \
  --network-alias minio \
  --publish "127.0.0.1::9000" \
  --env "MINIO_ROOT_USER=${MINIO_ROOT_USER}" \
  --env "MINIO_ROOT_PASSWORD=${MINIO_ROOT_PASSWORD}" \
  "${MINIO_IMAGE}" server /data >/dev/null

docker run --rm \
  --name "${INIT_CONTAINER}" \
  --network "${NETWORK_NAME}" \
  --volume "${POLICY_FILE}:/tmp/costrict-put-get-policy.json:ro" \
  --env "MC_HOST_e2e=http://${MINIO_ROOT_USER}:${MINIO_ROOT_PASSWORD}@minio:9000" \
  --env "APP_ACCESS_KEY=${APP_ACCESS_KEY}" \
  --env "APP_SECRET_KEY=${APP_SECRET_KEY}" \
  --env "BUCKET=${BUCKET}" \
  --entrypoint /bin/sh \
  "${MC_IMAGE}" -ceu '
    attempts=0
    until mc admin info e2e >/dev/null 2>&1; do
      attempts=$((attempts + 1))
      if [ "$attempts" -ge 60 ]; then
        echo "MinIO did not become ready within 60 seconds" >&2
        exit 1
      fi
      sleep 1
    done
    mc mb --ignore-existing "e2e/${BUCKET}"
    mc admin policy create e2e costrict-put-get /tmp/costrict-put-get-policy.json
    mc admin user add e2e "${APP_ACCESS_KEY}" "${APP_SECRET_KEY}"
    mc admin policy attach e2e costrict-put-get --user "${APP_ACCESS_KEY}"
  '

LIST_CHECK_OUTPUT=""
if LIST_CHECK_OUTPUT="$(
  docker run --rm \
    --name "${PERMISSION_CONTAINER}" \
    --network "${NETWORK_NAME}" \
    --env "MC_HOST_app=http://${APP_ACCESS_KEY}:${APP_SECRET_KEY}@minio:9000" \
    "${MC_IMAGE}" ls "app/${BUCKET}" 2>&1
)"; then
  echo "application credentials unexpectedly permit S3 ListBucket" >&2
  exit 1
fi
if ! grep -Eiq 'access[[:space:]]*denied|accessdenied|not authorized|forbidden' \
  <<<"${LIST_CHECK_OUTPUT}"; then
  echo "application ListBucket check failed for an unexpected reason:" >&2
  echo "${LIST_CHECK_OUTPUT}" >&2
  exit 1
fi
echo "Verified application credentials deny S3 ListBucket"

MINIO_PORT="$(
  docker port "${MINIO_CONTAINER}" 9000/tcp |
    awk -F: 'NR == 1 { print $NF }'
)"
readonly MINIO_PORT
if [[ ! "${MINIO_PORT}" =~ ^[0-9]+$ ]]; then
  echo "failed to resolve the published MinIO port: ${MINIO_PORT}" >&2
  exit 1
fi

env \
  ARTIFACT_STORAGE_BACKEND=s3 \
  S3_ENDPOINT="http://127.0.0.1:${MINIO_PORT}" \
  S3_BUCKET="${BUCKET}" \
  S3_REGION="${REGION}" \
  S3_FORCE_PATH_STYLE=true \
  AWS_ACCESS_KEY_ID="${APP_ACCESS_KEY}" \
  AWS_SECRET_ACCESS_KEY="${APP_SECRET_KEY}" \
  AWS_SESSION_TOKEN= \
  AWS_EC2_METADATA_DISABLED=true \
  go test -tags=s3e2e ./internal/handlers \
    -run "^${TEST_NAME}$" \
    -count=1 \
    -v
