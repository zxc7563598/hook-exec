package executor

import (
	"log/slog"

	"github.com/zxc7563598/hook-exec/internal/model"
)

// EnqueueOutcome 说明一次入队的结果。
type EnqueueOutcome int

const (
	// EnqueueQueued 记录已入库且任务已排队。
	EnqueueQueued EnqueueOutcome = iota
	// EnqueueDuplicate 同一个 delivery 已经处理过（GitHub 的 redeliver），不重复执行。
	EnqueueDuplicate
	// EnqueueSkipped 确认要执行但没排上（队列满／项目被移除），记录已落库为 skipped。
	EnqueueSkipped
	// EnqueueDraining 服务正在停机，没有受理。
	EnqueueDraining
)

// Enqueue 是「建记录 + 排进队列」的统一入口，webhook 和手动触发共用。
//
// 本方法不做任何阻塞等待，webhook 拿到结果就能立刻返回 200。
// 第二个返回值是未排队成功时的原因，供调用方原样转达给用户 ——
// "队列满"和"项目已被移除"是两回事，不该被压成同一句提示。
func (p *Pool) Enqueue(rec *model.Execution, proj *model.Project, payload []byte) (EnqueueOutcome, string) {
	if p.Draining() {
		return EnqueueDraining, "服务正在停机"
	}

	created, err := p.store.CreateExecutionIfAbsent(rec)
	if err != nil {
		slog.Error("创建执行记录失败", "project", proj.Name, "err", err)
		return EnqueueSkipped, "创建执行记录失败"
	}
	if !created {
		// (delivery_id, project_id) 唯一约束挡住了重复投递
		return EnqueueDuplicate, "该投递已处理过"
	}

	if err := p.Submit(&Task{Execution: rec, Project: proj, Payload: payload}); err != nil {
		// 没排上也要留个记录，否则运维在页面上看不到"这次 webhook 来了但没执行"
		if err := p.store.MarkExecutionSkipped(rec.ID, err.Error()); err != nil {
			slog.Error("标记跳过状态失败", "exec_id", rec.ID, "err", err)
		}
		slog.Warn("任务未能排队", "project", proj.Name, "exec_id", rec.ID, "reason", err)
		return EnqueueSkipped, err.Error()
	}

	return EnqueueQueued, ""
}
