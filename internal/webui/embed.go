// Package webui 把管理页的静态资源嵌进二进制。
//
// 全部资产都随二进制分发，页面不依赖任何外网 CDN。运维场景里服务器
// 常常在内网，挂个 CDN 只会让页面变白；而且锁定版本也避免了
// "CDN 上更新了大版本导致页面行为变了"这种没法复现的问题。
package webui

import (
	"embed"
	"io/fs"
)

//go:embed index.html vendor/*
var files embed.FS

// Index 返回管理页的 HTML。
func Index() ([]byte, error) {
	return files.ReadFile("index.html")
}

// Assets 返回 vendor 目录下的静态资源子树，供 HTTP 服务使用。
func Assets() (fs.FS, error) {
	return fs.Sub(files, "vendor")
}
