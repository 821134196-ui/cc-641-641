#!/usr/bin/env bash
# 一条命令验证：编译 + 全部单元/并发测试 + 启动真实服务做端到端断言。
set -euo pipefail
cd "$(dirname "$0")"

if ! command -v go >/dev/null 2>&1; then
  [ -x "$HOME/.local/go/bin/go" ] && export PATH="$HOME/.local/go/bin:$PATH"
fi
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

echo "========== 1) Go 单元/并发测试（含 -race）=========="
go test -race -count=1 ./...

echo
echo "========== 2) 构建后端 =========="
mkdir -p bin
go build -o bin/server ./cmd/server

echo
echo "========== 3) 启动真实服务并做端到端验证 =========="
python3 scripts/verify_e2e.py

echo
echo "全部验证完成。"
