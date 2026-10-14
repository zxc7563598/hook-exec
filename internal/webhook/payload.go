package webhook

import (
	"encoding/json"
	"strings"
)

// Payload 只解析我们需要的字段。
//
// 刻意不建完整的 GitHub payload 结构：字段太多，跟着上游演进维护不起，
// 而且脚本真正需要深层字段时可以直接读 HOOK_PAYLOAD_FILE / stdin，
// 比我们中间转一手更靠谱。
type Payload struct {
	Ref    string `json:"ref"`
	Before string `json:"before"`
	After  string `json:"after"`

	Repository struct {
		FullName string `json:"full_name"`
		Name     string `json:"name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`

	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`

	PullRequest *struct {
		Number int `json:"number"`
		Head   struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`

	Release *struct {
		TagName string `json:"tag_name"`
	} `json:"release"`

	MasterBranch string `json:"master_branch"`
}

// EventContext 是从 payload 里提炼出来的执行上下文。
type EventContext struct {
	Repo   string
	Ref    string
	Branch string
	SHA    string
	Actor  string
}

// FullRepoName 返回 owner/repo，取不到时退回 repository.name。
// 这是匹配项目的唯一依据。
func (p *Payload) FullRepoName() string {
	if p.Repository.FullName != "" {
		return p.Repository.FullName
	}
	if p.Repository.Name != "" && p.Repository.Owner.Login != "" {
		return p.Repository.Owner.Login + "/" + p.Repository.Name
	}
	return ""
}

// ExtractContext 按事件类型分别提取信息。
//
// 这里刻意**不要求** payload 带顶层 ref。旧项目强制要求 ref 非空，
// 而 pull_request、release 这类 payload 根本没有顶层 ref，
// 结果就是这些事件永远返回 400、配了也从来不触发。
// 取不到的字段一律降级成空字符串，绝不让它导致整个请求失败。
func ExtractContext(event string, p *Payload) EventContext {
	ctx := EventContext{
		Repo:  p.FullRepoName(),
		Actor: p.Sender.Login,
	}

	switch event {
	case "push":
		ctx.Ref = p.Ref
		ctx.Branch = trimRefPrefix(p.Ref)
		ctx.SHA = p.After

	case "pull_request", "pull_request_review", "pull_request_review_comment":
		if p.PullRequest != nil {
			ctx.Branch = p.PullRequest.Head.Ref
			ctx.SHA = p.PullRequest.Head.SHA
			if ctx.Branch != "" {
				ctx.Ref = "refs/heads/" + ctx.Branch
			}
		}

	case "release":
		if p.Release != nil {
			ctx.Branch = p.Release.TagName
			if ctx.Branch != "" {
				ctx.Ref = "refs/tags/" + ctx.Branch
			}
		}

	case "create", "delete":
		// 这两个事件的 ref 只是名字（如 v1.0），没有 refs/heads/ 前缀
		ctx.Branch = p.Ref
		ctx.Ref = p.Ref

	default:
		// 兜底：能拿到 ref 就用，拿不到就空着
		ctx.Ref = p.Ref
		ctx.Branch = trimRefPrefix(p.Ref)
		ctx.SHA = p.After
	}

	return ctx
}

func trimRefPrefix(ref string) string {
	for _, prefix := range []string{"refs/heads/", "refs/tags/", "refs/"} {
		if after, ok := strings.CutPrefix(ref, prefix); ok {
			return after
		}
	}
	return ref
}

// parsePayload 解析原始 body。解析失败返回 nil，由调用方决定怎么处理。
func parsePayload(body []byte) *Payload {
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	return &p
}
