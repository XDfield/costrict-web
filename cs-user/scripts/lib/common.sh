#!/usr/bin/env bash
# common.sh — shared helpers for cs-user ops scripts.
#
# Source this from any script under cs-user/scripts/. It wires up auth,
# resolves the cs-user base URL, and exposes curl + JSON pretty-printing
# wrappers so individual scripts stay short.
#
# Required env (load .env first, or export explicitly):
#   CS_USER_INTERNAL_TOKEN   — value sent as X-Internal-Token (matches the
#                              token the cs-user API expects; usually
#                              shared with @server's INTERNAL_SECRET).
#   CS_USER_BASE_URL         — defaults to http://localhost:8081
#   CS_USER_TENANT_ID        — default tenant for scripts that don't take
#                              an explicit --tenant flag.
#
# Sourcing this file does NOT parse flags — call csu_parse_args after
# defining your flag set, or just use csu_resolve_tenant / csu_assert_token
# directly inside your script.

set -euo pipefail

: "${CS_USER_BASE_URL:=http://localhost:8081}"

# Log to stderr so JSON on stdout stays parseable.
csu_log() {
    printf '[cs-user-ops] %s\n' "$*" >&2
}

csu_die() {
    printf '[cs-user-ops] ERROR: %s\n' "$*" >&2
    exit 1
}

# Assert CS_USER_INTERNAL_TOKEN is set. We never log the token itself.
csu_assert_token() {
    if [[ -z "${CS_USER_INTERNAL_TOKEN:-}" ]]; then
        csu_die "CS_USER_INTERNAL_TOKEN not set — source .env or export it before running this script."
    fi
}

# Resolve the effective tenant id: explicit $1 wins, else CS_USER_TENANT_ID,
# else error. Trims surrounding whitespace.
csu_resolve_tenant() {
    local explicit="${1:-}"
    explicit="${explicit// /}"
    if [[ -n "$explicit" ]]; then
        printf '%s' "$explicit"
        return
    fi
    if [[ -n "${CS_USER_TENANT_ID:-}" ]]; then
        printf '%s' "$CS_USER_TENANT_ID"
        return
    fi
    csu_die "no tenant id — pass --tenant or set CS_USER_TENANT_ID"
}

# Authenticated GET. Args: path. Prints response body to stdout.
csu_get() {
    local path="$1"
    csu_assert_token
    curl -sS -G "${CS_USER_BASE_URL}${path}" \
        -H "X-Internal-Token: ${CS_USER_INTERNAL_TOKEN}" \
        -H 'Accept: application/json'
}

# Authenticated POST/PUT/DELETE with JSON body.
# Usage: csu_json_request <METHOD> <path> [json-body-or-empty]
csu_json_request() {
    local method="$1"
    local path="$2"
    local body="${3:-}"
    csu_assert_token
    local -a curl_args=(
        -sS -X "$method"
        "${CS_USER_BASE_URL}${path}"
        -H "X-Internal-Token: ${CS_USER_INTERNAL_TOKEN}"
        -H 'Content-Type: application/json'
        -H 'Accept: application/json'
    )
    if [[ -n "$body" ]]; then
        curl_args+=(--data "$body")
    fi
    curl "${curl_args[@]}"
}

# Run JSON through python -m json.tool if python is on PATH; otherwise cat.
# Keeps responses readable without requiring jq.
csu_pretty() {
    if command -v python >/dev/null 2>&1; then
        python -m json.tool 2>/dev/null || cat
    elif command -v jq >/dev/null 2>&1; then
        jq . 2>/dev/null || cat
    else
        cat
    fi
}

# Read a file's contents to stdout, failing if missing.
csu_read_file() {
    local f="$1"
    [[ -f "$f" ]] || csu_die "file not found: $f"
    cat "$f"
}
