#!/usr/bin/env bash
# api-server 启动脚本
cd "$(dirname "$0")"
export PORT="${PORT:-8081}"
exec ./api-server