// Package webhook 处理 GitHub 推送过来的 webhook。
package webhook

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zxc7563598/hook-exec/internal/executor"
	"github.com/zxc7563598/hook-exec/internal/model"
	"github.com/zxc7563598/hook-exec/internal/store"
)

// maxBodyBytes 是 webhook body 的上限。
//
// GitHub 自己的上限是 25MB，但那是极端情况；普通 push 只有几 KB。
// 设 5MB 足够应付带大量 commits 的推送，同时挡住"拿超大 body 打爆内存"
// 这种最省事的 DoS。旧项目用 GetRawData 读 body 完全没有上限。
const maxBodyBytes = 5 << 20

// Handler 是 POST / 的处理者。
type Handler struct {
	store *store.Store
	pool  *executor.Pool
}

func New(st *store.Store, pool *executor.Pool) *Handler {
	return &Handler{store: st, pool: pool}
}

// receiveResponse 是回给 GitHub 的响应体。GitHub 不看它，
// 但运维可以在 delivery 详情里看到，所以字段要能说明"为什么没执行"。
type receiveResponse struct {
	Matched    int    `json:"matched"`
	Queued     int    `json:"queued"`
	Skipped    int    `json:"skipped"`
	Duplicated int    `json:"duplicated"`
	Disabled   int64  `json:"disabled,omitempty"`
	Message    string `json:"message,omitempty"`
}

// Receive 处理 POST /。
//
// 整体原则：除了"签名不对"之外，其余情况都尽量回 200。
// GitHub 只会在非 2xx 时标红并重投，而我们希望的是"没事发生"就是正常，
// 不该因为它推了个没配过的仓库就一直重试。
func (h *Handler) Receive(c *gin.Context) {
	if h.pool.Draining() {
		// 明确告诉 GitHub 稍后再来，而不是收下却丢掉
		c.JSON(http.StatusServiceUnavailable, gin.H{"message": "服务正在停机，请稍后重投"})
		return
	}

	// MaxBytesReader 必须在读取之前包上，否则限制起不到作用。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"message": "payload 过大"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"message": "读取请求体失败"})
		return
	}

	event := c.GetHeader("X-GitHub-Event")
	deliveryID := c.GetHeader("X-GitHub-Delivery")
	signature := c.GetHeader("X-Hub-Signature-256")

	if event == "" {
		c.JSON(http.StatusBadRequest, gin.H{"message": "缺少 X-GitHub-Event 头"})
		return
	}

	// GitHub 配置好 webhook 后会立刻发一个 ping 做连通性检查。
	// 回 200 且不做任何事就行 —— 它没有 repository 之外有用的东西。
	if event == "ping" {
		c.JSON(http.StatusOK, receiveResponse{Message: "pong"})
		return
	}

	payload := parsePayload(body)
	if payload == nil {
		c.JSON(http.StatusBadRequest, gin.H{"message": "payload 不是合法 JSON"})
		return
	}

	repo := payload.FullRepoName()
	if repo == "" {
		c.JSON(http.StatusBadRequest, gin.H{"message": "payload 里缺少 repository.full_name"})
		return
	}

	// delivery id 是幂等键的一半。GitHub 一定会带这个头，但中间的反代、
	// SIEM 或者自建客户端可能把它剥掉 —— 那样所有请求的 delivery_id 都是
	// 空串，第二个 webhook 就会撞上 (delivery_id, project_id) 唯一约束，
	// 被静默判成"重复投递"而**根本不执行**。对部署脚本来说这是最糟的
	// 失败方式：什么都没发生，还看不出来。所以缺失时合成一个。
	if deliveryID == "" {
		deliveryID = generatedDeliveryID()
		slog.Warn("请求缺少 X-GitHub-Delivery，已合成一个（本次投递无法去重）",
			"repo", repo, "event", event, "synthesized", deliveryID)
	}

	candidates, err := h.store.ListEnabledProjectsByRepo(repo)
	if err != nil {
		slog.Error("查询项目失败", "repo", repo, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"message": "内部错误"})
		return
	}

	// 仓库没配过（或配了但停用）时，返回与"验签失败"**完全一样**的响应。
	//
	// 不能在这里回 200 + "该仓库没有启用中的项目"：那样一个未认证的请求
	// 就能靠状态码区分某个仓库有没有被配置，等于把服务器上配置了哪些仓库
	// 白白枚举出去。两者响应一致，就什么都问不出来。
	if len(candidates) == 0 {
		disabled, err := h.store.CountDisabledProjectsByRepo(repo)
		if err != nil {
			slog.Error("统计停用项目失败", "repo", repo, "err", err)
		}
		slog.Warn("webhook 未匹配到启用中的项目",
			"repo", repo, "event", event, "delivery", deliveryID, "disabled_projects", disabled)
		c.JSON(http.StatusUnauthorized, receiveResponse{Message: "签名校验失败"})
		return
	}

	resp, code := h.dispatch(candidates, body, signature, event, deliveryID, payload)
	c.JSON(code, resp)
}

// generatedDeliveryID 为缺少 X-GitHub-Delivery 的请求合成一个幂等键。
func generatedDeliveryID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 拿不到随机数也不能让请求失败，退化成时间戳
		return fmt.Sprintf("no-delivery-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("no-delivery-%d-%s", time.Now().UnixNano(), hex.EncodeToString(b[:]))
}

// dispatch 验签、匹配事件、建记录、入队。返回响应体和 HTTP 状态码。
func (h *Handler) dispatch(candidates []model.Project, body []byte, signature, event, deliveryID string, payload *Payload) (receiveResponse, int) {
	// 逐个候选项目验签，并把结果记下来 —— 一个仓库下的多个项目可能
	// 各配了不同的 secret，只有验签通过的才该触发。
	//
	// 注意这里遍历的是全部候选项目，不提前 break：提前返回会让响应耗时
	// 随"匹配到第几个项目"变化，等于用时间侧信道泄露了哪个 secret 是对的。
	v := newVerifier(body, signature)
	verified := make([]bool, len(candidates))
	anyVerified := false
	for i := range candidates {
		if v.ok(candidates[i].Secret) {
			verified[i] = true
			anyVerified = true
		}
	}

	if !anyVerified {
		slog.Warn("webhook 签名校验失败",
			"repo", payload.FullRepoName(),
			"event", event,
			"delivery", deliveryID,
			"candidates", len(candidates),
		)
		return receiveResponse{Message: "签名校验失败"}, http.StatusUnauthorized
	}

	ctx := ExtractContext(event, payload)

	var resp receiveResponse
	for i, proj := range candidates {
		if !verified[i] {
			continue
		}
		// 事件不匹配的项目不落库。否则给一个仓库配了 push，
		// 每次 issue_comment 都会往记录表里塞一行噪音。
		if !model.MatchesEvent(proj.Events, event) {
			continue
		}
		resp.Matched++

		rec := &model.Execution{
			ProjectID:   proj.ID,
			ProjectName: proj.Name,
			Repo:        ctx.Repo,
			Event:       event,
			DeliveryID:  deliveryID,
			Ref:         ctx.Ref,
			Branch:      ctx.Branch,
			SHA:         ctx.SHA,
			Actor:       ctx.Actor,
			Trigger:     model.TriggerWebhook,
			ShellFile:   proj.ShellFile,
			Status:      model.StatusPending,
		}

		switch outcome, reason := h.pool.Enqueue(rec, &candidates[i], body); outcome {
		case executor.EnqueueQueued:
			resp.Queued++
		case executor.EnqueueDuplicate:
			resp.Duplicated++
		case executor.EnqueueSkipped:
			resp.Skipped++
			resp.Message = reason
		case executor.EnqueueDraining:
			resp.Skipped++
			resp.Message = reason
		}
	}

	slog.Info("收到 webhook",
		"repo", ctx.Repo,
		"event", event,
		"delivery", deliveryID,
		"actor", ctx.Actor,
		"branch", ctx.Branch,
		"matched", resp.Matched,
		"queued", resp.Queued,
		"duplicated", resp.Duplicated,
		"skipped", resp.Skipped,
	)

	// 即便一条都没匹配上事件，也回 200 —— 那只是"这个事件我们不关心"。
	return resp, http.StatusOK
}
