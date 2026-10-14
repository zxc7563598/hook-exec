// Package api 提供管理页与它的后端接口。
//
// 所有接口都注册在 /web 这一组里并受 Basic Auth 保护。这一点必须守住：
// 旧项目把 /api/* 挂在了根路由上，Basic Auth 只包了 /web/*，
// 结果就是配了密码也挡不住别人直接拉走全部部署命令和脚本输出。
package api

import (
	"time"

	"github.com/zxc7563598/hook-exec/internal/executor"
	"github.com/zxc7563598/hook-exec/internal/shelldir"
	"github.com/zxc7563598/hook-exec/internal/store"
)

// Server 汇集管理接口需要的依赖。
type Server struct {
	store    *store.Store
	pool     *executor.Pool
	shellDir *shelldir.Dir

	// 对外暴露的脚本目录路径，页面上要显示，方便运维知道该往哪传脚本。
	shellRoot string

	startedAt time.Time
	version   string
}

// NewServer 构造管理接口。
func NewServer(st *store.Store, pool *executor.Pool, sd *shelldir.Dir, version string) *Server {
	return &Server{
		store:     st,
		pool:      pool,
		shellDir:  sd,
		shellRoot: sd.Root(),
		startedAt: time.Now(),
		version:   version,
	}
}
