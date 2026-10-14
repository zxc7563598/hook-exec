package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zxc7563598/hook-exec/internal/model"
)

// configFileVersion 用于将来格式变动时做兼容判断。
const configFileVersion = 1

// exportProject 是导出文件里的单个项目，含明文 secret。
//
// 导出文件是有敏感性的：拿到它就等于能伪造 webhook。
// 页面上会明确提示这一点。
type exportProject struct {
	Name      string   `json:"name"`
	Repo      string   `json:"repo"`
	Secret    string   `json:"secret"`
	Events    []string `json:"events"`
	ShellFile string   `json:"shell_file"`
	WorkDir   string   `json:"workdir"`
	// TimeoutSec 用指针是为了区分"没写"和"写了 0"：前者用默认值，
	// 后者也是默认值，但语义不同，指针能让两种写法都落到正确的行为上。
	TimeoutSec *int `json:"timeout_sec"`
	// Enabled 用指针是为了和创建接口语义一致：省略时视为启用（true），
	// 而不是 Go 的零值 false。否则一份手写或裁剪过的导入文件会把项目
	// 静默导成停用状态。
	Enabled *bool `json:"enabled"`
}

type configFile struct {
	Version    int             `json:"version"`
	ExportedAt time.Time       `json:"exported_at"`
	Projects   []exportProject `json:"projects"`
}

// ExportConfig GET /web/api/config/export
func (s *Server) ExportConfig(c *gin.Context) {
	projects, err := s.store.ListProjects()
	if err != nil {
		fail(c, http.StatusInternalServerError, "读取项目列表失败")
		return
	}

	file := configFile{
		Version:    configFileVersion,
		ExportedAt: time.Now(),
		Projects:   make([]exportProject, 0, len(projects)),
	}
	for _, p := range projects {
		timeout := p.TimeoutSec
		enabled := p.Enabled
		file.Projects = append(file.Projects, exportProject{
			Name:       p.Name,
			Repo:       p.Repo,
			Secret:     p.Secret,
			Events:     model.DecodeEvents(p.Events),
			ShellFile:  p.ShellFile,
			WorkDir:    p.WorkDir,
			TimeoutSec: &timeout,
			Enabled:    &enabled,
		})
	}

	// 带 Content-Disposition 让浏览器直接存成文件
	c.Header("Content-Disposition", `attachment; filename="hook-exec-config.json"`)
	c.JSON(http.StatusOK, file)
}

// importResult 汇报导入结果。失败项逐条说明原因，不整体回滚 ——
// 导 20 条进去成功 18 条，比一条都不进去有用得多。
type importResult struct {
	Imported int          `json:"imported"`
	Skipped  int          `json:"skipped"`
	Failed   int          `json:"failed"`
	Warnings []FieldError `json:"warnings,omitempty"`
	Errors   []FieldError `json:"errors,omitempty"`
}

// ImportConfig POST /web/api/config/import
func (s *Server) ImportConfig(c *gin.Context) {
	var file configFile
	if err := c.ShouldBindJSON(&file); err != nil {
		fail(c, http.StatusBadRequest, "请求体不是合法的配置文件")
		return
	}

	if file.Version > configFileVersion {
		fail(c, http.StatusBadRequest, "配置文件版本高于当前程序支持的版本")
		return
	}

	var result importResult
	for i, ep := range file.Projects {
		label := ep.Name
		if label == "" {
			label = ep.Repo
		}
		// 用序号+名字定位，方便在提示里说清是哪一条
		field := "projects[" + strconv.Itoa(i) + "]"

		in := &projectInput{
			Name:      ep.Name,
			Repo:      ep.Repo,
			Secret:    ep.Secret,
			Events:    ep.Events,
			ShellFile: ep.ShellFile,
			WorkDir:   ep.WorkDir,
			// 省略 enabled 视为启用，和 POST /web/api/projects 保持一致
			Enabled: ep.Enabled,
		}
		if ep.TimeoutSec != nil {
			in.TimeoutSec = *ep.TimeoutSec
		}

		// 导入时脚本文件缺失只算警告：先导配置再传脚本是合理顺序。
		fields, warnings := s.validateInput(in, true, false)
		if len(fields) > 0 {
			result.Failed++
			result.Errors = append(result.Errors, FieldError{
				Field:   field,
				Message: label + "：" + joinFieldMessages(fields),
			})
			continue
		}
		for _, w := range warnings {
			result.Warnings = append(result.Warnings, FieldError{
				Field:   field,
				Message: label + "：" + w.Message,
			})
		}

		// 同名同仓库的项目跳过，避免重复导入把列表刷成两份。
		exists, err := s.store.ProjectExistsByNameAndRepo(strings.TrimSpace(ep.Name), strings.TrimSpace(ep.Repo))
		if err != nil {
			result.Failed++
			result.Errors = append(result.Errors, FieldError{Field: field, Message: label + "：查询失败"})
			continue
		}
		if exists {
			result.Skipped++
			continue
		}

		timeout := defaultTimeout
		if ep.TimeoutSec != nil && *ep.TimeoutSec > 0 {
			timeout = *ep.TimeoutSec
		}
		enabled := true
		if ep.Enabled != nil {
			enabled = *ep.Enabled
		}
		p := &model.Project{
			Name:       strings.TrimSpace(ep.Name),
			Repo:       strings.TrimSpace(ep.Repo),
			Secret:     ep.Secret,
			Events:     model.EncodeEvents(ep.Events),
			ShellFile:  strings.TrimSpace(ep.ShellFile),
			WorkDir:    strings.TrimSpace(ep.WorkDir),
			TimeoutSec: timeout,
			Enabled:    enabled,
		}
		if err := s.store.CreateProject(p); err != nil {
			result.Failed++
			result.Errors = append(result.Errors, FieldError{Field: field, Message: label + "：写入失败"})
			continue
		}
		result.Imported++
	}

	ok(c, result)
}

func joinFieldMessages(fields []FieldError) string {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, f.Message)
	}
	return strings.Join(parts, "；")
}
