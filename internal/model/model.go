// Package model 定义持久化实体。
//
// 这里只放存储层需要的东西，不掺 JSON 标签 —— 对外的字段裁剪和密钥脱敏
// 由 api 层的 DTO 负责，免得「不小心把 secret 序列化出去了」这类问题
// 要在实体上反复检查。
package model

import "time"

// Project 是一条项目配置，对应管理页上的一张卡片。
type Project struct {
	ID   uint   `gorm:"primaryKey"`
	Name string `gorm:"size:128;not null"`

	// Repo 形如 owner/repo，是 webhook 热路径的查询键。
	// 和 Enabled 组成联合索引，对应 ListEnabledProjectsByRepo 的
	// "WHERE repo = ? AND enabled = ?"。
	Repo string `gorm:"size:255;not null;index:idx_projects_repo_enabled,priority:1"`

	// Secret 是 GitHub webhook 的 secret，用于校验 HMAC 签名。
	Secret string `gorm:"size:255;not null"`

	// Events 是逗号分隔的事件名列表，空串或含 "*" 表示匹配全部事件。
	Events string `gorm:"size:512"`

	// ShellFile 是脚本目录下的文件名（相对路径，不是绝对路径）。
	// 每次执行前都会重新走一遍 shelldir 的路径校验，这里存的值不被信任。
	ShellFile string `gorm:"size:255;not null"`

	// WorkDir 是脚本的工作目录，留空表示继承进程当前目录。
	WorkDir string `gorm:"size:512"`

	TimeoutSec int `gorm:"not null;default:300"`

	// 和 Repo 组成联合索引。单独给 enabled 建索引意义不大（只有两个取值），
	// 但作为联合索引的第二列就能让热路径查询直接用上索引。
	//
	// 刻意不写 `default:true`：GORM 对"带默认值且取值为零"的字段会跳过写入，
	// 把值交给数据库默认值，于是 Enabled=false 的项目会被静默存成 true，
	// 「停用」这个开关根本不生效。所有写入路径都会显式设置本字段，
	// 所以让 Go 侧的值说了算。
	Enabled bool `gorm:"not null;index:idx_projects_repo_enabled,priority:2"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Execution 是一次脚本执行记录。
type Execution struct {
	ID uint `gorm:"primaryKey"`

	// ProjectID 同时参与两个索引：
	//   - idx_exec_project_created 的复合索引首列（按项目分页）
	//   - idx_exec_delivery_project 唯一约束的次列（幂等）
	ProjectID uint `gorm:"index:idx_exec_project_created,priority:1;uniqueIndex:idx_exec_delivery_project,priority:2"`

	// ProjectName 是写入时的快照，项目改名或删除后记录仍然可读，
	// 列表查询也就不需要 JOIN 回 projects 表。
	ProjectName string `gorm:"size:128"`
	Repo        string `gorm:"size:255"`

	// DeliveryID 取自 X-GitHub-Delivery。GitHub 支持手动 redeliver，
	// 重复投递的 delivery 会撞上 (DeliveryID, ProjectID) 唯一约束，
	// 由此实现幂等。手动触发的记录会生成一个合成的 ID。
	DeliveryID string `gorm:"size:128;uniqueIndex:idx_exec_delivery_project,priority:1"`

	Event  string `gorm:"size:64"`
	Ref    string `gorm:"size:255"`
	Branch string `gorm:"size:255"`
	SHA    string `gorm:"size:64"`
	Actor  string `gorm:"size:128"`

	// Trigger 区分 webhook 与 manual。
	Trigger string `gorm:"size:16"`

	// ShellFile 是这次执行所用的脚本文件快照。
	ShellFile string `gorm:"size:255"`

	Status string `gorm:"size:16;not null;index"`

	// ExitCode 用来区分「退出码 0」和「根本没跑起来」，所以是指针。
	ExitCode *int
	Error    string `gorm:"type:text"`

	// Output 是 stdout 与 stderr 合流后的尾部若干字节。
	Output          string `gorm:"type:text"`
	OutputTruncated bool

	DurationMs int64
	StartedAt  *time.Time
	FinishedAt *time.Time

	CreatedAt time.Time `gorm:"index;index:idx_exec_project_created,priority:2,sort:desc"`
}

// TableName 让表名固定下来，不受 GORM 复数化规则变动影响。
func (Execution) TableName() string { return "executions" }

func (Project) TableName() string { return "projects" }
