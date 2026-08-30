#!/usr/bin/env bash
# docker-build.sh — api-server Docker 镜像构建与推送脚本
#
# 流程：先在“工作电脑”编译目标架构二进制（./build.sh），再构建多架构镜像并推送。
# 镜像内不编译，只 COPY dist/ 下预编译的二进制。
#
# 用法：
#   ./docker-build.sh --version v0.1.0             # 编译+构建多架构并推送 pylintech/api-server
#   ./docker-build.sh --version v0.1.0 --no-push   # 仅本地构建（本机架构，不推送）
#   ./docker-build.sh --version v0.1.0 --arch amd64,arm64
#   ./docker-build.sh --version v0.1.0 --image pylintech/api-server
#   ./docker-build.sh --version v0.1.0 --skip-build  # 跳过二进制编译，直接用 dist/ 已有产物
#
# 环境变量：
#   DOCKER_REGISTRY   镜像仓库前缀（默认为空，直接用 IMAGE_NAME）
#   APISERVER_IMAGE   镜像名（默认 pylintech/api-server）

set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGE_NAME="${APISERVER_IMAGE:-pylintech/api-server}"
REGISTRY="${DOCKER_REGISTRY:-}"

VERSION=""
DO_PUSH=true
SKIP_BUILD=false
TARGET_ARCHES=("amd64" "arm64")

if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
    BLUE=$'\033[34m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; RED=$'\033[31m'; RESET=$'\033[0m'
else
    BLUE=""; GREEN=""; YELLOW=""; RED=""; RESET=""
fi

step() { printf "%b▶ %s%b\n" "${BLUE}" "$*" "${RESET}"; }
ok()   { printf "%b✓ %s%b\n" "${GREEN}" "$*" "${RESET}"; }
warn() { printf "%b! %s%b\n" "${YELLOW}" "$*" "${RESET}"; }
fail() { printf "%b✗ 错误：%s%b\n" "${RED}" "$*" "${RESET}" >&2; exit 1; }
on_error() { printf "%b✗ 脚本执行失败，位置：第 %s 行%b\n" "${RED}" "${1}" "${RESET}" >&2; exit 1; }
trap 'on_error "$LINENO"' ERR

usage() { sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 0; }

host_arch() {
    case "$(uname -m)" in
        x86_64|amd64)  echo "amd64" ;;
        arm64|aarch64) echo "arm64" ;;
        *)             echo "amd64" ;;
    esac
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --version)      VERSION="${2:?--version 需要参数}"; shift 2 ;;
        --image)        IMAGE_NAME="${2:?--image 需要参数}"; shift 2 ;;
        --arch)         IFS=',' read -r -a TARGET_ARCHES <<< "${2:?--arch 需要参数}"; shift 2 ;;
        --no-push)      DO_PUSH=false; shift ;;
        --skip-build)   SKIP_BUILD=true; shift ;;
        -h|--help)      usage ;;
        *)              fail "未知参数: $1（用 --help 查看用法）" ;;
    esac
done

if [[ -z "${VERSION}" ]]; then
    read -r -p "请输入版本号（如 v0.1.0）: " VERSION
    [[ -n "${VERSION}" ]] || fail "版本号不能为空"
fi
[[ "${VERSION}" == v* ]] || VERSION="v${VERSION}"
FULL_IMAGE="${REGISTRY:+${REGISTRY}/}${IMAGE_NAME}"

command -v docker >/dev/null 2>&1 || fail "未找到 docker，请先安装 Docker"
docker buildx version >/dev/null 2>&1 || fail "docker buildx 不可用，请升级 Docker 或执行 docker buildx install"

# ---- 1. 编译目标架构二进制（工作电脑完成，镜像只 COPY）----
if [[ "${SKIP_BUILD}" == true ]]; then
    step "跳过二进制编译（--skip-build），使用 dist/ 已有产物"
else
    for arch in "${TARGET_ARCHES[@]}"; do
        step "编译 linux/${arch} 二进制..."
        "${ROOT_DIR}/build.sh" --linux-"${arch}"
    done
fi

# 检查 dist 产物存在
for arch in "${TARGET_ARCHES[@]}"; do
    [[ -f "${ROOT_DIR}/dist/api-server-linux-${arch}" ]] \
        || fail "缺少 dist/api-server-linux-${arch}，请先运行 ./build.sh --linux-${arch}"
done

# ---- 2. 构建并推送多架构镜像 ----
PLATFORMS=""
for arch in "${TARGET_ARCHES[@]}"; do
    PLATFORMS="${PLATFORMS:+${PLATFORMS},}linux/${arch}"
done

build_args=(
    buildx build --platform "${PLATFORMS}"
    -t "${FULL_IMAGE}:${VERSION}"
    -t "${FULL_IMAGE}:latest"
    --provenance=false --sbom=false
)

if [[ "${DO_PUSH}" == true ]]; then
    build_args+=(--push)
    step "构建并推送 ${FULL_IMAGE}:${VERSION}（${PLATFORMS}）"
    warn "若未登录请先执行: docker login"
else
    # buildx --load 仅支持单平台，本地构建时退化为本机架构
    PLATFORMS="linux/$(host_arch)"
    if [[ ${#TARGET_ARCHES[@]} -gt 1 || "${PLATFORMS}" != "linux/${TARGET_ARCHES[0]}" ]]; then
        warn "--no-push 模式下仅构建本机架构 ${PLATFORMS}（buildx --load 不支持多平台本地加载）"
    fi
    build_args=(
        buildx build --platform "${PLATFORMS}"
        -t "${FULL_IMAGE}:${VERSION}"
        -t "${FULL_IMAGE}:latest"
        --provenance=false --sbom=false
        --load
    )
    step "构建镜像 ${FULL_IMAGE}:${VERSION}（${PLATFORMS}，仅本地，--no-push）"
fi

docker "${build_args[@]}" .

echo
ok "镜像 ${FULL_IMAGE}:${VERSION} 就绪"
[[ "${DO_PUSH}" == true ]] && ok "已推送: ${FULL_IMAGE}:${VERSION} / :latest"
