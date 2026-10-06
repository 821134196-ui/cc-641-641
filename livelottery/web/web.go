package web

import "embed"

// Static 前端构建产物。run.sh 会先执行 npm build 生成真实页面到 static/；
// 仓库内保留占位 index.html，保证未构建前端时后端也能编译运行。
// （目录刻意不叫 dist，以免被 .gitignore 全局忽略而导致 go:embed 失败。）
//
//go:embed all:static
var Static embed.FS
