#!/usr/bin/env bash
# build.sh — api-server 本地编译脚本
#
# 用法：
#   ./build.sh                    编译本机二进制 → dist/api-server
#   ./build.sh --linux-amd64      通过 Docker 编译 linux/amd64 二进制 → dist/api-server-linux-amd64
#   ./build.sh --linux-arm64      通过 Docker 编译 linux/arm64 二进制 → dist/api-server-linux-arm64
#   ./build.sh --all              本机 + linux/amd64 + linux/arm64
#   ./build.sh --clean            清理 dist 目录后重新编译
#
# 说明：
#   - 项目依赖 go-sqlite3（需要 CGO），本机编译需 CGO 环境
#     （macOS 需安装 Xcode Command Line Tools；Linux 需 gcc）
#   - linux 交叉编译通过 Docker golang:alpine 完成（内置 gcc/musl-dev）

set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"
DIST_DIR="${ROOT_DIR}/dist"
BINARY_NAME="api-server"

if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
    BLUE=$'\033[34m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; RED=$'\033[31m'; RESET=$'\033[0m'
else
    BLUE=""; GREEN=""; YELLOW=""; RED=""; RESET=""
fi

step() { printf "%b▶ %s%b\n" "${BLUE}" "$*" "${RESET}"; }
ok()   { printf "%b✓ %s%b\n" "${GREEN}" "$*" "${RESET}"; }
fail() { printf "%b✗ 错误：%s%b\n" "${RED}" "$*" "${RESET}" >&2; exit 1; }
on_error() { printf "%b✗ 脚本执行失败，位置：第 %s 行%b\n" "${RED}" "${1}" "${RESET}" >&2; exit 1; }
trap 'on_error "$LINENO"' ERR

usage() { sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 0; }

DO_LOCAL=false
DO_LINUX_AMD64=false
DO_LINUX_ARM64=false
CLEAN=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --linux-amd64) DO_LINUX_AMD64=true; shift ;;
        --linux-arm64) DO_LINUX_ARM64=true; shift ;;
        --all)         DO_LOCAL=true; DO_LINUX_AMD64=true; DO_LINUX_ARM64=true; shift ;;
        --clean)       CLEAN=true; shift ;;
        -h|--help)     usage ;;
        *)             fail "未知参数: $1（用 --help 查看用法）" ;;
    esac
done

if [[ "${DO_LOCAL}" == false && "${DO_LINUX_AMD64}" == false && "${DO_LINUX_ARM64}" == false ]]; then
    DO_LOCAL=true
fi

mkdir -p "${DIST_DIR}"
[[ "${CLEAN}" == true ]] && { step "清理 dist/ ..."; rm -rf "${DIST_DIR}"; mkdir -p "${DIST_DIR}"; }

# ---- 本机编译 ----
if [[ "${DO_LOCAL}" == true ]]; then
    command -v go >/dev/null 2>&1 || fail "未找到 go，请先安装 Go (https://go.dev/dl/)"
    step "编译本机二进制 → dist/${BINARY_NAME}"
    CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o "${DIST_DIR}/${BINARY_NAME}" .
    ok "本机二进制完成"
fi

# ---- linux 交叉编译（Docker）----
linux_build() {
    local arch="$1"
    command -v docker >/dev/null 2>&1 || fail "未找到 docker，无法交叉编译 linux/${arch}"
    step "交叉编译 linux/${arch} → dist/${BINARY_NAME}-linux-${arch}（Docker）"
    docker run --rm --platform "linux/${arch}" \
        -v "${ROOT_DIR}:/src" -w /src \
        -e CGO_ENABLED=1 \
        golang:1.26-alpine \
        sh -c 'apk add --no-cache gcc musl-dev >/dev/null 2>&1 && go build -trimpath -ldflags "-s -w" -o /src/dist/api-server-linux-'"${arch}"' .'
    ok "linux/${arch} 完成"
}

[[ "${DO_LINUX_AMD64}" == true ]] && linux_build "amd64"
[[ "${DO_LINUX_ARM64}" == true ]] && linux_build "arm64"

echo
ok "编译产物: ${DIST_DIR}/"
ls -lh "${DIST_DIR}"
