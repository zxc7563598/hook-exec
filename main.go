// hook-exec 接收 GitHub webhook，并执行 shell 目录下预先放好的脚本。
//
// 只暴露两个路由：/ 接收 webhook，/web 是管理页。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zxc7563598/hook-exec/internal/api"
	"github.com/zxc7563598/hook-exec/internal/config"
	"github.com/zxc7563598/hook-exec/internal/executor"
	"github.com/zxc7563598/hook-exec/internal/shelldir"
	"github.com/zxc7563598/hook-exec/internal/store"
	"github.com/zxc7563598/hook-exec/internal/webhook"
	"github.com/zxc7563598/hook-exec/internal/webui"
)

// version 可在构建时用 -ldflags "-X main.version=..." 覆盖。
var version = "dev"

const (
	httpShutdownGrace = 10 * time.Second
	executionGrace    = 30 * time.Second
)

func main() {
	if err := run(); err != nil {
		// flag 包在 -h 时返回 ErrHelp，那属于正常退出
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "启动失败："+err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Parse(os.Args[1:], os.Stderr)
	if err != nil {
		return err
	}
	setupLogger(cfg.Debug)

	dirs, err := config.ResolveDataDir(cfg.DataDir)
	if err != nil {
		return err
	}

	st, err := store.Open(dirs.DBPath, cfg.Debug)
	if err != nil {
		return err
	}
	defer st.Close()

	// 崩溃或 kill -9 会留下永远停在 pending/running 的记录，启动时收尾。
	if n, err := st.ReconcileOnStartup(); err != nil {
		slog.Error("启动对账失败", "err", err)
	} else if n > 0 {
		slog.Warn("发现上次未正常结束的执行记录，已标记为中断", "count", n)
	}

	shellDir, err := shelldir.New(dirs.ShellDir)
	if err != nil {
		return fmt.Errorf("初始化脚本目录失败：%w", err)
	}

	pool := executor.New(st, shellDir, cfg.Concurrency)

	cleanupCtx, stopCleanup := context.WithCancel(context.Background())
	defer stopCleanup()
	st.StartCleanupLoop(cleanupCtx, cfg.RetentionDays, cfg.RetentionPerProject)

	indexHTML, err := webui.Index()
	if err != nil {
		return fmt.Errorf("读取内嵌页面失败：%w", err)
	}
	assets, err := webui.Assets()
	if err != nil {
		return fmt.Errorf("读取内嵌静态资源失败：%w", err)
	}

	gin.SetMode(gin.ReleaseMode)
	if cfg.Debug {
		gin.SetMode(gin.DebugMode)
	}
	router := gin.New()
	router.Use(gin.LoggerWithWriter(ginSlogWriter{}), gin.Recovery())

	api.Register(router, api.RouterDeps{
		Server:    api.NewServer(st, pool, shellDir, version),
		Webhook:   webhook.New(st, pool),
		WebUser:   cfg.WebUser,
		WebPass:   cfg.WebPass,
		IndexHTML: indexHTML,
		Assets:    assets,
	})

	printBanner(cfg, dirs, shellDir.Root())

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 监听失败（端口被占等）要立刻退出，不能装作起来了
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return fmt.Errorf("HTTP 服务启动失败：%w", err)
	case sig := <-stop:
		slog.Info("收到退出信号，开始优雅停机", "signal", sig.String())
	}

	return shutdown(srv, pool, stopCleanup)
}

// shutdown 按顺序收尾：先停止接收新请求，再等在途脚本跑完，最后关库。
func shutdown(srv *http.Server, pool *executor.Pool, stopCleanup context.CancelFunc) error {
	stopCleanup()

	// 先把 HTTP 停掉，这样不会再有新的 webhook 进来。
	// 用 context.Background 而不是带超时的 ctx：Shutdown 自身会等
	// 空闲连接关闭，超时由下面的 executionGrace 统一兜底。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownGrace)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("HTTP 服务未能干净关闭", "err", err)
	} else {
		slog.Info("HTTP 服务已停止接收请求")
	}

	// 再等在途脚本执行完。超时的话 pool.Shutdown 会强杀进程组。
	pool.Shutdown(executionGrace)

	slog.Info("已退出")
	return nil
}

func setupLogger(debug bool) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
}

// printBanner 把运维最需要的几条信息一次性打清楚，
// 尤其是脚本目录和（随机生成的）密码。
func printBanner(cfg *config.Config, dirs *config.Dirs, shellRoot string) {
	slog.Info("hook-exec 已启动",
		"version", version,
		"listen", cfg.Listen,
		"data_dir", dirs.Root,
	)
	slog.Info("脚本目录（把你的脚本放进来并 chmod +x）", "path", shellRoot)
	slog.Info("管理页", "url", fmt.Sprintf("http://%s/web", displayAddr(cfg.Listen)))

	if cfg.WebPassGenerated {
		// 这条用 Warn 是有意的：随机密码只打印这一次，错过了就得重启换一个。
		slog.Warn("未指定密码，已生成随机密码。请立即保存，此密码只显示这一次",
			"user", cfg.WebUser,
			"password", cfg.WebPass,
		)
	} else {
		slog.Info("管理页认证已启用", "user", cfg.WebUser)
	}

	slog.Info("GitHub webhook 地址", "url", fmt.Sprintf("http://%s/", displayAddr(cfg.Listen)))
}

// displayAddr 把 :9000 之类的监听地址补成可点击的形式。
func displayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}

// ginSlogWriter 把 gin 的访问日志转给 slog，避免两套日志格式混在一起。
type ginSlogWriter struct{}

func (ginSlogWriter) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	if line != "" {
		slog.Debug(line)
	}
	return len(p), nil
}
