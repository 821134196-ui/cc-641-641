#!/usr/bin/env bash
# 一条命令启动：构建前端 → 编译后端 → 启动服务（SQLite 本地库 + 模拟直播/物流事件）
set -euo pipefail

cd "$(dirname "$0")"
ROOT="$(pwd)"

# 1) 定位 Go（支持已安装或位于 ~/.local/go）
if ! command -v go >/dev/null 2>&1; then
  if [ -x "$HOME/.local/go/bin/go" ]; then
    export PATH="$HOME/.local/go/bin:$PATH"
  else
    echo "未找到 go，请先安装 Go 1.22+（或解压到 ~/.local/go）" >&2
    exit 1
  fi
fi

export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

# 2) 构建前端到 web/static（Go embed 目录）
if [ "${SKIP_FRONTEND:-0}" != "1" ]; then
  echo "==> 构建 Vue 前端"
  ( cd web/frontend
    if [ ! -d node_modules ]; then npm install; fi
    npm run build )
fi

# 3) 编译后端
echo "==> 编译 Go 后端"
mkdir -p bin data
go build -o bin/server ./cmd/server

# 4) 启动
ADDR="${ADDR:-:8080}"
DB_PATH="${DB_PATH:-data/lottery.db}"
echo "==> 启动服务 http://localhost${ADDR} （数据库 $DB_PATH）"
exec env ADDR="$ADDR" DB_PATH="$DB_PATH" ./bin/server
