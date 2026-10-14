package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zxc7563598/hook-exec/internal/executor"
	"github.com/zxc7563598/hook-exec/internal/model"
	"github.com/zxc7563598/hook-exec/internal/shelldir"
	"github.com/zxc7563598/hook-exec/internal/store"
)

// repoPattern 校验 owner/name 形式。GitHub 的允许字符是字母数字和 . _ -
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

const (
	maxNameLen     = 128
	maxShellLen    = 255
	maxWorkDirLen  = 512
	maxTimeoutSec  = 86400
	defaultTimeout = 300
)

// projectDTO 是项目对外的形态。
//
// Secret 永远不回传明文，只给一个掩码和"是否已设置"。管理页上编辑时
// 输入框留空表示不修改 —— 页面本来也不需要知道原值。
type projectDTO struct {
	ID         uint      `json:"id"`
	Name       string    `json:"name"`
	Repo       string    `json:"repo"`
	SecretMask string    `json:"secret_mask"`
	HasSecret  bool      `json:"has_secret"`
	Events     []string  `json:"events"`
	ShellFile  string    `json:"shell_file"`
	WorkDir    string    `json:"workdir"`
	TimeoutSec int       `json:"timeout_sec"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`

	// 脚本文件在磁盘上的现状。列表里带上，卡片就能直接标出
	// 「文件没了」或「没有可执行位」，不必等到执行失败才发现。
	ScriptExists     bool `json:"script_exists"`
	ScriptExecutable bool `json:"script_executable"`

	// 最近一次执行，供卡片底部展示
	LastExecution *lastExecutionDTO `json:"last_execution,omitempty"`
}

type lastExecutionDTO struct {
	ID         uint       `json:"id"`
	Status     string     `json:"status"`
	FinishedAt *time.Time `json:"finished_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// projectInput 是新建/更新项目的入参。
type projectInput struct {
	Name       string   `json:"name"`
	Repo       string   `json:"repo"`
	Secret     string   `json:"secret"`
	Events     []string `json:"events"`
	ShellFile  string   `json:"shell_file"`
	WorkDir    string   `json:"workdir"`
	TimeoutSec int      `json:"timeout_sec"`
	Enabled    *bool    `json:"enabled"`
}

func (s *Server) toDTO(p model.Project) projectDTO {
	dto := projectDTO{
		ID:         p.ID,
		Name:       p.Name,
		Repo:       p.Repo,
		HasSecret:  p.Secret != "",
		SecretMask: maskSecret(p.Secret),
		Events:     model.DecodeEvents(p.Events),
		ShellFile:  p.ShellFile,
		WorkDir:    p.WorkDir,
		TimeoutSec: p.TimeoutSec,
		Enabled:    p.Enabled,
		CreatedAt:  p.CreatedAt,
		UpdatedAt:  p.UpdatedAt,
	}

	// 脚本文件的实际状态。文件可能已经被运维从磁盘上删掉，
	// 或者从来没传上来过（比如导入配置之后）。
	if info, err := s.shellDir.Stat(p.ShellFile); err == nil {
		dto.ScriptExists = true
		dto.ScriptExecutable = info.Executable
	}

	return dto
}

func maskSecret(secret string) string {
	if secret == "" {
		return ""
	}
	const visible = 4
	if len(secret) <= visible {
		return strings.Repeat("•", len(secret))
	}
	return secret[:visible] + strings.Repeat("•", 8)
}

// ListProjects GET /web/api/projects
func (s *Server) ListProjects(c *gin.Context) {
	projects, err := s.store.ListProjects()
	if err != nil {
		fail(c, http.StatusInternalServerError, "读取项目列表失败")
		return
	}

	latest, err := s.store.LatestExecutionsByProject()
	if err != nil {
		fail(c, http.StatusInternalServerError, "读取最近执行记录失败")
		return
	}

	out := make([]projectDTO, 0, len(projects))
	for _, p := range projects {
		dto := s.toDTO(p)
		if last, found := latest[p.ID]; found {
			dto.LastExecution = &lastExecutionDTO{
				ID:         last.ID,
				Status:     last.Status,
				FinishedAt: last.FinishedAt,
				CreatedAt:  last.CreatedAt,
			}
		}
		out = append(out, dto)
	}

	ok(c, gin.H{
		"projects":      out,
		"shell_dir":     s.shellRoot,
		"common_events": model.CommonEvents,
		"all_statuses":  model.AllStatuses,
	})
}

// CreateProject POST /web/api/projects
func (s *Server) CreateProject(c *gin.Context) {
	var in projectInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}

	if fields, _ := s.validateInput(&in, true, true); len(fields) > 0 {
		invalid(c, "参数校验未通过", fields)
		return
	}

	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}

	p := &model.Project{
		Name:       strings.TrimSpace(in.Name),
		Repo:       strings.TrimSpace(in.Repo),
		Secret:     in.Secret,
		Events:     model.EncodeEvents(in.Events),
		ShellFile:  strings.TrimSpace(in.ShellFile),
		WorkDir:    strings.TrimSpace(in.WorkDir),
		TimeoutSec: in.TimeoutSec,
		Enabled:    enabled,
	}
	if p.TimeoutSec == 0 {
		p.TimeoutSec = defaultTimeout
	}

	if err := s.store.CreateProject(p); err != nil {
		fail(c, http.StatusInternalServerError, "创建项目失败")
		return
	}
	ok(c, s.toDTO(*p))
}

// UpdateProject PUT /web/api/projects/:id
func (s *Server) UpdateProject(c *gin.Context) {
	id, okID := parseID(c)
	if !okID {
		return
	}

	existing, err := s.store.GetProject(id)
	if err != nil {
		failStore(c, err, "项目不存在")
		return
	}

	var in projectInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}

	// secret 留空表示不修改。否则每次编辑都得把原值重新输一遍，
	// 而我们的列表接口又刻意不回传明文。
	if strings.TrimSpace(in.Secret) == "" {
		in.Secret = existing.Secret
	}

	if fields, _ := s.validateInput(&in, true, true); len(fields) > 0 {
		invalid(c, "参数校验未通过", fields)
		return
	}

	updated := *existing
	updated.Name = strings.TrimSpace(in.Name)
	updated.Repo = strings.TrimSpace(in.Repo)
	updated.Secret = in.Secret
	updated.Events = model.EncodeEvents(in.Events)
	updated.ShellFile = strings.TrimSpace(in.ShellFile)
	updated.WorkDir = strings.TrimSpace(in.WorkDir)
	updated.TimeoutSec = in.TimeoutSec
	if updated.TimeoutSec == 0 {
		updated.TimeoutSec = defaultTimeout
	}
	if in.Enabled != nil {
		updated.Enabled = *in.Enabled
	}

	if err := s.store.UpdateProject(&updated); err != nil {
		fail(c, http.StatusInternalServerError, "更新项目失败")
		return
	}
	ok(c, s.toDTO(updated))
}

// SetProjectEnabled PATCH /web/api/projects/:id/enabled
//
// 卡片上的开关走这个接口，只需要提交一个布尔值，
// 不必把整个表单回传 —— 也就不会因为表单里某个字段没填对而调不动开关。
func (s *Server) SetProjectEnabled(c *gin.Context) {
	id, okID := parseID(c)
	if !okID {
		return
	}

	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Enabled == nil {
		invalid(c, "参数校验未通过", []FieldError{{Field: "enabled", Message: "必须提供布尔值"}})
		return
	}

	if err := s.store.SetProjectEnabled(id, *body.Enabled); err != nil {
		failStore(c, err, "项目不存在")
		return
	}

	if p, err := s.store.GetProject(id); err == nil {
		ok(c, s.toDTO(*p))
		return
	}
	ok(c, gin.H{"id": id, "enabled": *body.Enabled})
}

// DeleteProject DELETE /web/api/projects/:id
func (s *Server) DeleteProject(c *gin.Context) {
	id, okID := parseID(c)
	if !okID {
		return
	}

	if err := s.store.DeleteProject(id); err != nil {
		failStore(c, err, "项目不存在")
		return
	}

	// 顺带停掉这个项目的执行通道。在途任务会跑完、队列里剩下的会被排空，
	// 之后那条 goroutine 自己退出。
	s.pool.RemoveLane(id)
	ok(c, gin.H{"id": id})
}

// RunProject POST /web/api/projects/:id/run
//
// 手动执行一次。不经过 GitHub，方便在没有真实推送的情况下调试脚本。
func (s *Server) RunProject(c *gin.Context) {
	id, okID := parseID(c)
	if !okID {
		return
	}

	p, err := s.store.GetProject(id)
	if err != nil {
		failStore(c, err, "项目不存在")
		return
	}

	if s.pool.Draining() {
		fail(c, http.StatusServiceUnavailable, "服务正在停机")
		return
	}

	rec := &model.Execution{
		ProjectID:   p.ID,
		ProjectName: p.Name,
		Repo:        p.Repo,
		Event:       "manual",
		// 手动触发没有 delivery id，但唯一约束是 (DeliveryID, ProjectID)，
		// 若都用空串就会互相冲突。用时间戳造一个合成值。
		DeliveryID: fmt.Sprintf("manual-%d", time.Now().UnixNano()),
		Trigger:    model.TriggerManual,
		ShellFile:  p.ShellFile,
		Status:     model.StatusPending,
	}

	switch outcome, reason := s.pool.Enqueue(rec, p, nil); outcome {
	case executor.EnqueueQueued:
		ok(c, gin.H{"execution_id": rec.ID, "status": rec.Status})
	case executor.EnqueueDuplicate:
		fail(c, http.StatusConflict, "重复触发")
	default:
		// 队列满、项目刚被移除、写记录失败 —— 把真实原因带出去
		fail(c, http.StatusServiceUnavailable, reason)
	}
}

// validateInput 校验项目入参，返回字段级错误列表。
//
// scriptStrict 为 false 时，"脚本文件不存在"只算警告不算错误。
// 导入配置走的就是这条路：先导入配置、再把脚本传上去是合理顺序，
// 不该因为顺序问题就整体失败。真正的强制点在执行时（run.go 里每次都重解析）。
func (s *Server) validateInput(in *projectInput, requireSecret, scriptStrict bool) (fields []FieldError, warnings []FieldError) {
	add := func(field, msg string) { fields = append(fields, FieldError{Field: field, Message: msg}) }
	warn := func(field, msg string) { warnings = append(warnings, FieldError{Field: field, Message: msg}) }

	name := strings.TrimSpace(in.Name)
	switch {
	case name == "":
		add("name", "项目名称不能为空")
	case len(name) > maxNameLen:
		add("name", fmt.Sprintf("项目名称不能超过 %d 个字符", maxNameLen))
	}

	repo := strings.TrimSpace(in.Repo)
	switch {
	case repo == "":
		add("repo", "仓库不能为空")
	case !repoPattern.MatchString(repo):
		add("repo", "仓库格式应为 owner/repo")
	}

	if requireSecret && strings.TrimSpace(in.Secret) == "" {
		add("secret", "密钥不能为空，请填写 GitHub webhook 的 secret")
	}

	// 脚本文件必须真实存在且位于脚本目录内。界面上是下拉选择，
	// 正常不会撞到这条；它挡的是直接打 API、以及从库里被改坏的配置。
	shellFile := strings.TrimSpace(in.ShellFile)
	switch {
	case shellFile == "":
		add("shell_file", "请选择要执行的脚本文件")
	case len(shellFile) > maxShellLen:
		add("shell_file", "脚本文件名过长")
	default:
		if _, err := s.shellDir.Resolve(shellFile); err != nil {
			// 文件暂时不存在只算警告：先把配置导进来、稍后再把脚本传上去，
			// 是合理的顺序。但路径本身不合法（越界、绝对路径、不是普通文件）
			// 是另一回事 —— 那种值永远不会变得可用，必须当场拒绝，
			// 不能让一条注定跑不起来的配置进到库里。
			recoverable := errors.Is(err, shelldir.ErrNotFound) && !scriptStrict
			if recoverable {
				warn("shell_file", fmt.Sprintf("脚本 %s 当前不可用：%v", shellFile, err))
			} else {
				add("shell_file", fmt.Sprintf("脚本文件不可用：%v", err))
			}
		}
	}

	workdir := strings.TrimSpace(in.WorkDir)
	if workdir != "" && len(workdir) > maxWorkDirLen {
		add("workdir", fmt.Sprintf("工作目录路径不能超过 %d 个字符", maxWorkDirLen))
	}

	if in.TimeoutSec < 0 || in.TimeoutSec > maxTimeoutSec {
		add("timeout_sec", fmt.Sprintf("超时时间应在 1 到 %d 秒之间（填 0 表示使用默认的 %d 秒）", maxTimeoutSec, defaultTimeout))
	}

	return fields, warnings
}

// parseID 从路径里取 :id。
func parseID(c *gin.Context) (uint, bool) {
	raw := c.Param("id")
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || n == 0 {
		fail(c, http.StatusBadRequest, "无效的 ID")
		return 0, false
	}
	return uint(n), true
}

// failStore 把 store 层的错误翻译成状态码和消息。
//
// 之前这里一律回"项目不存在"，即使真实原因是数据库故障 —— 会把运维
// 往完全错误的方向带。现在只有确实是 ErrNotFound 才回 404，其余记日志回 500。
func failStore(c *gin.Context, err error, notFoundMsg string) {
	if errors.Is(err, store.ErrNotFound) {
		fail(c, http.StatusNotFound, notFoundMsg)
		return
	}
	slog.Error("数据访问失败", "err", err)
	fail(c, http.StatusInternalServerError, "内部错误")
}
