package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zxc7563598/hook-exec/internal/model"
	"github.com/zxc7563598/hook-exec/internal/store"
)

// executionDTO 是执行记录对外的形态。列表与详情共用，
// 差别只在列表不带 output。
type executionDTO struct {
	ID              uint       `json:"id"`
	ProjectID       uint       `json:"project_id"`
	ProjectName     string     `json:"project_name"`
	Repo            string     `json:"repo"`
	Event           string     `json:"event"`
	DeliveryID      string     `json:"delivery_id"`
	Ref             string     `json:"ref"`
	Branch          string     `json:"branch"`
	SHA             string     `json:"sha"`
	Actor           string     `json:"actor"`
	Trigger         string     `json:"trigger"`
	ShellFile       string     `json:"shell_file"`
	Status          string     `json:"status"`
	ExitCode        *int       `json:"exit_code"`
	Error           string     `json:"error"`
	Output          string     `json:"output,omitempty"`
	OutputTruncated bool       `json:"output_truncated"`
	DurationMs      int64      `json:"duration_ms"`
	StartedAt       *time.Time `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

func toExecutionDTO(e model.Execution, withOutput bool) executionDTO {
	dto := executionDTO{
		ID:              e.ID,
		ProjectID:       e.ProjectID,
		ProjectName:     e.ProjectName,
		Repo:            e.Repo,
		Event:           e.Event,
		DeliveryID:      e.DeliveryID,
		Ref:             e.Ref,
		Branch:          e.Branch,
		SHA:             e.SHA,
		Actor:           e.Actor,
		Trigger:         e.Trigger,
		ShellFile:       e.ShellFile,
		Status:          e.Status,
		ExitCode:        e.ExitCode,
		Error:           e.Error,
		OutputTruncated: e.OutputTruncated,
		DurationMs:      e.DurationMs,
		StartedAt:       e.StartedAt,
		FinishedAt:      e.FinishedAt,
		CreatedAt:       e.CreatedAt,
	}
	if withOutput {
		dto.Output = e.Output
	}
	return dto
}

// ListExecutions GET /web/api/executions
func (s *Server) ListExecutions(c *gin.Context) {
	filter := store.ExecutionFilter{
		Status: c.Query("status"),
	}

	if v := c.Query("project_id"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			invalid(c, "参数校验未通过", []FieldError{{Field: "project_id", Message: "必须是数字"}})
			return
		}
		filter.ProjectID = uint(n)
	}

	if v := c.Query("status"); v != "" && !isKnownStatus(v) {
		invalid(c, "参数校验未通过", []FieldError{{Field: "status", Message: "未知的状态值"}})
		return
	}

	filter.Page = atoiDefault(c.Query("page"), 1)
	filter.PageSize = atoiDefault(c.Query("page_size"), store.DefaultPageSize)
	// 先归一化再查询，这样下面回显的就是实际生效的分页参数
	filter = filter.Normalize()

	records, total, err := s.store.ListExecutions(filter)
	if err != nil {
		fail(c, http.StatusInternalServerError, "读取执行记录失败")
		return
	}

	out := make([]executionDTO, 0, len(records))
	for _, r := range records {
		out = append(out, toExecutionDTO(r, false))
	}

	ok(c, gin.H{
		"executions": out,
		"total":      total,
		"page":       filter.Page,
		"page_size":  filter.PageSize,
	})
}

// GetExecution GET /web/api/executions/:id —— 含完整输出
func (s *Server) GetExecution(c *gin.Context) {
	id, okID := parseID(c)
	if !okID {
		return
	}

	rec, err := s.store.GetExecution(id)
	if err != nil {
		failStore(c, err, "记录不存在")
		return
	}
	ok(c, toExecutionDTO(*rec, true))
}

// DeleteExecution DELETE /web/api/executions/:id
func (s *Server) DeleteExecution(c *gin.Context) {
	id, okID := parseID(c)
	if !okID {
		return
	}
	if err := s.store.DeleteExecution(id); err != nil {
		failStore(c, err, "记录不存在")
		return
	}
	ok(c, gin.H{"id": id})
}

// ClearExecutions DELETE /web/api/executions?project_id=&status=
//
// 至少要带一个条件。两个都空的话拒绝执行 —— 一个手滑的请求就能把
// 整张表清掉，这种接口不该存在。
func (s *Server) ClearExecutions(c *gin.Context) {
	var projectID uint
	if v := c.Query("project_id"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			invalid(c, "参数校验未通过", []FieldError{{Field: "project_id", Message: "必须是数字"}})
			return
		}
		projectID = uint(n)
	}

	status := c.Query("status")
	if projectID == 0 && status == "" {
		invalid(c, "参数校验未通过", []FieldError{{
			Field:   "project_id",
			Message: "必须指定 project_id 或 status，不允许无条件清空",
		}})
		return
	}
	if status != "" && !isKnownStatus(status) {
		invalid(c, "参数校验未通过", []FieldError{{Field: "status", Message: "未知的状态值"}})
		return
	}

	// 参数已在上面校验过，所以这里剩下的只可能是数据库错误
	n, err := s.store.DeleteExecutions(projectID, status)
	if err != nil {
		slog.Error("清空执行记录失败", "err", err)
		fail(c, http.StatusInternalServerError, "清空失败")
		return
	}
	ok(c, gin.H{"deleted": n})
}

func isKnownStatus(s string) bool {
	for _, v := range model.AllStatuses {
		if v == s {
			return true
		}
	}
	return false
}

func atoiDefault(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}
