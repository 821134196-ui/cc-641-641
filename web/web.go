// Package web 通过 go:embed 打包前端静态资源，最终产出单一可执行文件。
package web

import "embed"

//go:embed index.html vendor
var Files embed.FS
