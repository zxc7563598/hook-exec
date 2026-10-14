package store

import (
	"errors"

	"gorm.io/gorm"

	"github.com/zxc7563598/hook-exec/internal/model"
)

// ErrNotFound 表示目标记录不存在。
var ErrNotFound = errors.New("记录不存在")

// ListProjects 按 ID 升序返回全部项目。
func (s *Store) ListProjects() ([]model.Project, error) {
	var out []model.Project
	err := s.db.Order("id asc").Find(&out).Error
	return out, err
}

// ListEnabledProjectsByRepo 是 webhook 热路径上的查询：按仓库名找启用中的项目。
func (s *Store) ListEnabledProjectsByRepo(repo string) ([]model.Project, error) {
	var out []model.Project
	err := s.db.Where("repo = ? AND enabled = ?", repo, true).
		Order("id asc").
		Find(&out).Error
	return out, err
}

// CountDisabledProjectsByRepo 数一下同仓库里被禁用的项目有几个。
//
// 只在"没匹配到任何启用项目"时才会调用，用来在 webhook 响应里区分
// 「压根没配」和「配了但停用了」—— 后者是运维最容易自己搞混的情况。
func (s *Store) CountDisabledProjectsByRepo(repo string) (int64, error) {
	var n int64
	err := s.db.Model(&model.Project{}).
		Where("repo = ? AND enabled = ?", repo, false).
		Count(&n).Error
	return n, err
}

// GetProject 按 ID 取单个项目。
func (s *Store) GetProject(id uint) (*model.Project, error) {
	var p model.Project
	err := s.db.First(&p, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &p, err
}

// CreateProject 新建项目，成功后回填 ID。
//
// 注意 Enabled 字段刻意没有 default 标签（原因见模型上的注释）：
// GORM 会跳过带默认值的零值字段，那会让 Enabled=false 被写成 true。
func (s *Store) CreateProject(p *model.Project) error {
	return s.db.Create(p).Error
}

// UpdateProject 全量更新项目配置。
func (s *Store) UpdateProject(p *model.Project) error {
	return s.db.Model(&model.Project{}).Where("id = ?", p.ID).Updates(map[string]any{
		"name":        p.Name,
		"repo":        p.Repo,
		"secret":      p.Secret,
		"events":      p.Events,
		"shell_file":  p.ShellFile,
		"work_dir":    p.WorkDir,
		"timeout_sec": p.TimeoutSec,
		"enabled":     p.Enabled,
	}).Error
}

// SetProjectEnabled 只改启用开关。
// 管理页卡片上的开关走这条，不必把整个表单提交上来。
func (s *Store) SetProjectEnabled(id uint, enabled bool) error {
	res := s.db.Model(&model.Project{}).Where("id = ?", id).Update("enabled", enabled)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteProject 删除项目。执行记录刻意保留 —— 那是历史事实，
// 项目没了不代表当时没跑过，页面上仍应能查到 ProjectName。
func (s *Store) DeleteProject(id uint) error {
	res := s.db.Delete(&model.Project{}, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// RepoExists 判断某个仓库名是否已被其它项目使用。
func (s *Store) RepoExists(repo string) (bool, error) {
	var n int64
	err := s.db.Model(&model.Project{}).Where("repo = ?", repo).Count(&n).Error
	return n > 0, err
}

// ProjectExistsByNameAndRepo 供导入时去重用：同名同仓库视为同一条配置，
// 重复导入应当跳过而不是再建一条。
func (s *Store) ProjectExistsByNameAndRepo(name, repo string) (bool, error) {
	var n int64
	err := s.db.Model(&model.Project{}).
		Where("name = ? AND repo = ?", name, repo).
		Count(&n).Error
	return n > 0, err
}
