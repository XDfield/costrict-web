#!/bin/bash
# ============================================================
# cs-cloud Release Publish Script (精简版)
# ============================================================
# 功能：
#   固定包名列表，直接组装下载地址，调用 API 创建版本记录
#
# 下载地址拼接：
#   ${DOWNLOAD_BASE_URL}/${VERSION}/${filename}
#
# 本地目录模式（--local-dir）：
#   从本地目录读取包文件计算 SHA256 和大小，downloadUrl 仍使用
#   DOWNLOAD_BASE_URL 拼接（服务端需要下载地址）
#
# API 路由：
#   POST ${API_BASE_URL}/releases  (SystemToken 鉴权)
#
# 固定包列表:
#   darwin-amd64  -> cs-cloud-darwin-amd64.tar.gz
#   darwin-arm64  -> cs-cloud-darwin-arm64.tar.gz
#   linux-amd64   -> cs-cloud-linux-amd64.tar.gz
#   linux-arm64   -> cs-cloud-linux-arm64.tar.gz
#   windows-amd64 -> cs-cloud-windows-amd64.zip
# ============================================================

set -euo pipefail

# ============================================================
# 默认配置
# ============================================================

DOWNLOAD_BASE_URL="${DOWNLOAD_BASE_URL:-https://aidc.sit.cmhk.com/costrict-static/cs-cloud}"
API_BASE_URL="${API_BASE_URL:-https://aidc.sit.cmhk.com/cloud-api}"
CHANGELOG="${CHANGELOG:-}"
FORCE="${FORCE:-false}"
MIN_CLIENT_VER="${MIN_CLIENT_VER:-}"
CHANNEL="${CHANNEL:-stable}"
DRY_RUN="${DRY_RUN:-false}"
LOCAL_DIR="${LOCAL_DIR:-}"

# 固定平台包定义
declare -A PACKAGES
PACKAGES["darwin-amd64"]="cs-cloud-darwin-amd64.tar.gz"
PACKAGES["darwin-arm64"]="cs-cloud-darwin-arm64.tar.gz"
PACKAGES["linux-amd64"]="cs-cloud-linux-amd64.tar.gz"
PACKAGES["linux-arm64"]="cs-cloud-linux-arm64.tar.gz"
PACKAGES["windows-amd64"]="cs-cloud-windows-amd64.zip"

# ============================================================
# 函数定义
# ============================================================

log_info()  { echo "[INFO]  $*" >&2; }
log_warn()  { echo "[WARN]  $*" >&2; }
log_error() { echo "[ERROR] $*" >&2; }
log_dry()   { echo "[DRY]   $*" >&2; }

usage() {
    cat <<EOF
用法: $0 <version> <system_token> [选项]

参数:
  version      版本号，如 v1.2.28（必填）
  system_token 系统令牌（必填）

选项:
  --changelog TEXT       更新日志
  --force BOOL           强制更新（默认: false）
  --min-client-ver VER   最低客户端版本
  --channel CHANNEL      发布渠道（默认: stable）
  --local-dir DIR        本地包目录（指定后从该目录读取包，跳过远程下载）
  --dry-run              仅打印，不实际调用 API
  --help                 显示此帮助信息

环境变量:
  DOWNLOAD_BASE_URL  下载地址前缀（默认: https://aidc.sit.cmhk.com/costrict-static/cs-cloud）
  API_BASE_URL       服务端 API 地址（默认: https://aidc.sit.cmhk.com/cloud-api）
  CHANGELOG          更新日志
  FORCE              强制更新
  MIN_CLIENT_VER     最低客户端版本
  CHANNEL            发布渠道
  LOCAL_DIR          本地包目录（与 --local-dir 等价）

示例:
  # 远程模式（从 DOWNLOAD_BASE_URL 下载获取元数据）
  $0 v1.2.28 "sk-xxx" --changelog "fix: bug fix"

  # 本地目录模式（从本地目录读取包，downloadUrl 仍使用 DOWNLOAD_BASE_URL 拼接）
  $0 v1.2.28 "sk-xxx" --local-dir ./dist --changelog "fix: bug fix"

  DOWNLOAD_BASE_URL="https://static.example.com/cs-cloud" \
  API_BASE_URL="https://api.example.com/cloud-api" \
  $0 v1.2.28 "sk-xxx"

下载地址示例:
  https://aidc.sit.cmhk.com/costrict-static/cs-cloud/v1.2.28/cs-cloud-darwin-amd64.tar.gz
  https://aidc.sit.cmhk.com/costrict-static/cs-cloud/v1.2.28/cs-cloud-windows-amd64.zip
EOF
    exit 0
}

# 从下载地址获取文件并计算 SHA256 和大小
fetch_asset_meta() {
    local url="$1"
    local tmpdir
    tmpdir=$(mktemp -d)
    local tmpfile="${tmpdir}/download"

    log_info "  抓取: ${url}"
    if ! curl -sSL --connect-timeout 10 --max-time 120 "$url" -o "$tmpfile"; then
        rm -rf "$tmpdir"
        log_error "  下载失败: ${url}"
        return 1
    fi

    local sha256_val
    sha256_val=$(sha256sum "$tmpfile" | cut -d' ' -f1)
    local size_val
    size_val=$(stat -c%s "$tmpfile")

    rm -rf "$tmpdir"

    echo "${sha256_val} ${size_val}"
}

# 从本地目录读取包文件计算 SHA256 和大小
fetch_asset_meta_local() {
    local filepath="$1"
    local filename
    filename=$(basename "$filepath")

    if [[ ! -f "$filepath" ]]; then
        log_error "  本地文件不存在: ${filepath}"
        return 1
    fi

    log_info "  读取: ${filepath}"

    local sha256_val
    sha256_val=$(sha256sum "$filepath" | cut -d' ' -f1)
    local size_val
    size_val=$(stat -c%s "$filepath")

    echo "${sha256_val} ${size_val}"
}

# 生成所有 assets 的 JSON 数组
build_assets_json() {
    local assets_json="["
    local first=true
    local asset_count=0

    # 将平台列表按固定顺序排列
    local platforms=("darwin-amd64" "darwin-arm64" "linux-amd64" "linux-arm64" "windows-amd64")

    for platform in "${platforms[@]}"; do
        local filename="${PACKAGES[$platform]}"
        local download_url="${DOWNLOAD_BASE_URL}/${VERSION}/${filename}"

        local sha256_val=""
        local size_val=""
        local meta

        if [[ -n "$LOCAL_DIR" ]]; then
            # 本地目录模式：从本地文件计算元数据
            local local_path="${LOCAL_DIR}/${filename}"
            if meta=$(fetch_asset_meta_local "$local_path"); then
                sha256_val=$(echo "$meta" | cut -d' ' -f1)
                size_val=$(echo "$meta" | cut -d' ' -f2)
            else
                log_warn "  跳过 ${platform}（本地文件不可用: ${local_path}）"
                continue
            fi
        else
            # 远程模式：从 DOWNLOAD_BASE_URL 下载获取元数据
            if meta=$(fetch_asset_meta "$download_url"); then
                sha256_val=$(echo "$meta" | cut -d' ' -f1)
                size_val=$(echo "$meta" | cut -d' ' -f2)
            else
                log_warn "  跳过 ${platform}（文件不可用）"
                continue
            fi
        fi

        log_info "  平台: ${platform}"
        log_info "    sha256: ${sha256_val}"
        log_info "    size:   ${size_val} bytes"
        log_info "    url:    ${download_url}"

        if [[ "$first" == "true" ]]; then
            first=false
        else
            assets_json+=","
        fi

        assets_json+=$(printf '\n    {"platform":"%s","downloadUrl":"%s","sha256":"%s","binarySize":%s}' \
            "$platform" "$download_url" "$sha256_val" "$size_val")
        asset_count=$((asset_count + 1))
    done

    assets_json+=$'\n  '"]"

    if [[ "$asset_count" -eq 0 ]]; then
        log_error "所有平台文件均不可用，终止。"
        exit 1
    fi

    echo "$assets_json"
    log_info "共 ${asset_count} 个平台"
}

# 调用 CreateRelease API
call_create_release_api() {
    local assets_json="$1"

    # API version 字段不带 v 前缀，做切割
    local version_for_api="${VERSION#v}"

    # 构建请求体
    local request_body
    request_body=$(cat <<EOF
{
  "version": "${version_for_api}",
  "assets": ${assets_json},
  "changelog": "${CHANGELOG}",
  "force": ${FORCE},
  "channel": "${CHANNEL}"
EOF
)

    if [[ -n "$MIN_CLIENT_VER" ]]; then
        request_body+=","$'\n'"  \"minClientVersion\": \"${MIN_CLIENT_VER}\""
    fi
    request_body+=$'\n}'

    log_info "POST ${API_BASE_URL}/api/releases"
    if [[ "$DRY_RUN" == "true" ]]; then
        log_dry "请求体:"
        echo "${request_body}" | sed 's/^/  /'
        log_dry "跳过 API 调用（dry-run 模式）"
        return
    fi

    local http_code
    local response
    response=$(curl -s -w "\n%{http_code}" -X POST "${API_BASE_URL}/api/releases" \
        -H "Content-Type: application/json" \
        -H "X-System-Token: ${SYSTEM_TOKEN}" \
        -d "${request_body}")

    http_code=$(echo "$response" | tail -1)
    local body
    body=$(echo "$response" | sed '$d')

    if [[ "$http_code" == "201" ]] || [[ "$http_code" == "200" ]]; then
        log_info "成功 (HTTP ${http_code})"
        log_info "响应: ${body}"
    else
        log_error "失败 (HTTP ${http_code})"
        log_error "响应: ${body}"
        exit 1
    fi
}

# ============================================================
# 主流程
# ============================================================

main() {
    # 解析位置参数
    if [[ $# -lt 2 ]]; then
        log_error "缺少必填参数: version system_token"
        usage
    fi
    VERSION="$1"
    SYSTEM_TOKEN="$2"
    shift 2

    # 解析可选参数
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --changelog)      CHANGELOG="$2";      shift 2 ;;
            --force)          FORCE="$2";          shift 2 ;;
            --min-client-ver) MIN_CLIENT_VER="$2"; shift 2 ;;
            --channel)        CHANNEL="$2";        shift 2 ;;
            --local-dir)      LOCAL_DIR="$2";      shift 2 ;;
            --dry-run)        DRY_RUN="true";      shift   ;;
            --help)           usage                ;;
            *) log_error "未知选项: $1"; usage ;;
        esac
    done

    echo "============================================================"
    echo "  cs-cloud Release Publish"
    echo "============================================================"
    echo "  版本:          ${VERSION}"
    echo "  下载地址前缀:  ${DOWNLOAD_BASE_URL}"
    echo "  API 地址:      ${API_BASE_URL}"
    echo "  渠道:          ${CHANNEL}"
    echo "  强制更新:      ${FORCE}"
    echo "  最小客户端版本: ${MIN_CLIENT_VER:-<未设置>}"
    if [[ -n "$LOCAL_DIR" ]]; then
        echo "  本地目录:      ${LOCAL_DIR}"
    fi
    echo "  Dry-Run:       ${DRY_RUN}"
    echo "============================================================"

    log_info "阶段 1/2: 抓取文件并组装资产信息..."
    local assets_json
    assets_json=$(build_assets_json)

    echo ""
    log_info "阶段 2/2: 提交版本记录..."
    call_create_release_api "$assets_json"

    echo ""
    log_info "完成！版本 ${VERSION} 已发布。"
    log_info "客户端检查更新: GET ${API_BASE_URL}/updates/check?platform={platform}&version={current_version}"
}

main "$@"
