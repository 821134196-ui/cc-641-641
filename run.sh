#!/usr/bin/env bash
# 一条命令启动：电商直播抽奖资格与奖品发货系统
# 用法：./run.sh [--addr :8080] [--db data/lottery.db] [--reset]
set -euo pipefail
cd "$(dirname "$0")"

# 若 PATH 中没有 go，但用户目录下安装过 sdk/go，则自动补上
if ! command -v go >/dev/null 2>&1 && [ -x "$HOME/sdk/go/bin/go" ]; then
  export PATH="$HOME/sdk/go/bin:$PATH"
fi

if [ "${1:-}" = "--reset" ]; then
  rm -rf data
  shift
fi

mkdir -p data bin
if [ -d vendor ]; then
  echo ">> 使用已固化的依赖离线编译"
  go build -mod=vendor -o bin/lottery ./cmd/server
else
  echo ">> 拉取依赖并编译（首次需要联网下载 Go 模块）"
  go mod tidy
  go build -o bin/lottery ./cmd/server
fi

echo ">> 启动服务：http://localhost:8080"
exec ./bin/lottery "$@"
