#!/usr/bin/env bash
# common.sh — shared helpers for server ops scripts.
#
# Source this from any script under server/scripts/. Wires up internal
# auth (X-Internal-Secret), resolves the server base URL, and exposes
# curl + jq wrappers so individual scripts stay short.
#
# Required env (load .env first, or export explicitly):
#   INTERNAL_SECRET   — value sent as X-Internal-Secret (must match the
#                       INTERNAL_SECRET the server was started with).
#   SERVER_BASE_URL   — defaults to http://localhost:8080
#   DEFAULT_TENANT_ID — default tenant slug/id for scripts that don't
#                       take an explicit --tenant flag.
#
# Sourcing this file does NOT parse flags — call srv_resolve_tenant /
# srv_assert_secret directly inside your script.

set -euo pipefail

: "${SERVER_BASE_URL:=http://localhost:8080}"

command -v curl >/dev/null 2>&1 || { echo '[server-ops] curl not found in PATH' >&2; exit 2; }
command -v jq   >/dev/null 2>&1 || { echo '[server-ops] jq not found in PATH' >&2; exit 2; }

# Log to stderr so JSON on stdout stays parseable.
srv_log() {
    printf '[server-ops] %s\n' "$*" >&2
}

srv_die() {
    printf '[server-ops] ERROR: %s\n' "$*" >&2
    exit 1
}

# Assert INTERNAL_SECRET is set. We never log the secret itself.
srv_assert_secret() {
    if [[ -z "${INTERNAL_SECRET:-}" ]]; then
        srv_die "INTERNAL_SECRET not set — source .env or export it before running this script."
    fi
}

# Resolve the effective tenant id: explicit $1 wins, else DEFAULT_TENANT_ID,
# else error.
srv_resolve_tenant() {
    local explicit="${1:-}"
    explicit="${explicit// /}"
    if [[ -n "$explicit" ]]; then
        printf '%s' "$explicit"
        return
    fi
    if [[ -n "${DEFAULT_TENANT_ID:-}" ]]; then
        printf '%s' "$DEFAULT_TENANT_ID"
        return
    fi
    srv_die "no tenant id — pass --tenant or set DEFAULT_TENANT_ID"
}

# Authenticated GET. Args: path. Prints response body to stdout.
srv_get() {
    local path="$1"
    srv_assert_secret
    curl -sS -G "${SERVER_BASE_URL}${path}" \
        -H "X-Internal-Secret: ${INTERNAL_SECRET}" \
        -H 'Accept: application/json'
}

# Authenticated POST/PUT/DELETE with optional JSON body.
# Usage: srv_json_request <METHOD> <path> [json-body-or-empty]
# Prints "<body>\n<http_code>" — call srv_body / srv_status to split.
srv_json_request() {
    local method="$1"
    local path="$2"
    local body="${3:-}"
    srv_assert_secret
    local -a curl_args=(
        -sS -X "$method"
        "${SERVER_BASE_URL}${path}"
        -H "X-Internal-Secret: ${INTERNAL_SECRET}"
        -H 'Content-Type: application/json'
        -H 'Accept: application/json'
        -w '\n%{http_code}'
    )
    if [[ -n "$body" ]]; then
        curl_args+=(--data "$body")
    fi
    curl "${curl_args[@]}"
}

# Strip the trailing status-code line that srv_json_request appends and
# echo only the body. Args: full_output.
srv_body() {
    printf '%s' "$1" | sed '$d'
}

# Echo only the trailing HTTP status code.
srv_status() {
    printf '%s' "$1" | tail -n1
}

# Pretty-print JSON on stdin via jq.
srv_pretty() {
    jq . 2>/dev/null || cat
}
