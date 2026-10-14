package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/zxc7563598/hook-exec/internal/model"
)

// killGrace 是发出 SIGTERM 后等脚本自己收尾的时间，超过就 SIGKILL。
// 给脚本一个 trap TERM 的机会，比上来就 KILL 友好。
const killGrace = 3 * time.Second

// waitDelay 限制 ctx 结束后等待 I/O 收尾的时间。脚本如果把 stdout 管道
// 传给了没退出的孙进程，没有这个上限会一直挂住。
const waitDelay = 5 * time.Second

// Task 是一次待执行的作业。
type Task struct {
	Execution *model.Execution
	Project   *model.Project
	Payload   []byte // GitHub 原始 payload
}

// runTask 执行一个任务并落库结果。调用方保证同一个项目不会并发进入这里。
func (p *Pool) runTask(baseCtx context.Context, t *Task) {
	timeout := time.Duration(t.Project.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}

	startedAt := time.Now()
	if err := p.store.MarkExecutionRunning(t.Execution.ID, startedAt); err != nil {
		slog.Error("标记执行为运行中失败", "exec_id", t.Execution.ID, "err", err)
	}

	rec := t.Execution

	// 执行前重新解析一次脚本路径。
	//
	// 刻意不相信数据库里的值：即使有人直接改库、或者导入了一份恶意配置，
	// 也逃不出脚本目录。这里是"只能执行 shell 目录内文件"这条保证的落点。
	scriptPath, err := p.shellDir.Resolve(t.Project.ShellFile)
	if err != nil {
		p.finish(rec, model.StatusFailed, nil, fmt.Sprintf("脚本文件不可用：%v", err), "", false, time.Since(startedAt))
		return
	}

	// 采用直接 exec，依赖 shebang，因此需要可执行位。
	// 提前给出可读的提示，而不是让用户去看一个晦涩的 permission denied。
	if ok, err := p.shellDir.IsExecutable(t.Project.ShellFile); err != nil {
		p.finish(rec, model.StatusFailed, nil, fmt.Sprintf("无法检查脚本权限：%v", err), "", false, time.Since(startedAt))
		return
	} else if !ok {
		p.finish(rec, model.StatusFailed, nil,
			fmt.Sprintf("脚本 %s 没有可执行位，请在服务器上执行 chmod +x", t.Project.ShellFile),
			"", false, time.Since(startedAt))
		return
	}

	if t.Project.WorkDir != "" {
		if fi, err := os.Stat(t.Project.WorkDir); err != nil {
			p.finish(rec, model.StatusFailed, nil,
				fmt.Sprintf("工作目录 %s 不可用：%v", t.Project.WorkDir, err), "", false, time.Since(startedAt))
			return
		} else if !fi.IsDir() {
			p.finish(rec, model.StatusFailed, nil,
				fmt.Sprintf("工作目录 %s 不是目录", t.Project.WorkDir), "", false, time.Since(startedAt))
			return
		}
	}

	// payload 同时以文件和 stdin 两种方式提供。脚本想读深层字段
	// （commits[].message、pull_request.title）直接 jq 那个文件即可。
	payloadPath, cleanup, err := writePayloadFile(t.Payload)
	if err != nil {
		p.finish(rec, model.StatusFailed, nil, fmt.Sprintf("写入 payload 临时文件失败：%v", err), "", false, time.Since(startedAt))
		return
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(baseCtx, timeout)
	defer cancel()

	// 命令行里没有任何来自配置或 payload 的字符串 —— 唯一的变量是脚本
	// 路径，而它刚刚经过脚本目录的包含校验。脚本所需的信息全部通过
	// 环境变量和 stdin 传递。
	cmd := exec.CommandContext(ctx, scriptPath)
	cmd.Dir = t.Project.WorkDir
	cmd.Env = append(os.Environ(), hookEnv(rec, payloadPath)...)
	cmd.Stdin = bytes.NewReader(t.Payload)
	cmd.WaitDelay = waitDelay

	tb := NewTailBuffer(DefaultTailSize)
	// 同一个 TailBuffer 指针同时给 stdout 和 stderr，os/exec 会因为
	// 接口相等而复用同一个拷贝 goroutine，输出顺序接近真实执行顺序。
	cmd.Stdout = tb
	cmd.Stderr = tb

	// 子进程退出后关闭，用来取消 terminateGroup 里等待补刀的定时器。
	// 不能用 cmd.ProcessState 判断"是否已退出" —— 那个字段由 Wait 在
	// 另一个 goroutine 里写，裸读是数据竞态；而且从 wait4 返回到内核回收
	// pid 之间有窗口，一旦 pid 被复用，补发的 SIGKILL 会打到无关的进程组上。
	exited := make(chan struct{})

	// 关键：默认的 CommandContext 超时只会杀掉目标进程本身，脚本派生出来的
	// 子进程（npm run build、后台起的服务）会变成孤儿继续跑。必须把整个
	// 进程组一起收掉，否则"超时"只是杀了个壳。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return terminateGroup(cmd.Process.Pid, exited, tb)
	}

	runErr := cmd.Run()
	close(exited)

	exitCode := exitCodeOf(cmd)
	status, errMsg := classify(ctx, runErr, exitCode)

	p.finish(rec, status, exitCode, errMsg, tb.String(), tb.Truncated(), time.Since(startedAt))
}

// terminateGroup 先对进程组发 TERM，宽限期内没退再 KILL。
//
// exited 由调用方在 cmd.Run() 返回后关闭，是本函数判断"进程是否已经结束"
// 的唯一依据。这里刻意不看 cmd.ProcessState：那个字段由 Wait 在别的
// goroutine 里写，无同步地读它是数据竞态；而且从 wait4 返回到内核回收 pid
// 之间存在窗口，pid 一旦被复用，补发的 SIGKILL 就会打到无关的进程组上。
// 用 channel 之后，定时器只会在进程**确实还活着**时才走到 SIGKILL。
func terminateGroup(pid int, exited <-chan struct{}, tb *TailBuffer) error {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		// 进程可能已经自己退了，不算失败
		return nil
	}

	go func() {
		timer := time.NewTimer(killGrace)
		defer timer.Stop()

		select {
		case <-exited:
			// 收到 TERM 后自己收尾了，不需要补刀
			return
		case <-timer.C:
		}

		tb.Write([]byte(fmt.Sprintf("\n[hook-exec] 超时宽限 %s 已过，强制终止进程组 %d\n", killGrace, pid)))
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
			slog.Debug("补发 SIGKILL 失败（进程组可能已退出）", "pgid", pid, "err", err)
		}
	}()
	return nil
}

// classify 把执行结果翻译成状态和可读的错误信息。
//
// 状态以 ctx.Err() 为准，而不是看 cmd.Wait() 返回的错误：我们用了自定义的
// Cancel，Wait 的错误不再保证是 context.DeadlineExceeded。旧项目在这里
// 直接比 == DeadlineExceeded 并且从不设置 timeout 状态，导致超时一律被
// 记成 failed，超时这个信号就丢了。
func classify(ctx context.Context, runErr error, exitCode *int) (status, msg string) {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return model.StatusTimeout, "执行超时，进程组已被终止"
	case errors.Is(ctx.Err(), context.Canceled):
		return model.StatusInterrupted, "服务停机，执行被中断"
	case runErr == nil:
		return model.StatusSuccess, ""
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		if exitCode != nil {
			return model.StatusFailed, fmt.Sprintf("脚本以退出码 %d 结束", *exitCode)
		}
		return model.StatusFailed, "脚本异常结束"
	}
	return model.StatusFailed, fmt.Sprintf("启动脚本失败：%v", runErr)
}

// finish 写入最终状态。落库失败只记日志 —— 这时候任务已经结束了，
// 没有更好的补救办法，但至少要让运维能从日志里看到。
func (p *Pool) finish(rec *model.Execution, status string, exitCode *int, errMsg, output string, truncated bool, dur time.Duration) {
	rec.Status = status
	rec.ExitCode = exitCode
	rec.Error = errMsg
	rec.Output = output
	rec.OutputTruncated = truncated

	if err := p.store.FinishExecution(rec, dur); err != nil {
		slog.Error("写入执行结果失败", "exec_id", rec.ID, "status", status, "err", err)
		return
	}

	attrs := []any{
		"exec_id", rec.ID,
		"project", rec.ProjectName,
		"event", rec.Event,
		"status", status,
		"duration_ms", dur.Milliseconds(),
	}
	if exitCode != nil {
		attrs = append(attrs, "exit_code", *exitCode)
	}
	if status == model.StatusSuccess {
		slog.Info("脚本执行完成", attrs...)
	} else {
		slog.Warn("脚本执行未成功", append(attrs, "err", errMsg)...)
	}
}

func exitCodeOf(cmd *exec.Cmd) *int {
	if cmd.ProcessState == nil {
		return nil
	}
	code := cmd.ProcessState.ExitCode()
	return &code
}

// writePayloadFile 把原始 payload 落成一个 0600 的临时文件。
// CreateTemp 本身就以 0600 创建，这里不再重复 chmod。
func writePayloadFile(payload []byte) (path string, cleanup func(), err error) {
	if len(payload) == 0 {
		return "", func() {}, nil
	}
	f, err := os.CreateTemp("", "hook-exec-payload-*.json")
	if err != nil {
		return "", func() {}, err
	}
	if _, err := f.Write(payload); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", func() {}, err
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// hookEnv 组装注入给脚本的环境变量。
//
// 命名沿用 GitHub Actions 的习惯（GITHUB_REPOSITORY / GITHUB_SHA 等），
// 这样现成的部署脚本改一下就还能用；HOOK_* 是本项目自己的名字。
// 空值一律不注入，让脚本能靠 ${VAR:-default} 区分"没有这个信息"和"信息为空"。
func hookEnv(e *model.Execution, payloadPath string) []string {
	pairs := [][2]string{
		{"HOOK_EXEC_ID", fmt.Sprint(e.ID)},
		{"HOOK_PROJECT", e.ProjectName},
		{"HOOK_REPO", e.Repo},
		{"HOOK_EVENT", e.Event},
		{"HOOK_DELIVERY", e.DeliveryID},
		{"HOOK_REF", e.Ref},
		{"HOOK_BRANCH", e.Branch},
		{"HOOK_SHA", e.SHA},
		{"HOOK_ACTOR", e.Actor},
		{"HOOK_SCRIPT", e.ShellFile},
		{"HOOK_TRIGGER", e.Trigger},
		{"HOOK_PAYLOAD_FILE", payloadPath},

		// GitHub Actions 风格的同义别名
		{"GITHUB_REPOSITORY", e.Repo},
		{"GITHUB_EVENT_NAME", e.Event},
		{"GITHUB_SHA", e.SHA},
		{"GITHUB_REF_NAME", e.Branch},
		{"GITHUB_REF", e.Ref},
		{"GITHUB_ACTOR", e.Actor},
		{"GITHUB_DELIVERY", e.DeliveryID},
	}

	env := make([]string, 0, len(pairs))
	for _, kv := range pairs {
		if strings.TrimSpace(kv[1]) == "" {
			continue
		}
		env = append(env, kv[0]+"="+kv[1])
	}
	return env
}
