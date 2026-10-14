package model

import (
	"sort"
	"strings"
)

// 执行状态。
const (
	StatusPending     = "pending"     // 已入库，等待执行
	StatusRunning     = "running"     // 正在执行
	StatusSuccess     = "success"     // 退出码 0
	StatusFailed      = "failed"      // 脚本非 0 退出，或根本没跑起来
	StatusTimeout     = "timeout"     // 超过配置的超时时间，进程组已被杀
	StatusInterrupted = "interrupted" // 服务停机或崩溃打断
	StatusSkipped     = "skipped"     // 确认要执行但没排上（队列满）
)

// 触发来源。
const (
	TriggerWebhook = "webhook"
	TriggerManual  = "manual"
)

// AllStatuses 供 API 校验筛选参数用。
var AllStatuses = []string{
	StatusPending, StatusRunning, StatusSuccess,
	StatusFailed, StatusTimeout, StatusInterrupted, StatusSkipped,
}

// IsTerminalStatus 判断状态是否已经不会再变化。管理页据此决定还要不要轮询。
func IsTerminalStatus(s string) bool {
	switch s {
	case StatusSuccess, StatusFailed, StatusTimeout, StatusInterrupted, StatusSkipped:
		return true
	}
	return false
}

// AllEventsWildcard 表示匹配全部事件。
const AllEventsWildcard = "*"

// CommonEvents 是管理页事件选择器里预置的候选项。
// 用户可以自行添加任意事件名，这里只是常见的那些。
var CommonEvents = []string{
	"push",
	"pull_request",
	"release",
	"issues",
	"issue_comment",
	"create",
	"delete",
	"workflow_dispatch",
	"workflow_run",
	"fork",
	"star",
	"repository",
}

// EncodeEvents 把事件名列表整理成存储用的逗号分隔串。
//
// 去重、去空白、排序，保证「同一组事件」只有一种存储形态 —— 否则
// "push,release" 和 "release,push" 会被当成两种配置，导入导出时看着很乱。
// 含通配符时直接归一成单个 "*"，因为其余项已经没有意义了。
func EncodeEvents(events []string) string {
	seen := map[string]bool{}
	var out []string
	for _, e := range events {
		e = strings.TrimSpace(e)
		if e == "" || seen[e] {
			continue
		}
		if e == AllEventsWildcard {
			return AllEventsWildcard
		}
		seen[e] = true
		out = append(out, e)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// DecodeEvents 把存储串拆回列表。空串返回空切片而非 nil，方便直接序列化成 []。
func DecodeEvents(events string) []string {
	events = strings.TrimSpace(events)
	if events == "" {
		return []string{}
	}
	parts := strings.Split(events, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// MatchesEvent 判断配置的事件列表是否匹配到来的事件。
//
// 配置为空视为通配 —— 「没填」和「填了 *」在行为上应当一致，
// 否则漏填事件的项目会静默地永不触发。
func MatchesEvent(configured, event string) bool {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return true
	}
	for _, e := range DecodeEvents(configured) {
		if e == AllEventsWildcard || e == event {
			return true
		}
	}
	return false
}
