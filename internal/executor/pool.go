// Package executor 负责异步执行脚本。
//
// 并发模型是「同项目 FIFO 串行，跨项目并行」：
//
//   - 每个项目有一条独立的 lane（自己的队列 + 常驻 goroutine），
//     队列里的任务按投递顺序依次执行。这一点很关键 —— 两次 push 连续到来时，
//     部署脚本必须按顺序跑，不能在服务器上互相打架。
//   - 不同项目的 lane 彼此独立，并行执行，总量由全局信号量兜住，
//     免得二十个项目同时触发把机器打满。
package executor

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/zxc7563598/hook-exec/internal/model"
	"github.com/zxc7563598/hook-exec/internal/shelldir"
	"github.com/zxc7563598/hook-exec/internal/store"
)

// laneQueueSize 是单个项目的排队上限。超过就判为 skipped，
// webhook 依然立刻返回 200 —— 绝不让 GitHub 等我们。
const laneQueueSize = 64

// ErrDraining 表示服务正在停机，不再接收新任务。
var ErrDraining = errors.New("服务正在停机")

// enqueueResult 说明任务为什么没排上。
type enqueueResult int

const (
	enqueueOK enqueueResult = iota
	enqueueQueueFull
	enqueueStopped
)

// Pool 管理所有项目的执行。
type Pool struct {
	store    *store.Store
	shellDir *shelldir.Dir

	sem chan struct{} // 全局并发闸

	lanesMu sync.Mutex
	lanes   map[uint]*lane

	// stateMu 同时保护 draining 和在途计数。
	//
	// 两者必须用同一把锁：Submit 要"检查未停机"和"计数 +1"原子完成，
	// 否则会和 Shutdown 产生竞态。也因为如此，这里用 Mutex+Cond 而不是
	// WaitGroup —— WaitGroup 的 Add 与 Wait 并发调用会 panic。
	stateMu   sync.Mutex
	inflight  int
	inflightC *sync.Cond
	draining  bool

	wg     sync.WaitGroup // 所有 lane 的 worker goroutine
	ctx    context.Context
	cancel context.CancelFunc
}

// New 创建执行池。concurrency 是全局并行上限。
func New(st *store.Store, sd *shelldir.Dir, concurrency int) *Pool {
	if concurrency < 1 {
		concurrency = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		store:    st,
		shellDir: sd,
		sem:      make(chan struct{}, concurrency),
		lanes:    make(map[uint]*lane),
		ctx:      ctx,
		cancel:   cancel,
	}
	p.inflightC = sync.NewCond(&p.stateMu)
	return p
}

// Draining 供 HTTP 层判断要不要回 503。
func (p *Pool) Draining() bool {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.draining
}

// Submit 把任务投递给对应项目的 lane。本方法不做任何阻塞等待。
func (p *Pool) Submit(t *Task) error {
	p.stateMu.Lock()
	if p.draining {
		p.stateMu.Unlock()
		return ErrDraining
	}
	// 计数、以及 lane 的创建（内含 wg.Add），都必须在这段临界区里完成。
	// 先计数再入队，反过来的话任务可能在计数之前就跑完了。
	//
	// 把 laneFor 也放进来是为了堵住停机时的一个竞态：如果 wg.Add 发生在
	// 临界区之外，Shutdown 在 waitInflight 超时后调用 wg.Wait() 的同时，
	// 一个刚通过 draining 检查的 Submit 可能才走到 wg.Add —— 这既违反
	// sync.WaitGroup 的约束，新建的 lane 也不在 Shutdown 的快照里，
	// 它的 quit 永远不会被 close，worker 永久阻塞，进程退不掉。
	p.inflight++
	l := p.laneFor(t.Project.ID)
	p.stateMu.Unlock()

	switch res := l.enqueue(t); res {
	case enqueueOK:
		return nil
	case enqueueQueueFull:
		p.doneInflight()
		return errors.New("队列已满，该项目积压了太多待执行任务")
	default: // enqueueStopped
		p.doneInflight()
		return errors.New("项目已被移除")
	}
}

// laneFor 取出项目的 lane，没有就建一个。
//
// 「查找 + 创建」必须在同一把锁内完成，否则两个并发的 webhook 会各自
// 建一条 lane，同一个项目就有了两条 worker —— 直接破坏"同项目串行"这条
// 唯一的保证。
//
// 调用方必须已持有 stateMu（见 Submit 里的说明）。
func (p *Pool) laneFor(projectID uint) *lane {
	p.lanesMu.Lock()
	defer p.lanesMu.Unlock()

	if l, ok := p.lanes[projectID]; ok {
		return l
	}
	l := &lane{
		projectID: projectID,
		queue:     make(chan *Task, laneQueueSize),
		quit:      make(chan struct{}),
	}
	p.lanes[projectID] = l
	p.wg.Add(1)
	go l.run(p)
	return l
}

// RemoveLane 停掉某个项目的 lane（项目被删除时调用）。
//
// worker 会把正在跑的任务跑完、把队列里剩下的排空，然后自己退出。
// 已经入队的任务不会丢，它们仍然会被执行并落库。
//
// 刻意**不从 map 里删掉**这条 lane，只是把它标记为已停止。如果删掉，
// 一个已经进入 dispatch、手里握着 old candidates 的 webhook 会在
// laneFor 里查不到、于是**再建一条** —— 同一个项目就有了两条 worker，
// 新任务会和正在跑的长任务并发，正是"同项目 FIFO"要防的情况。
// 留着这条 stopped 的 lane，后续投递会直接返回 enqueueStopped。
// 项目 ID 是自增的，不存在复用，所以不会出现"需要重新启用同一条 lane"。
func (p *Pool) RemoveLane(projectID uint) {
	p.lanesMu.Lock()
	l, ok := p.lanes[projectID]
	p.lanesMu.Unlock()

	if ok {
		l.stop()
	}
}

// Shutdown 优雅停机：先停止收任务，等宽限期内跑完，超时则强杀。
//
// 调用方应当先关掉 HTTP 服务（这样不会再有新 webhook 进来），再调用本方法。
func (p *Pool) Shutdown(grace time.Duration) {
	p.stateMu.Lock()
	p.draining = true
	p.stateMu.Unlock()

	if p.waitInflight(grace) {
		slog.Info("在途脚本已全部结束")
	} else {
		slog.Warn("等待在途脚本超时，开始强制终止", "grace", grace)
		// cancel 会让所有正在跑的 cmd 走自定义 Cancel，把整个进程组收掉
		p.cancel()
		p.waitInflight(5 * time.Second)
	}

	// 停掉所有 lane。即便还有任务残留在队列里，也会被 worker 排空；
	// 因为 ctx 已取消，它们会被直接标记为 interrupted，不会真的执行。
	//
	// 这份快照是完整的：draining 已在 stateMu 内置位，而 lane 的创建只在
	// Submit 持有 stateMu 时发生（见那里的说明），所以此刻不会再有新 lane
	// 冒出来，也不会有新的 wg.Add 与本函数末尾的 wg.Wait() 相撞。
	p.lanesMu.Lock()
	lanes := make([]*lane, 0, len(p.lanes))
	for _, l := range p.lanes {
		lanes = append(lanes, l)
	}
	p.lanes = make(map[uint]*lane)
	p.lanesMu.Unlock()

	for _, l := range lanes {
		l.stop()
	}

	p.wg.Wait()
	p.cancel()
}

func (p *Pool) waitInflight(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		p.stateMu.Lock()
		for p.inflight > 0 {
			p.inflightC.Wait()
		}
		p.stateMu.Unlock()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (p *Pool) doneInflight() {
	p.stateMu.Lock()
	p.inflight--
	p.inflightC.Broadcast()
	p.stateMu.Unlock()
}

// lane 是单个项目的执行通道。
type lane struct {
	projectID uint
	queue     chan *Task

	// quit 只被 close，从不被 send，所以不存在 send on closed channel。
	// 队列 channel 则永不 close —— 旧项目正是在停机时 close 了仍可能被
	// 写入的队列，导致 send on closed channel panic。
	quit chan struct{}

	mu      sync.Mutex
	stopped bool
}

func (l *lane) run(p *Pool) {
	defer p.wg.Done()

	for {
		select {
		case t := <-l.queue:
			p.execute(t)
		case <-l.quit:
			// 队列里还剩的都跑掉再退，不丢任务
			for {
				select {
				case t := <-l.queue:
					p.execute(t)
				default:
					return
				}
			}
		}
	}
}

func (l *lane) enqueue(t *Task) enqueueResult {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.stopped {
		return enqueueStopped
	}
	select {
	case l.queue <- t:
		return enqueueOK
	default:
		return enqueueQueueFull
	}
}

func (l *lane) stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped {
		return
	}
	l.stopped = true
	close(l.quit)
}

// execute 取全局并发名额后真正执行任务。
func (p *Pool) execute(t *Task) {
	defer p.doneInflight()

	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-p.ctx.Done():
		// 停机中被排空的任务：直接落库说明未执行，不要真的跑起来
		p.finish(t.Execution, model.StatusInterrupted, nil, "服务停机，任务未执行", "", false, 0)
		return
	}

	p.runTask(p.ctx, t)
}
