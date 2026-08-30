#!/usr/bin/env bash
# install.sh — api-server 一键安装/部署脚本（Docker）
#
# 一键完成：拉取 pylintech/api-server 镜像 + 初始化管理员配置 + 启动容器
# - 镜像从仓库拉取（不在此构建）
# - 数据存于 Docker 命名卷（.env / api.db），不依赖宿主目录
# - 管理员密码由程序自身生成（环境变量 ADMIN_USER/ADMIN_PASSWORD 注入），脚本不参与密码生成
# - compose 配置内置，不依赖外部 docker-compose.yml
#
# 用法：
#   ./install.sh                          一键部署（拉取镜像 + 启动）
#   ./install.sh --tag v0.1.0             部署指定版本的镜像
#   ./install.sh --status                 查看状态
#   ./install.sh --restart                重启
#   ./install.sh --stop                   停止（保留数据）
#   ./install.sh --logs                   查看日志
#   ./install.sh --uninstall              卸载（保留数据）
#   ./install.sh --uninstall --purge      卸载并删除数据卷
#
# 环境变量：
#   APISERVER_IMAGE=pylintech/api-server   镜像名
#   APISERVER_TAG=latest                   镜像版本标签
#   DATA_VOLUME=api-server-data            数据卷名
#   CONFIG_DIR=/etc/api-server             编排文件目录（默认系统配置目录）
#   ADMIN_USER / ADMIN_PASSWORD            管理员账号与密码（未设置时交互输入）

set -Eeuo pipefail

APP_NAME="api-server"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
APISERVER_IMAGE="${APISERVER_IMAGE:-pylintech/api-server}"
DATA_VOLUME="${DATA_VOLUME:-api-server-data}"

ACTION="install"
PURGE=false
TAG="latest"

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

usage() { sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'; exit 0; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --status)      ACTION="status"; shift ;;
        --restart)     ACTION="restart"; shift ;;
        --stop)        ACTION="stop"; shift ;;
        --logs)        ACTION="logs"; shift ;;
        --uninstall)   ACTION="uninstall"; shift ;;
        --purge)       PURGE=true; shift ;;
        --tag)         TAG="${2:?--tag 需要参数}"; shift 2 ;;
        -h|--help)     usage ;;
        *)             fail "未知参数: $1（用 --help 查看用法）" ;;
    esac
done

export APISERVER_IMAGE
export APISERVER_TAG="${TAG}"
export APISERVER_DATA_VOLUME="${DATA_VOLUME}"
FULL_IMAGE="${APISERVER_IMAGE}:${TAG}"

# ---- 内置 compose 配置（写入系统配置目录，供 1Panel 等面板识别编排）----
CONFIG_DIR="${CONFIG_DIR:-/etc/api-server}"
if ! mkdir -p "${CONFIG_DIR}" 2>/dev/null; then
    fail "无法创建配置目录 ${CONFIG_DIR}（需要 root 权限，可设置 CONFIG_DIR 指定其他目录）"
fi
COMPOSE_FILE="${CONFIG_DIR}/docker-compose.yml"
cat > "${COMPOSE_FILE}" <<'YAML'
name: api-server
services:
  api-server:
    image: ${APISERVER_IMAGE:-pylintech/api-server}:${APISERVER_TAG:-latest}
    container_name: api-server
    ports:
      - "8081:8081"   # 管理面板 + API 端点服务
    environment:
      ADMIN_USER: ${ADMIN_USER:-}
      ADMIN_PASSWORD: ${ADMIN_PASSWORD:-}
    volumes:
      - api-server-data:/app/data   # 命名卷持久化 .env 与 api.db
    networks:
      - api-server-net
    restart: unless-stopped
volumes:
  api-server-data:
    name: ${APISERVER_DATA_VOLUME:-api-server-data}
networks:
  api-server-net:
    name: api-server-net   # 固定网络名，不带 compose 项目名前缀
YAML
COMPOSE=(docker compose -f "${COMPOSE_FILE}")

case "${ACTION}" in
    status)
        docker ps --filter "name=api-server" --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"
        exit 0 ;;
    stop)
        "${COMPOSE[@]}" stop
        ok "已停止全部服务（数据保留在卷 ${DATA_VOLUME}）"
        exit 0 ;;
    restart)
        "${COMPOSE[@]}" restart
        ok "已重启全部服务"
        exit 0 ;;
    logs)
        docker logs -f api-server
        exit 0 ;;
    uninstall)
        "${COMPOSE[@]}" down
        if [[ "${PURGE}" == true ]]; then
            docker volume rm "${DATA_VOLUME}" >/dev/null 2>&1 || true
            ok "已卸载并清除数据卷 ${DATA_VOLUME}（.env / api.db）"
        else
            warn "已卸载服务，数据卷 ${DATA_VOLUME} 保留（如需清除加 --purge）"
        fi
        exit 0 ;;
esac

# ---- 依赖检测 ----
command -v docker >/dev/null 2>&1 || fail "未找到 docker，请先安装 Docker"
docker compose version >/dev/null 2>&1 || fail "docker compose 不可用，请升级 Docker"

# ---- 初始化管理员配置（由程序在容器内生成，脚本只提供账号密码）----
init_admin() {
    local image="$1"
    if docker run --rm --entrypoint sh -v "${DATA_VOLUME}:/app/data" "${image}" \
        -c 'test -f /app/data/.env' >/dev/null 2>&1; then
        ok "数据卷 ${DATA_VOLUME} 已有管理员配置，跳过初始化"
        return 0
    fi
    if [[ -n "${ADMIN_USER:-}" && -n "${ADMIN_PASSWORD:-}" ]]; then
        ok "使用环境变量提供的管理员配置（${ADMIN_USER}）"
    else
        step "首次部署：创建管理员账号（用于登录管理面板 http://localhost:8081）"
        local pass2
        read -r -p "管理员账号: " ADMIN_USER
        [[ -n "${ADMIN_USER}" ]] || fail "管理员账号不能为空"
        while true; do
            read -r -s -p "管理员密码（≥6位）: " ADMIN_PASSWORD; echo
            read -r -s -p "再次输入密码: " pass2; echo
            if [[ -z "${ADMIN_PASSWORD}" || ${#ADMIN_PASSWORD} -lt 6 ]]; then
                warn "密码长度不能少于 6 位，请重新输入"
            elif [[ "${ADMIN_PASSWORD}" != "${pass2}" ]]; then
                warn "两次输入不一致，请重新输入"
            else
                break
            fi
        done
    fi
    export ADMIN_USER ADMIN_PASSWORD
}

# ---- 一键安装 ----
step "开始一键部署 ${APP_NAME}（${FULL_IMAGE}，数据卷 ${DATA_VOLUME}）"

# 1. 拉取镜像
step "拉取镜像 ${FULL_IMAGE} ..."
docker pull "${FULL_IMAGE}"
ok "镜像就绪"

# 2. 初始化管理员配置（容器首次启动时由程序生成 .env）
init_admin "${FULL_IMAGE}"

# 3. 启动容器
step "启动 api-server 容器（compose）..."
"${COMPOSE[@]}" up -d

# 4. 等待管理员配置写入数据卷（容器内程序生成 .env）
step "等待管理员配置写入数据卷 ${DATA_VOLUME} ..."
for _ in $(seq 1 20); do
    if docker run --rm --entrypoint sh -v "${DATA_VOLUME}:/app/data" "${FULL_IMAGE}" \
        -c 'test -f /app/data/.env' >/dev/null 2>&1; then
        break
    fi
    sleep 1
done
if docker run --rm --entrypoint sh -v "${DATA_VOLUME}:/app/data" "${FULL_IMAGE}" \
    -c 'test -f /app/data/.env' >/dev/null 2>&1; then
    ok "管理员配置已写入数据卷"
else
    warn "未检测到数据卷中的 .env（可能是已存在旧配置或容器仍在初始化），请用 ./install.sh --logs 检查"
fi
# 配置已写入成功，移除凭据环境变量，避免敏感信息残留
unset ADMIN_USER ADMIN_PASSWORD

# 5. 提示
sleep 3
docker ps --filter "name=api-server" --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"
echo
ok "部署完成！"
warn "管理面板：http://localhost:8081/login"
warn "日志查看：./install.sh --logs"
