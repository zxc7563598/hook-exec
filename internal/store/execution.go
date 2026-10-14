package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/zxc7563598/hook-exec/internal/model"
)

// ExecutionFilter 是执行记录的查询条件。
type ExecutionFilter struct {
	ProjectID uint   // 0 表示不限
	Status    string // 空表示不限
	Page      int    // 从 1 开始
	PageSize  int
}

// 分页边界。
const (
	DefaultPageSize = 20
	MaxPageSize     = 200
)

// Normalize 把分页参数收进合法范围。page_size 必须封顶，
// 否则一个 page_size=1000000 的请求就能把内存拉满。
//
// 调用方（api 层）会先调用它再把结果原样回显给前端，这样响应里的
// page/page_size 就是**实际生效**的值，而不是前端传进来的原始值。
func (f ExecutionFilter) Normalize() ExecutionFilter {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 {
		f.PageSize = DefaultPageSize
	}
	if f.PageSize > MaxPageSize {
		f.PageSize = MaxPageSize
	}
	return f
}

// CreateExecutionIfAbsent 插入一条执行记录，重复则跳过。
//
// 靠 (delivery_id, project_id) 的唯一约束做幂等：GitHub 支持手动
// redeliver，同一个 delivery 会被再投一次，这里直接判为重复，不重复执行。
// 返回 created=false 表示撞上了已有记录。
func (s *Store) CreateExecutionIfAbsent(e *model.Execution) (created bool, err error) {
	res := s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(e)
	if res.Error != nil {
		return false, res.Error
	}
	// OnConflict DoNothing 命中冲突时 RowsAffected 为 0，且不会回填 ID。
	return res.RowsAffected > 0, nil
}

// MarkExecutionRunning 记录开始执行。
func (s *Store) MarkExecutionRunning(id uint, startedAt time.Time) error {
	return s.db.Model(&model.Execution{}).Where("id = ?", id).Updates(map[string]any{
		"status":     model.StatusRunning,
		"started_at": &startedAt,
	}).Error
}

// FinishExecution 写入最终状态。duration 由调用方算好，避免这里再依赖时钟。
func (s *Store) FinishExecution(e *model.Execution, duration time.Duration) error {
	finished := time.Now()
	return s.db.Model(&model.Execution{}).Where("id = ?", e.ID).Updates(map[string]any{
		"status":           e.Status,
		"exit_code":        e.ExitCode,
		"error":            e.Error,
		"output":           e.Output,
		"output_truncated": e.OutputTruncated,
		"duration_ms":      duration.Milliseconds(),
		"finished_at":      &finished,
	}).Error
}

// MarkExecutionSkipped 把记录标成已跳过（队列满、项目被移除等）。
// 与 FinishExecution 分开是因为这类记录压根没开始执行，没有耗时和退出码。
func (s *Store) MarkExecutionSkipped(id uint, reason string) error {
	finished := time.Now()
	return s.db.Model(&model.Execution{}).Where("id = ?", id).Updates(map[string]any{
		"status":      model.StatusSkipped,
		"error":       reason,
		"finished_at": &finished,
	}).Error
}

// GetExecution 取单条记录，含完整输出。
func (s *Store) GetExecution(id uint) (*model.Execution, error) {
	var e model.Execution
	err := s.db.First(&e, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &e, err
}

// ListExecutions 按条件分页返回记录，并给出总数。
//
// 列表刻意 Omit("output")：单条输出最大 32KB，一页 20 条就是 640KB，
// 而列表上根本不显示正文。正文只在详情接口里取。
//
// 这里是 offset 分页而非键集分页。记录量被保留策略压着（每项目几百条），
// offset 的开销可以忽略，换来的是和筛选条件组合时简单得多。
func (s *Store) ListExecutions(f ExecutionFilter) ([]model.Execution, int64, error) {
	f = f.Normalize()

	query := s.db.Model(&model.Execution{})
	if f.ProjectID > 0 {
		query = query.Where("project_id = ?", f.ProjectID)
	}
	if f.Status != "" {
		query = query.Where("status = ?", f.Status)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var out []model.Execution
	err := query.
		Omit("output").
		Order("created_at DESC, id DESC").
		Limit(f.PageSize).
		Offset((f.Page - 1) * f.PageSize).
		Find(&out).Error
	return out, total, err
}

// LatestExecutionsByProject 一次查出每个项目最新的一条记录，按项目 ID 索引。
//
// 管理页的每张卡片都要显示"最近一次执行"，逐个查会变成 N+1。
// 这里用 MAX(id) 取每个项目的最新一条 —— 自增主键的顺序就是写入顺序，
// 也就等于时间顺序。刻意不 SELECT output，那列最大 32KB，卡片上根本用不到。
func (s *Store) LatestExecutionsByProject() (map[uint]model.Execution, error) {
	var rows []model.Execution
	err := s.db.Raw(`
		SELECT e.id, e.project_id, e.project_name, e.repo, e.event,
		       e.delivery_id, e.ref, e.branch, e.sha, e.actor, e.trigger,
		       e.shell_file, e.status, e.exit_code, e.error,
		       e.output_truncated, e.duration_ms, e.started_at, e.finished_at, e.created_at
		FROM executions e
		JOIN (
			SELECT project_id, MAX(id) AS max_id
			FROM executions
			GROUP BY project_id
		) latest ON e.id = latest.max_id
	`).Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	out := make(map[uint]model.Execution, len(rows))
	for _, r := range rows {
		out[r.ProjectID] = r
	}
	return out, nil
}

// DeleteExecution 删除单条记录。
func (s *Store) DeleteExecution(id uint) error {
	res := s.db.Delete(&model.Execution{}, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteExecutions 按条件批量删除。两个条件都为空时拒绝执行，
// 免得一个手滑的请求把整张表清空。
func (s *Store) DeleteExecutions(projectID uint, status string) (int64, error) {
	if projectID == 0 && status == "" {
		return 0, errors.New("必须指定 project_id 或 status，不允许无条件清空")
	}
	query := s.db.Model(&model.Execution{})
	if projectID > 0 {
		query = query.Where("project_id = ?", projectID)
	}
	if status != "" {
		query = query.Where("status = ?", status)
	}
	res := query.Delete(&model.Execution{})
	return res.RowsAffected, res.Error
}

// CountExecutions 统计记录总数，供页面展示。
func (s *Store) CountExecutions() (int64, error) {
	var n int64
	err := s.db.Model(&model.Execution{}).Count(&n).Error
	return n, err
}
