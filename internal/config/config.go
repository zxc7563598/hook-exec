// Package config 负责启动参数与环境变量的解析和校验。
//
// 密码支持环境变量是为了避免它出现在 ps 输出和 shell history 里。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// 环境变量名。命令行参数的优先级高于环境变量。
const (
	EnvListen  = "HOOKEXEC_LISTEN"
	EnvWebUser = "HOOKEXEC_WEB_USER"
	EnvWebPass = "HOOKEXEC_WEB_PASS"
)

// 默认值
const (
	DefaultListen              = "127.0.0.1:9000"
	DefaultWebUser             = "admin"
	DefaultConcurrency         = 4
	DefaultRetentionDays       = 30
	DefaultRetentionPerProject = 500
)

// Config 是进程启动后不再变化的配置。
type Config struct {
	Listen              string
	DataDir             string // 空表示用可执行文件所在目录
	WebUser             string
	WebPass             string
	WebPassGenerated    bool // 密码是随机生成的（需要打印出来提醒用户保存）
	Concurrency         int
	RetentionDays       int
	RetentionPerProject int
	Debug               bool
}

// Parse 解析命令行参数。stderr 用于输出 usage 和 flag 错误信息。
func Parse(args []string, stderr io.Writer) (*Config, error) {
	fs := flag.NewFlagSet("hook-exec", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var c Config
	fs.StringVar(&c.Listen, "listen", envOr(EnvListen, DefaultListen), "HTTP 监听地址")
	fs.StringVar(&c.DataDir, "data-dir", "", "数据目录，存放数据库与 shell/ 脚本目录；默认取可执行文件所在目录")
	fs.StringVar(&c.WebUser, "web-user", envOr(EnvWebUser, DefaultWebUser), "管理页 Basic Auth 用户名")
	fs.StringVar(&c.WebPass, "web-pass", envOr(EnvWebPass, ""), "管理页 Basic Auth 密码（也可用 "+EnvWebPass+" 传入）")
	fs.IntVar(&c.Concurrency, "concurrency", DefaultConcurrency, "全局同时执行的脚本数上限")
	fs.IntVar(&c.RetentionDays, "retention-days", DefaultRetentionDays, "执行记录保留天数")
	fs.IntVar(&c.RetentionPerProject, "retention-per-project", DefaultRetentionPerProject, "每个项目保留的执行记录条数上限")
	fs.BoolVar(&c.Debug, "debug", false, "调试模式：gin 非 release 模式 + debug 级日志")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "hook-exec — 接收 GitHub webhook 并执行 shell 目录下的脚本\n\n用法：\n  hook-exec [参数]\n\n参数：\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("不支持的位置参数：%s", strings.Join(fs.Args(), " "))
	}

	if c.Concurrency < 1 {
		return nil, fmt.Errorf("-concurrency 必须大于 0，当前为 %d", c.Concurrency)
	}
	if c.RetentionDays < 1 {
		return nil, fmt.Errorf("-retention-days 必须大于 0，当前为 %d", c.RetentionDays)
	}
	if c.RetentionPerProject < 1 {
		return nil, fmt.Errorf("-retention-per-project 必须大于 0，当前为 %d", c.RetentionPerProject)
	}
	if c.WebUser == "" {
		return nil, fmt.Errorf("-web-user 不能为空")
	}

	// 没给密码就生成一个随机的，绝不裸奔。生成出来的密码由调用方打印一次。
	if c.WebPass == "" {
		pass, err := randomPassword()
		if err != nil {
			return nil, fmt.Errorf("生成随机密码失败：%w", err)
		}
		c.WebPass = pass
		c.WebPassGenerated = true
	}

	return &c, nil
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func randomPassword() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
