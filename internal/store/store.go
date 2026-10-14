// Package store 封装 SQLite 的读写。
package store

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/zxc7563598/hook-exec/internal/model"
)

// Store 是数据访问入口。
type Store struct {
	db *gorm.DB
}

// Open 打开（必要时创建）数据库并完成建表。
//
// dbPath 由调用方从数据目录派生，本包不关心目录是怎么定出来的。
func Open(dbPath string, debug bool) (*Store, error) {
	logMode := logger.Error
	if debug {
		logMode = logger.Info
	}

	db, err := gorm.Open(sqlite.Open(dsn(dbPath)), &gorm.Config{
		Logger: logger.Default.LogMode(logMode),
	})
	if err != nil {
		return nil, fmt.Errorf("打开数据库 %s 失败：%w", dbPath, err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("获取数据库连接失败：%w", err)
	}

	// 只保留一条连接，让 database/sql 的连接池把读写全部串行化。
	//
	// 这样做有两个好处：彻底没有 SQLITE_BUSY；并且"多个 worker 并发写库
	// 要不要单写协程收敛"这个问题直接消失了。代价是读也没法并发，
	// 但本项目的负载是低频 webhook 加偶尔打开的管理页，完全无所谓。
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(0)

	if err := db.AutoMigrate(&model.Project{}, &model.Execution{}); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("建表失败：%w", err)
	}

	return &Store{db: db}, nil
}

// dsn 拼出带 pragma 的连接串。
//
// 关键点：pragma 必须写进 DSN，因为它是**每连接**生效的。用
// db.Exec("PRAGMA busy_timeout=...") 设置只会作用于池中碰巧执行它的
// 那一条连接，其余连接没有这个设置。
//
// 另一个关键点：auto_vacuum 必须排在 journal_mode(WAL) **之前**。
// WAL 一启用就会把数据库文件建出来，而 auto_vacuum 只能在空库上设置；
// 顺序反了会静默失效（实测 pragma 读回来是 0）。
func dsn(dbPath string) string {
	q := url.Values{}
	q.Add("_pragma", "auto_vacuum(2)") // 必须在最前：2 = INCREMENTAL
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")

	u := url.URL{Scheme: "file", Path: dbPath, RawQuery: q.Encode()}
	return u.String()
}

// Close 关闭数据库。
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// ReconcileOnStartup 把所有停留在 pending/running 的记录标成 interrupted。
//
// 进程被 kill -9 或崩溃时，正在执行和排队中的记录会永远停在中间状态 ——
// 页面上看起来像是一直在跑。启动时对账一次，把它们收尾。
func (s *Store) ReconcileOnStartup() (int64, error) {
	now := time.Now()
	res := s.db.Model(&model.Execution{}).
		Where("status IN ?", []string{model.StatusPending, model.StatusRunning}).
		Updates(map[string]any{
			"status":      model.StatusInterrupted,
			"error":       "服务重启，执行结果未知",
			"finished_at": &now,
		})
	return res.RowsAffected, res.Error
}

// Cleanup 按保留策略删除旧记录，返回删除条数。
//
// 两条约束取更严格者：
//   - 早于 retentionDays 的记录
//   - 每个项目超过 perProject 条的更早部分
//
// 第二条是为了防突发事件：某天 webhook 被刷了几百次，光靠天数约束
// 会把这段时间的记录全留下。
func (s *Store) Cleanup(retentionDays, perProject int) (int64, error) {
	var total int64

	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	res := s.db.Where("created_at < ?", cutoff).Delete(&model.Execution{})
	if res.Error != nil {
		return total, res.Error
	}
	total += res.RowsAffected

	// 窗口函数按项目分区编号，只保留每项目最新的 perProject 条。
	res = s.db.Exec(`
		DELETE FROM executions
		WHERE id NOT IN (
			SELECT id FROM (
				SELECT id,
				       ROW_NUMBER() OVER (
				           PARTITION BY project_id
				           ORDER BY created_at DESC, id DESC
				       ) AS rn
				FROM executions
			) WHERE rn <= ?
		)`, perProject)
	if res.Error != nil {
		return total, res.Error
	}
	total += res.RowsAffected

	if total > 0 {
		// 开着 auto_vacuum=INCREMENTAL 才有的回收手段。删行不会自动缩小
		// 文件，不回收的话磁盘占用只增不减。
		if err := s.db.Exec("PRAGMA incremental_vacuum").Error; err != nil {
			slog.Warn("回收数据库空间失败", "err", err)
		}
	}
	return total, nil
}

// StartCleanupLoop 起一个后台协程按小时跑清理，直到 ctx 取消。
func (s *Store) StartCleanupLoop(ctx context.Context, retentionDays, perProject int) {
	go func() {
		// 启动时先跑一次，省得积压要等一小时。
		s.runCleanup(retentionDays, perProject)

		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runCleanup(retentionDays, perProject)
			}
		}
	}()
}

func (s *Store) runCleanup(retentionDays, perProject int) {
	n, err := s.Cleanup(retentionDays, perProject)
	if err != nil {
		slog.Error("清理执行记录失败", "err", err)
		return
	}
	if n > 0 {
		slog.Info("已清理执行记录", "deleted", n)
	}
}
