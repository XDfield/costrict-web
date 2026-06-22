#!/bin/bash
# ============================================================
# cs-cloud Package Download Script
# ============================================================
# 功能：
#   从 GitHub Releases 下载 cs-cloud 各平台包
#
# 下载地址拼接：
#   ${GITHUB_URL}/releases/download/${VERSION}/${filename}
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

GITHUB_URL="${GITHUB_URL:-https://github.com/XDfield/cs-cloud}"
OUTPUT_DIR="${OUTPUT_DIR:-./downloads}"
VERSION=""
SKIP_EXISTING="${SKIP_EXISTING:-true}"

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
用法: $0 <version> [选项]

参数:
  version      版本号，如 v1.2.28（必填）

选项:
  --github-url URL    GitHub 仓库地址（默认: https://github.com/XDfield/cs-cloud）
  --output-dir DIR    下载目录（默认: ./downloads）
  --force             强制重新下载，覆盖已有文件
  --dry-run           仅打印下载链接，不实际下载
  --help              显示此帮助信息

环境变量:
  GITHUB_URL     GitHub 仓库地址（与 --github-url 等价）
  OUTPUT_DIR     下载目录（与 --output-dir 等价）
  SKIP_EXISTING  跳过已存在的文件（默认: true，设 false 覆盖）

示例:
  # 基本用法
  $0 v1.2.28

  # 指定 GitHub 地址和输出目录
  $0 v1.2.28 --github-url https://github.com/myorg/cs-cloud --output-dir ./dist

  # 强制重新下载
  $0 v1.2.28 --force

  # 仅查看待下载的链接
  $0 v1.2.28 --dry-run

下载地址示例:
  https://github.com/XDfield/cs-cloud/releases/download/v1.2.28/cs-cloud-darwin-amd64.tar.gz
  https://github.com/XDfield/cs-cloud/releases/download/v1.2.28/cs-cloud-windows-amd64.zip
EOF
    exit 0
}

# 下载单个包文件
download_asset() {
    local platform="$1"
    local filename="$2"
    local version="$3"

    local url="${GITHUB_URL}/releases/download/${version}/${filename}"
    local outfile="${OUTPUT_DIR}/${filename}"

    if [[ "$SKIP_EXISTING" == "true" ]] && [[ -f "$outfile" ]]; then
        log_info "  跳过 ${platform}（已存在: ${outfile}）"
        return 0
    fi

    log_info "  下载: ${url}"
    log_info "     -> ${outfile}"

    if ! curl -sSfL --connect-timeout 10 --max-time 300 "$url" -o "$outfile"; then
        log_error "  下载失败: ${url}"
        return 1
    fi

    local size
    size=$(stat -c%s "$outfile")
    log_info "  完成: ${filename} (${size} bytes)"
}

# ============================================================
# 主流程
# ============================================================

main() {
    # 解析位置参数
    if [[ $# -lt 1 ]]; then
        log_error "缺少必填参数: version"
        usage
    fi
    VERSION="$1"
    shift

    # 解析可选参数
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --github-url)     GITHUB_URL="$2";     shift 2 ;;
            --output-dir)     OUTPUT_DIR="$2";     shift 2 ;;
            --force)          SKIP_EXISTING="false"; shift ;;
            --dry-run)        DRY_RUN="true";      shift   ;;
            --help)           usage                ;;
            *) log_error "未知选项: $1"; usage ;;
        esac
    done

    # 去除 GITHUB_URL 末尾的 /
    GITHUB_URL="${GITHUB_URL%/}"

    echo "============================================================"
    echo "  cs-cloud Package Download"
    echo "============================================================"
    echo "  版本:         ${VERSION}"
    echo "  GitHub:       ${GITHUB_URL}"
    echo "  输出目录:     ${OUTPUT_DIR}"
    echo "  跳过已存在:   ${SKIP_EXISTING}"
    echo "  Dry-Run:      ${DRY_RUN:-false}"
    echo "============================================================"

    # 创建输出目录
    if [[ "${DRY_RUN:-false}" != "true" ]]; then
        mkdir -p "$OUTPUT_DIR"
    fi

    # 按固定顺序遍历平台
    local platforms=("darwin-amd64" "darwin-arm64" "linux-amd64" "linux-arm64" "windows-amd64")
    local success_count=0
    local fail_count=0

    for platform in "${platforms[@]}"; do
        local filename="${PACKAGES[$platform]}"
        local url="${GITHUB_URL}/releases/download/${VERSION}/${filename}"

        if [[ "${DRY_RUN:-false}" == "true" ]]; then
            echo "  ${url}"
            success_count=$((success_count + 1))
            continue
        fi

        if download_asset "$platform" "$filename" "$VERSION"; then
            success_count=$((success_count + 1))
        else
            fail_count=$((fail_count + 1))
        fi
    done

    echo ""
    log_info "下载完成: 成功 ${success_count}，失败 ${fail_count}"

    if [[ "$fail_count" -gt 0 ]]; then
        exit 1
    fi

    # 本地发布快捷提示
    if [[ "${DRY_RUN:-false}" != "true" ]]; then
        echo ""
        echo "============================================================"
        echo "  如需将下载的包发布到更新服务，可执行:"
        echo ""
        echo "  scripts/publish-cs-cloud.sh ${VERSION} \"<system_token>\" \\"
        echo "    --local-dir ${OUTPUT_DIR} --changelog \"<changelog>\""
        echo "============================================================"
    fi
}

main "$@"
