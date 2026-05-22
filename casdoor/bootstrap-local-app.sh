#!/usr/bin/env bash
# 本地 Casdoor 启动后调用 REST API 创建 costrict-local application。
# 幂等：如果 application 已存在则 no-op。
#
# 用法：
#   ./casdoor/bootstrap-local-app.sh
#
# 前置：casdoor 已 docker compose up 起来，且监听在 CASDOOR_ENDPOINT（默认 http://localhost:8000）。

set -euo pipefail

CASDOOR_ENDPOINT="${CASDOOR_ENDPOINT:-http://localhost:8000}"
ADMIN_USER="${CASDOOR_ADMIN_USER:-admin}"
ADMIN_PASS="${CASDOOR_ADMIN_PASS:-123}"
ADMIN_ORG="${CASDOOR_ADMIN_ORG:-built-in}"
ADMIN_APP="${CASDOOR_ADMIN_APP:-app-built-in}"

APP_NAME="${APP_NAME:-costrict-local}"
CLIENT_ID="${CLIENT_ID:-costrict-local-client-id}"
CLIENT_SECRET="${CLIENT_SECRET:-costrict-local-client-secret-do-not-use-in-prod}"

COOKIE_JAR="$(mktemp)"
trap 'rm -f "$COOKIE_JAR"' EXIT

log() { printf '[bootstrap] %s\n' "$*"; }

log "casdoor endpoint: $CASDOOR_ENDPOINT"

# 1. wait until casdoor is reachable
for _ in $(seq 1 30); do
  if curl -fsS -o /dev/null "$CASDOOR_ENDPOINT/api/get-applications"; then
    break
  fi
  sleep 1
done

# 2. login as built-in admin
log "login as $ADMIN_ORG/$ADMIN_USER ..."
login_resp=$(curl -fsS -c "$COOKIE_JAR" -X POST "$CASDOOR_ENDPOINT/api/login" \
  -H "Content-Type: application/json" \
  -d "{\"organization\":\"$ADMIN_ORG\",\"username\":\"$ADMIN_USER\",\"password\":\"$ADMIN_PASS\",\"type\":\"login\",\"application\":\"$ADMIN_APP\"}")
if echo "$login_resp" | grep -q '"status":[[:space:]]*"ok"'; then
  log "login ok"
else
  log "login failed: $login_resp"; exit 1
fi

# 3. check if application already exists (idempotent)
existing=$(curl -fsS -b "$COOKIE_JAR" "$CASDOOR_ENDPOINT/api/get-application?id=admin/$APP_NAME")
if echo "$existing" | grep -q '"data":[[:space:]]*null'; then
  log "application admin/$APP_NAME not found, creating ..."
else
  log "application admin/$APP_NAME already exists, skipping create"
  exit 0
fi

# 4. POST add-application
# NOTE: 实测 casbin/casdoor:latest 对完整 schema payload 会静默拒绝（response status=ok
# 但 INSERT 没发生）。保持 payload 最小化才能稳定 affect 一行 —— 缺失字段由 casdoor 用
# DB 默认值填充。
payload=$(cat <<EOF
{
  "owner": "admin",
  "name": "$APP_NAME",
  "organization": "built-in",
  "cert": "cert-built-in",
  "displayName": "Costrict Local Dev",
  "clientId": "$CLIENT_ID",
  "clientSecret": "$CLIENT_SECRET",
  "redirectUris": [
    "http://localhost:8080/api/auth/callback",
    "http://localhost:3000/api/auth/callback",
    "http://127.0.0.1:3000/api/auth/callback",
    "http://localhost:3000/callback"
  ],
  "grantTypes": ["authorization_code", "refresh_token"],
  "enablePassword": true,
  "enableSignUp": true,
  "tokenFormat": "JWT",
  "expireInHours": 168,
  "refreshExpireInHours": 168
}
EOF
)

add_resp=$(curl -fsS -b "$COOKIE_JAR" -X POST "$CASDOOR_ENDPOINT/api/add-application" \
  -H "Content-Type: application/json" \
  -d "$payload")
if echo "$add_resp" | grep -q '"status":[[:space:]]*"ok"'; then
  log "application admin/$APP_NAME created"
else
  log "add-application failed: $add_resp"; exit 1
fi

# 5. verify clientId persisted as expected (casdoor will sometimes regenerate)
verify=$(curl -fsS -b "$COOKIE_JAR" "$CASDOOR_ENDPOINT/api/get-application?id=admin/$APP_NAME")
if echo "$verify" | grep -q "\"clientId\":[[:space:]]*\"$CLIENT_ID\""; then
  log "verified clientId=$CLIENT_ID"
else
  actual=$(echo "$verify" | sed -n 's/.*"clientId":[[:space:]]*"\([^"]*\)".*/\1/p')
  log "WARN: clientId mismatch — wanted=$CLIENT_ID actual=$actual"
  log "update .env.local with actual=$actual or rerun after deleting the app"
  exit 1
fi

log "done."
