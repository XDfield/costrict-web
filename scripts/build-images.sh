#!/bin/bash

# costrict-web 一键打包脚本
# 用于构建 gateway, server-api, server-worker, web 四个镜像

set -e

# 配置
REGISTRY="zgsm"
VERSION="${1:-latest}"
PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PLATFORMS=""
BUILDER_NAME="costrict-multiarch"

# 镜像名称
GATEWAY_IMAGE="${REGISTRY}/costrict-web-gateway"
API_IMAGE="${REGISTRY}/costrict-web-api"
WORKER_IMAGE="${REGISTRY}/costrict-web-worker"
PORTAL_IMAGE="${REGISTRY}/costrict-web-portal"

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# 打印帮助信息
print_help() {
    echo "Usage: $0 [VERSION] [OPTIONS]"
    echo ""
    echo "Arguments:"
    echo "  VERSION       镜像版本标签 (默认: latest)"
    echo ""
    echo "Options:"
    echo "  -h, --help    显示帮助信息"
    echo "  --gateway     仅构建 gateway 镜像"
    echo "  --api         仅构建 server-api 镜像"
    echo "  --worker      仅构建 server-worker 镜像"
    echo "  --portal      仅构建 web portal 镜像"
    echo "  --multi-arch  构建多架构镜像 (linux/amd64,linux/arm64)"
    echo "  --push        推送镜像到仓库"
    echo ""
    echo "Examples:"
    echo "  $0                              # 构建所有镜像，版本为 latest"
    echo "  $0 v1.0.0                       # 构建所有镜像，版本为 v1.0.0"
    echo "  $0 v1.0.0 --gateway             # 仅构建 gateway 镜像"
    echo "  $0 v1.0.0 --multi-arch --push   # 构建多架构镜像并推送"
    echo ""
    echo "镜像列表:"
    echo "  ${GATEWAY_IMAGE}:<version>"
    echo "  ${API_IMAGE}:<version>"
    echo "  ${WORKER_IMAGE}:<version>"
    echo "  ${PORTAL_IMAGE}:<version>"
}

# 确保 buildx builder 存在（多架构构建需要）
ensure_buildx_builder() {
    if [ -n "$PLATFORMS" ]; then
        if ! docker buildx inspect "$BUILDER_NAME" &>/dev/null; then
            log_info "Creating buildx builder: $BUILDER_NAME"
            docker buildx create --name "$BUILDER_NAME" --use --bootstrap
        else
            docker buildx use "$BUILDER_NAME"
        fi
    fi
}

# 通用构建函数
do_build() {
    local image_name="$1"
    local context="$2"
    local dockerfile="$3"
    local extra_args="${4:-}"

    if [ -n "$PLATFORMS" ]; then
        local push_flag=""
        if [ "$PUSH" = "true" ]; then
            push_flag="--push"
        else
            push_flag="--load"
            # --load 不支持多平台，仅在单平台时使用
            if [[ "$PLATFORMS" == *","* ]]; then
                push_flag="--push"
                log_warn "多架构构建必须配合 --push 使用，已自动启用推送"
            fi
        fi
        docker buildx build \
            --platform "$PLATFORMS" \
            -f "$dockerfile" \
            -t "${image_name}:${VERSION}" \
            -t "${image_name}:latest" \
            $extra_args \
            $push_flag \
            "$context"
    else
        docker build \
            -f "$dockerfile" \
            -t "${image_name}:${VERSION}" \
            -t "${image_name}:latest" \
            $extra_args \
            "$context"
    fi
}

# 构建 gateway 镜像
build_gateway() {
    log_info "Building gateway image..."
    do_build "${GATEWAY_IMAGE}" "${PROJECT_ROOT}/gateway" "${PROJECT_ROOT}/gateway/Dockerfile"
    log_info "Gateway image built: ${GATEWAY_IMAGE}:${VERSION}"
}

# 构建 server-api 镜像
build_api() {
    log_info "Building server-api image..."
    do_build "${API_IMAGE}" "${PROJECT_ROOT}/server" "${PROJECT_ROOT}/server/Dockerfile"
    log_info "Server-api image built: ${API_IMAGE}:${VERSION}"
}

# 构建 server-worker 镜像
build_worker() {
    log_info "Building server-worker image..."
    do_build "${WORKER_IMAGE}" "${PROJECT_ROOT}/server" "${PROJECT_ROOT}/server/Dockerfile.worker"
    log_info "Server-worker image built: ${WORKER_IMAGE}:${VERSION}"
}

# 构建 web portal 镜像
build_portal() {
    log_info "Building web portal image..."
    do_build "${PORTAL_IMAGE}" "${PROJECT_ROOT}/web" "${PROJECT_ROOT}/web/Dockerfile"
    log_info "Web portal image built: ${PORTAL_IMAGE}:${VERSION}"
}

# 构建所有镜像
build_all() {
    log_info "Building all images with version: ${VERSION}"
    echo "============================================"
    build_gateway
    echo "============================================"
    build_api
    echo "============================================"
    build_worker
    echo "============================================"
    build_portal
    echo "============================================"
    log_info "All images built successfully!"
    echo ""
    echo "Built images:"
    echo "  ${GATEWAY_IMAGE}:${VERSION}"
    echo "  ${API_IMAGE}:${VERSION}"
    echo "  ${WORKER_IMAGE}:${VERSION}"
    echo "  ${PORTAL_IMAGE}:${VERSION}"
}

# 推送镜像到仓库
push_images() {
    log_info "Pushing images to registry..."
    docker push "${GATEWAY_IMAGE}:${VERSION}"
    docker push "${API_IMAGE}:${VERSION}"
    docker push "${WORKER_IMAGE}:${VERSION}"
    docker push "${PORTAL_IMAGE}:${VERSION}"
    log_info "All images pushed successfully!"
}

# 主函数
main() {
    local build_target="all"
    PUSH="false"

    # 解析参数
    while [[ $# -gt 0 ]]; do
        case $1 in
            -h|--help)
                print_help
                exit 0
                ;;
            --gateway)
                build_target="gateway"
                shift
                ;;
            --api)
                build_target="api"
                shift
                ;;
            --worker)
                build_target="worker"
                shift
                ;;
            --portal)
                build_target="portal"
                shift
                ;;
            --multi-arch)
                PLATFORMS="linux/amd64,linux/arm64"
                shift
                ;;
            --push)
                PUSH="true"
                shift
                ;;
            *)
                VERSION="$1"
                shift
                ;;
        esac
    done

    # 检查 docker 是否可用
    if ! command -v docker &> /dev/null; then
        log_error "Docker is not installed or not in PATH"
        exit 1
    fi

    # 多架构构建需要 buildx
    if [ -n "$PLATFORMS" ]; then
        if ! docker buildx version &> /dev/null; then
            log_error "Docker Buildx is required for multi-arch builds. Please install it first."
            exit 1
        fi
        ensure_buildx_builder
    fi

    # 执行构建
    case $build_target in
        gateway)
            build_gateway
            ;;
        api)
            build_api
            ;;
        worker)
            build_worker
            ;;
        portal)
            build_portal
            ;;
        all)
            build_all
            ;;
    esac

    # 推送镜像（非多架构模式下的独立推送）
    if [ "$PUSH" = "true" ] && [ -z "$PLATFORMS" ]; then
        push_images
    fi
}

main "$@"
