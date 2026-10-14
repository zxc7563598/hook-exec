package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zxc7563598/hook-exec/internal/executor"
	"github.com/zxc7563598/hook-exec/internal/model"
	"github.com/zxc7563598/hook-exec/internal/shelldir"
	"github.com/zxc7563598/hook-exec/internal/store"
)

func init() { gin.SetMode(gin.TestMode) }

// harness 搭一套最小的运行环境：临时库 + 临时脚本目录 + 执行池 + 路由。
type harness struct {
	router *gin.Engine
	store  *store.Store
	pool   *executor.Pool
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), false)
	if err != nil {
		t.Fatalf("建库失败：%v", err)
	}

	shellRoot := filepath.Join(t.TempDir(), "shell")
	if err := os.MkdirAll(shellRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(shellRoot, "noop.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sd, err := shelldir.New(shellRoot)
	if err != nil {
		t.Fatalf("初始化脚本目录失败：%v", err)
	}

	pool := executor.New(st, sd, 2)

	router := gin.New()
	router.POST("/", New(st, pool).Receive)

	t.Cleanup(func() {
		pool.Shutdown(2 * time.Second)
		st.Close()
	})

	return &harness{router: router, store: st, pool: pool}
}

func (h *harness) addProject(t *testing.T, name, repo, secret, events string, enabled bool) *model.Project {
	t.Helper()
	p := &model.Project{
		Name: name, Repo: repo, Secret: secret, Events: events,
		ShellFile: "noop.sh", TimeoutSec: 5, Enabled: enabled,
	}
	if err := h.store.CreateProject(p); err != nil {
		t.Fatalf("建项目失败：%v", err)
	}
	return p
}

// post 发一个请求并把响应解析成一个 map，方便断言。
func (h *harness) post(t *testing.T, body string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)

	var parsed map[string]any
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &parsed)
	}
	return w.Code, parsed
}

func signBody(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

const pushBody = `{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"me/demo"},"sender":{"login":"lisiqi"}}`

func TestValidWebhookQueuesExecution(t *testing.T) {
	h := newHarness(t)
	h.addProject(t, "演示", "me/demo", "s3cret", "push", true)

	code, resp := h.post(t, pushBody, map[string]string{
		"X-GitHub-Event":      "push",
		"X-GitHub-Delivery":   "d-1",
		"X-Hub-Signature-256": signBody("s3cret", pushBody),
		"Content-Type":        "application/json",
	})

	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d（%v）", code, resp)
	}
	if resp["queued"] != float64(1) {
		t.Fatalf("期望 queued=1，得到 %v", resp)
	}
}

// 没配过的仓库必须返回和"验签失败"完全一样的响应。
//
// 否则一个未认证的请求只要发个 payload 就能靠状态码区分"这个仓库配了没有"，
// 把服务器上配置了哪些仓库枚举出来。
func TestUnmatchedRepoIsIndistinguishableFromBadSignature(t *testing.T) {
	h := newHarness(t)
	h.addProject(t, "演示", "me/demo", "s3cret", "push", true)

	// ① 仓库配过，但签名是错的
	code1, resp1 := h.post(t, pushBody, map[string]string{
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": signBody("wrong-secret", pushBody),
	})

	// ② 仓库压根没配过
	unmatched := `{"repository":{"full_name":"nobody/knows"}}`
	code2, resp2 := h.post(t, unmatched, map[string]string{
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": signBody("anything", unmatched),
	})

	if code1 != http.StatusUnauthorized {
		t.Fatalf("签名错误期望 401，得到 %d", code1)
	}
	if code2 != http.StatusUnauthorized {
		t.Fatalf("未配置的仓库也应当回 401（否则可被枚举），得到 %d", code2)
	}
	if resp1["message"] != resp2["message"] {
		t.Fatalf("两种情况的响应体必须一致，实际 %v vs %v", resp1["message"], resp2["message"])
	}
}

// 停用的项目也不能泄露"这个仓库配过"。
func TestDisabledProjectDoesNotLeakByStatus(t *testing.T) {
	h := newHarness(t)
	h.addProject(t, "停用的", "me/demo", "s3cret", "push", false)

	code, _ := h.post(t, pushBody, map[string]string{
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": signBody("s3cret", pushBody),
	})
	if code != http.StatusUnauthorized {
		t.Fatalf("停用项目应当与未配置表现一致（401），得到 %d", code)
	}
}

// 缺少 X-GitHub-Delivery 时不能把 delivery_id 写成空串 —— 那样第二个
// webhook 会撞唯一约束被静默判成重复投递，脚本根本不执行。
func TestMissingDeliveryIDDoesNotSuppressNextExecution(t *testing.T) {
	h := newHarness(t)
	h.addProject(t, "演示", "me/demo", "s3cret", "push", true)

	headers := map[string]string{
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": signBody("s3cret", pushBody),
		// 刻意不带 X-GitHub-Delivery
	}

	_, resp1 := h.post(t, pushBody, headers)
	_, resp2 := h.post(t, pushBody, headers)

	if resp1["queued"] != float64(1) {
		t.Fatalf("第一次应当排队，得到 %v", resp1)
	}
	if resp2["queued"] != float64(1) {
		t.Fatalf("第二次也应当排队（不能因为 delivery 为空被判重复），得到 %v", resp2)
	}
}

// GitHub 的 redeliver 会重复投递同一个 delivery，必须去重。
func TestRedeliveryIsDeduplicated(t *testing.T) {
	h := newHarness(t)
	h.addProject(t, "演示", "me/demo", "s3cret", "push", true)

	headers := map[string]string{
		"X-GitHub-Event":      "push",
		"X-GitHub-Delivery":   "same-delivery",
		"X-Hub-Signature-256": signBody("s3cret", pushBody),
	}

	_, resp1 := h.post(t, pushBody, headers)
	_, resp2 := h.post(t, pushBody, headers)

	if resp1["queued"] != float64(1) {
		t.Fatalf("第一次应当排队，得到 %v", resp1)
	}
	if resp2["duplicated"] != float64(1) {
		t.Fatalf("第二次应当被判为重复，得到 %v", resp2)
	}
}

// ping 是 GitHub 配置 webhook 后的连通性检查，必须 200 且不做任何事。
func TestPingIsAcknowledgedWithoutSideEffects(t *testing.T) {
	h := newHarness(t)
	body := `{"zen":"Keep it logically awesome."}`

	code, resp := h.post(t, body, map[string]string{"X-GitHub-Event": "ping"})
	if code != http.StatusOK {
		t.Fatalf("ping 期望 200，得到 %d", code)
	}
	if resp["message"] != "pong" {
		t.Fatalf("期望 pong，得到 %v", resp["message"])
	}
}

// 非 push 事件（payload 没有顶层 ref）必须能正常触发。
// 旧项目强制要求 ref 非空，导致 pull_request / release 永远返回 400。
func TestNonPushEventWithoutTopLevelRef(t *testing.T) {
	h := newHarness(t)
	h.addProject(t, "演示", "me/demo", "s3cret", "pull_request", true)

	body := `{"action":"opened","pull_request":{"number":7,` +
		`"head":{"ref":"feature","sha":"deadbeef"}},` +
		`"repository":{"full_name":"me/demo"},"sender":{"login":"lisiqi"}}`

	code, resp := h.post(t, body, map[string]string{
		"X-GitHub-Event":      "pull_request",
		"X-GitHub-Delivery":   "pr-1",
		"X-Hub-Signature-256": signBody("s3cret", body),
	})

	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", code)
	}
	if resp["queued"] != float64(1) {
		t.Fatalf("pull_request 应当触发执行，得到 %v", resp)
	}
}

// 事件不匹配时既不执行、也不落库，避免给无关事件刷一堆记录。
func TestUnmatchedEventCreatesNoRecord(t *testing.T) {
	h := newHarness(t)
	h.addProject(t, "演示", "me/demo", "s3cret", "push", true)

	body := `{"repository":{"full_name":"me/demo"},"sender":{"login":"x"}}`
	code, resp := h.post(t, body, map[string]string{
		"X-GitHub-Event":      "issues",
		"X-GitHub-Delivery":   "i-1",
		"X-Hub-Signature-256": signBody("s3cret", body),
	})

	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", code)
	}
	if resp["matched"] != float64(0) {
		t.Fatalf("事件不匹配不应计入 matched，得到 %v", resp)
	}

	_, total, err := h.store.ListExecutions(store.ExecutionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("事件不匹配不应产生记录，实际有 %d 条", total)
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	h := newHarness(t)

	huge := `{"repository":{"full_name":"me/demo"},"padding":"` +
		strings.Repeat("x", maxBodyBytes+1024) + `"}`

	code, _ := h.post(t, huge, map[string]string{
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": signBody("s3cret", huge),
	})
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大 body 期望 413，得到 %d", code)
	}
}

func TestGeneratedDeliveryIDIsUniqueAndNonEmpty(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := generatedDeliveryID()
		if id == "" {
			t.Fatal("合成的 delivery id 不能为空")
		}
		if seen[id] {
			t.Fatalf("合成的 delivery id 重复了：%s", id)
		}
		seen[id] = true

		// 必须带前缀，方便在记录列表里一眼看出这是合成的
		if !strings.HasPrefix(id, "no-delivery-") {
			t.Fatalf("合成 id 应当带可识别的前缀，得到 %s", id)
		}
	}
}
