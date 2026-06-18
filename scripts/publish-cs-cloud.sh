#!/bin/bash
# ============================================================
# cs-cloud Release Publish Script
# ============================================================
# 功能：
#   1. 从本地目录 A 读取 cs-cloud 二进制包
#   2. 复制到目标目录 B（nginx 托管目录）
#   3. 计算 SHA256 及文件大小
#   4. 拼接待下载地址（遵循服务端更新检查接口规范）
#   5. 调用 API 创建版本记录
#
# 服务端 URL 拼接规则 (update_service.go:112)：
#   downloadURL = ReleaseDownloadBaseURL + "/" + platform + "/cs-cloud-" + platform
#   例：https://download.example.com/cs-cloud/v1.2.28/darwin-amd64/cs-cloud-darwin-amd64.tar.gz
#
# API 路由：
#   GET  /api/updates/check?platform=xxx&version=xxx  (公开)
#   POST /api/releases                                  (SystemToken 鉴权)
#
# GitHub Release 命名规范（cs-cloud）：
#   cs-cloud-{platform}.tar.gz  (Unix)
#   cs-cloud-{platform}.zip     (Windows)
#   例：cs-cloud-darwin-amd64.tar.gz, cs-cloud-windows-amd64.zip
# ============================================================

set -euo pipefail

# ============================================================
# 配置区 — 可通过环境变量覆盖
# ============================================================

# 源目录 A：cs-cloud 包所在目录（下载的 GitHub Release 产物）
SRC_DIR="${SRC_DIR:-./_packages}"

# 目标目录 B：nginx 托管的静态文件根目录
# 实际目录: ${NGINX_DIR}/${VERSION}/{platform}/
NGINX_DIR="${NGINX_DIR:-/var/www/html/cs-cloud}"

# 下载域名/主机头
DOWNLOAD_HOST="${DOWNLOAD_HOST:-https://download.example.com}"

# 下载路径前缀（拼接为 DOWNLOAD_HOST + DOWNLOAD_PATH_PREFIX + "/" + platform + "/" + filename）
DOWNLOAD_PATH_PREFIX="${DOWNLOAD_PATH_PREFIX:-/cs-cloud}"

# 服务端 API 地址
API_BASE_URL="${API_BASE_URL:-https://api.example.com/api}"

# 系统令牌（SystemToken），用于 CreateRelease API 鉴权
SYSTEM_TOKEN="${SYSTEM_TOKEN:-}"

# 版本号，如 v1.2.28（为空则从包文件名第一个匹配项自动提取）
VERSION="${VERSION:-}"

# 更新日志
CHANGELOG="${CHANGELOG:-}"

# 是否强制更新
FORCE="${FORCE:-false}"

# 最低客户端版本
MIN_CLIENT_VER="${MIN_CLIENT_VER:-}"

# 发布渠道
CHANNEL="${CHANNEL:-stable}"

# 是否无实际操作（只打印，不做文件操作和 API 调用）
DRY_RUN="${DRY_RUN:-false}"

# ============================================================
# 函数定义
# ============================================================

log_info()  { echo "[INFO]  $*"; }
log_warn()  { echo "[WARN]  $*" >&2; }
log_error() { echo "[ERROR] $*" >&2; }
log_dry()   { echo "[DRY]   $*"; }

usage() {
    cat <<EOF
用法: $0 [选项]

选项:
  --src-dir DIR          源目录 A（默认: ./_packages）
  --nginx-dir DIR        目标目录 B, nginx 托管目录（默认: /var/www/html/cs-cloud）
  --download-host URL    下载域名（默认: https://download.example.com）
  --download-prefix PATH 下载路径前缀（默认: /cs-cloud）
  --api-base-url URL     服务端 API 地址（默认: https://api.example.com/api）
  --system-token TOKEN   系统令牌（必填）
  --version VERSION      版本号，如 v1.2.28（默认从文件名自动提取）
  --changelog TEXT       更新日志
  --force BOOL           强制更新（默认: false）
  --min-client-ver VER   最低客户端版本
  --channel CHANNEL      发布渠道（默认: stable）
  --dry-run              无实际操作模式
  --help                 显示此帮助信息

环境变量:
  所有选项均可通过同名全大写环境变量设置（例: SYSTEM_TOKEN, SRC_DIR）
  优先级: 命令行参数 > 环境变量 > 默认值

示例:
  # 最基本用法
  SYSTEM_TOKEN="xxx" VERSION="v1.2.28" \\
    SRC_DIR="./downloads" \\
    NGINX_DIR="/var/www/html/cs-cloud" \\
    DOWNLOAD_HOST="https://download.example.com" \\
    API_BASE_URL="https://api.costrict.example.com/api" \\
    $0

  # 带 changelog 的完整用法
  $0 \\
    --src-dir ./release-artifacts \\
    --nginx-dir /data/nginx/static/cs-cloud \\
    --download-host https://static.costrict.cn \\
    --api-base-url https://api.costrict.cn/api \\
    --system-token "sk-xxxx" \\
    --version "v1.2.28" \\
    --changelog "fix(updater): prevent redundant upgrade on restart" \\
    --force false \\
    --channel stable

目录结构（nginx 托管目录）:
  {NGINX_DIR}/
    v1.2.28/
      darwin-amd64/cs-cloud-darwin-amd64.tar.gz
      darwin-arm64/cs-cloud-darwin-arm64.tar.gz
      linux-amd64/cs-cloud-linux-amd64.tar.gz
      linux-arm64/cs-cloud-linux-arm64.tar.gz
      windows-amd64/cs-cloud-windows-amd64.zip
    v1.2.27/
      ...

EOF
    exit 0
}

# 解析命令行参数
parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --src-dir)         SRC_DIR="$2";         shift 2 ;;
            --nginx-dir)       NGINX_DIR="$2";       shift 2 ;;
            --download-host)   DOWNLOAD_HOST="$2";   shift 2 ;;
            --download-prefix) DOWNLOAD_PATH_PREFIX="$2"; shift 2 ;;
            --api-base-url)    API_BASE_URL="$2";     shift 2 ;;
            --system-token)    SYSTEM_TOKEN="$2";     shift 2 ;;
            --version)         VERSION="$2";          shift 2 ;;
            --changelog)       CHANGELOG="$2";        shift 2 ;;
            --force)           FORCE="$2";            shift 2 ;;
            --min-client-ver)  MIN_CLIENT_VER="$2";   shift 2 ;;
            --channel)         CHANNEL="$2";          shift 2 ;;
            --dry-run)         DRY_RUN="true";        shift   ;;
            --help)            usage                  ;;
            *) log_error "未知选项: $1"; usage ;;
        esac
    done
}

# 检测文件平台类型
# 输入: 文件名 (如 cs-cloud-darwin-amd64.tar.gz)
# 输出: 平台标识 (如 darwin-amd64)
detect_platform() {
    local filename
    filename=$(basename "$1")
    # 去掉前缀 cs-cloud- 和扩展名
    # cs-cloud-darwin-amd64.tar.gz -> darwin-amd64
    # cs-cloud-windows-amd64.zip   -> windows-amd64
    local without_prefix="${filename#cs-cloud-}"
    # 去掉 .tar.gz / .zip 等扩展名
    echo "${without_prefix%.tar.gz}" | sed 's/\.zip$//' | sed 's/\.tar\.gz$//'
}

# 校验系统令牌
check_system_token() {
    if [[ -z "$SYSTEM_TOKEN" ]]; then
        log_error "SYSTEM_TOKEN 未配置！请通过 --system-token 或 SYSTEM_TOKEN 环境变量设置。"
        exit 1
    fi
}

# 校验必填参数
check_required() {
    if [[ -z "$VERSION" ]]; then
        # 尝试从源目录中的文件自动提取版本号
        local first_file
        first_file=$(ls "$SRC_DIR"/cs-cloud-* 2>/dev/null | head -1 || true)
        if [[ -n "$first_file" ]]; then
            log_warn "VERSION 未指定，尝试从 ${SRC_DIR}/checksums.txt 或目录名提取..."
            # 检查是否有 checksums.txt
            if [[ -f "$SRC_DIR/checksums.txt" ]]; then
                # checksums.txt 所在目录可能包含版本信息
                log_error "无法从 checksums.txt 自动提取版本，请显式指定 --version"
                exit 1
            fi
        fi
        log_error "VERSION 未配置！请通过 --version 或 VERSION 环境变量设置。"
        exit 1
    fi
}

# 准备目标目录结构（含版本子目录）
prepare_target_dir() {
    local target_dir="${NGINX_DIR}/${VERSION}"
    if [[ "$DRY_RUN" == "true" ]]; then
        log_dry "创建目标目录: ${target_dir}"
        return
    fi
    mkdir -p "$target_dir"
    log_info "目标目录就绪: ${target_dir}"
}

# 复制文件到 nginx 目录并生成 asset 元数据
# 目标结构:
#   ${NGINX_DIR}/${VERSION}/
#     darwin-amd64/
#       cs-cloud-darwin-amd64.tar.gz
#     linux-amd64/
#       cs-cloud-linux-amd64.tar.gz
#     windows-amd64/
#       cs-cloud-windows-amd64.zip
process_packages() {
    local assets_json="["
    local first=true
    local asset_count=0

    # 遍历源目录中的 cs-cloud 包
    for pkg in "$SRC_DIR"/cs-cloud-*.tar.gz "$SRC_DIR"/cs-cloud-*.zip; do
        # 避免通配符无匹配时仍循环
        [[ -f "$pkg" ]] || continue

        local filename
        filename=$(basename "$pkg")
        local platform
        platform=$(detect_platform "$filename")

        if [[ -z "$platform" ]]; then
            log_warn "无法从文件名提取平台信息，跳过: ${filename}"
            continue
        fi

        log_info "处理包: ${filename} -> platform=${platform}"

        # 创建版本化平台子目录: {NGINX_DIR}/{VERSION}/{platform}
        local platform_dir="${NGINX_DIR}/${VERSION}/${platform}"
        if [[ "$DRY_RUN" != "true" ]]; then
            mkdir -p "$platform_dir"
        fi

        # 计算 SHA256 和文件大小
        local sha256_val
        local size_val
        if [[ "$DRY_RUN" == "true" ]]; then
            sha256_val="<computed-sha256>"
            size_val="<file-size>"
        else
            sha256_val=$(sha256sum "$pkg" | cut -d' ' -f1)
            size_val=$(stat -c%s "$pkg")
        fi

        # 构造下载 URL（含版本号）
        # 格式: DOWNLOAD_HOST + DOWNLOAD_PATH_PREFIX + "/" + VERSION + "/" + platform + "/" + filename
        local download_url="${DOWNLOAD_HOST}${DOWNLOAD_PATH_PREFIX}/${VERSION}/${platform}/${filename}"

        # 复制文件到目标目录
        local dest="${platform_dir}/${filename}"
        if [[ "$DRY_RUN" == "true" ]]; then
            log_dry "cp \"$pkg\" \"$dest\""
            log_dry "  sha256: ${sha256_val}"
            log_dry "  size:   ${size_val} bytes"
            log_dry "  url:    ${download_url}"
        else
            cp "$pkg" "$dest"
            log_info "  已复制到: ${dest}"
            log_info "  sha256: ${sha256_val}"
            log_info "  size:   ${size_val} bytes"
            log_info "  url:    ${download_url}"
        fi

        # 构建 asset JSON（用于 API 请求）
        if [[ "$first" == "true" ]]; then
            first=false
        else
            assets_json+=","
        fi
        assets_json+=$(cat <<EOF
{
    "platform": "${platform}",
    "downloadUrl": "${download_url}",
    "sha256": "${sha256_val}",
    "binarySize": ${size_val}
}
EOF
        )

        asset_count=$((asset_count + 1))
    done

    assets_json+="]"

    if [[ "$asset_count" -eq 0 ]]; then
        log_error "在 ${SRC_DIR} 中未找到任何 cs-cloud 包（cs-cloud-*.tar.gz 或 cs-cloud-*.zip）"
        exit 1
    fi

    log_info "共处理 ${asset_count} 个包"
    echo "$assets_json"
}

# 调用 API 创建版本记录
call_create_release_api() {
    local assets_json="$1"

    local api_url="${API_BASE_URL}/releases"

    # 构建请求体
    local request_body
    request_body=$(cat <<EOF
{
    "version": "${VERSION}",
    "assets": ${assets_json},
    "changelog": "${CHANGELOG}",
    "force": ${FORCE},
    "channel": "${CHANNEL}"
EOF
)

    # 可选字段
    if [[ -n "$MIN_CLIENT_VER" ]]; then
        request_body+=","$'\n'"    \"minClientVersion\": \"${MIN_CLIENT_VER}\""
    fi
    request_body+=$'\n}'

    log_info "调用 API: POST ${api_url}"
    log_info "请求体:"
    echo "${request_body}" | sed 's/^/  /'

    if [[ "$DRY_RUN" == "true" ]]; then
        log_dry "跳过 API 调用（dry-run 模式）"
        return
    fi

    # 发送请求
    local http_code
    local response
    response=$(curl -s -w "\n%{http_code}" -X POST "${api_url}" \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer ${SYSTEM_TOKEN}" \
        -d "${request_body}")

    http_code=$(echo "$response" | tail -1)
    local body
    body=$(echo "$response" | sed '$d')

    if [[ "$http_code" == "201" ]] || [[ "$http_code" == "200" ]]; then
        log_info "API 调用成功 (HTTP ${http_code})"
        log_info "响应: ${body}"
    else
        log_error "API 调用失败 (HTTP ${http_code})"
        log_error "响应: ${body}"
        exit 1
    fi
}

# 清理临时文件（预留）
cleanup() {
    :
}

# ============================================================
# 主流程
# ============================================================

main() {
    parse_args "$@"
    trap cleanup EXIT

    echo "============================================================"
    echo "  cs-cloud Release Publish"
    echo "============================================================"
    echo "  源目录:        ${SRC_DIR}"
    echo "  目标目录:      ${NGINX_DIR}/${VERSION}/"
    echo "  下载域名:      ${DOWNLOAD_HOST}"
    echo "  路径前缀:      ${DOWNLOAD_PATH_PREFIX}"
    echo "  API 地址:      ${API_BASE_URL}"
    echo "  版本号:        ${VERSION:-<自动提取>}"
    echo "  渠道:          ${CHANNEL}"
    echo "  强制更新:      ${FORCE}"
    echo "  最小客户端版本: ${MIN_CLIENT_VER:-<未设置>}"
    echo "  Dry-Run:       ${DRY_RUN}"
    echo "============================================================"

    # 检查系统令牌
    check_system_token

    # 检查并确认参数
    check_required

    # 确保源目录存在
    if [[ ! -d "$SRC_DIR" ]]; then
        log_error "源目录不存在: ${SRC_DIR}"
        exit 1
    fi

    # 准备目标目录
    prepare_target_dir

    # 处理所有包并获取 assets JSON
    echo ""
    log_info "阶段 1/2: 处理包文件..."
    local assets_json
    assets_json=$(process_packages)
    echo ""

    # 调用 API
    log_info "阶段 2/2: 提交版本记录..."
    call_create_release_api "$assets_json"
    echo ""

    log_info "完成！版本 ${VERSION} 已发布。"
    log_info "客户端可通过以下接口检查更新:"
    log_info "  GET ${API_BASE_URL}/updates/check?platform={platform}&version={current_version}"

    # 输出支持的平台列表
    echo ""
    echo "支持的平台:"
    for pkg in "$SRC_DIR"/cs-cloud-*.tar.gz "$SRC_DIR"/cs-cloud-*.zip; do
        [[ -f "$pkg" ]] || continue
        local platform
        platform=$(detect_platform "$(basename "$pkg")")
        echo "  - ${platform}"
    done
}

main "$@"
