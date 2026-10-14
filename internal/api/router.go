package api

import (
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zxc7563598/hook-exec/internal/webhook"
)

// RouterDeps 是注册路由需要的依赖。
type RouterDeps struct {
	Server    *Server
	Webhook   *webhook.Handler
	WebUser   string
	WebPass   string
	IndexHTML []byte
	Assets    fs.FS
}

// Register 挂载全部路由。
//
// 整个服务只有两个路由：
//
//	POST /       接收 GitHub webhook（靠 HMAC 验签，不需要 Basic Auth）
//	GET  /       返回 200，可当存活探针用，不泄露任何信息
//	ANY  /web/*  管理页与管理接口，全部受 Basic Auth 保护
//
// 管理接口挂在 /web 组内部，这一点必须守住。旧项目把 /api/* 注册在根路由上，
// Basic Auth 只包住 /web/* 的静态资源，结果是配了密码也拦不住别人
// 直接拉走全部部署命令与脚本输出。
func Register(r *gin.Engine, d RouterDeps) {
	h := d.Webhook

	// ── 对外路由 ──
	r.POST("/", h.Receive)
	r.GET("/", func(c *gin.Context) {
		// 一个不泄露任何信息的存活探针。刻意不额外开 /healthz：
		// 保持"只有两个路由"这个约束。
		c.String(http.StatusOK, "hook-exec")
	})

	// ── 管理页 ──
	web := r.Group("/web", basicAuth(d.WebUser, d.WebPass))

	serveIndex := func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", d.IndexHTML)
	}
	// "/web" 与 "/web/" 都指向同一个单页
	web.GET("", serveIndex)
	web.GET("/", serveIndex)

	if d.Assets != nil {
		fileServer := http.FileServer(http.FS(d.Assets))
		web.GET("/assets/*filepath", gin.WrapH(http.StripPrefix("/web/assets/", fileServer)))
	}

	// ── 管理接口 ──
	// 整组继承 Basic Auth，另外对写操作要求 X-Requested-With 头。
	api := web.Group("/api", csrfGuard())

	api.GET("/projects", d.Server.ListProjects)
	api.POST("/projects", d.Server.CreateProject)
	api.PUT("/projects/:id", d.Server.UpdateProject)
	api.PATCH("/projects/:id/enabled", d.Server.SetProjectEnabled)
	api.DELETE("/projects/:id", d.Server.DeleteProject)
	api.POST("/projects/:id/run", d.Server.RunProject)

	api.GET("/scripts", d.Server.ListScripts)
	api.GET("/scripts/content", d.Server.GetScriptContent)

	api.GET("/executions", d.Server.ListExecutions)
	api.DELETE("/executions", d.Server.ClearExecutions)
	api.GET("/executions/:id", d.Server.GetExecution)
	api.DELETE("/executions/:id", d.Server.DeleteExecution)

	api.GET("/config/export", d.Server.ExportConfig)
	api.POST("/config/import", d.Server.ImportConfig)

	slog.Debug("路由已注册", "routes", len(r.Routes()))
}
