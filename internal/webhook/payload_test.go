package webhook

import "testing"

// 旧项目强制要求 payload 带顶层 ref，导致 pull_request、release 这类
// 事件永远返回 400、配了也从来不触发。这组用例就是钉住这一点：
// 各类事件的 payload 都必须能被解析出上下文，且不缺字段时不报错。
func TestExtractContextByEvent(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		event      string
		wantRepo   string
		wantRef    string
		wantBranch string
		wantSHA    string
		wantActor  string
	}{
		{
			name:       "push",
			event:      "push",
			wantRepo:   "me/demo",
			wantRef:    "refs/heads/main",
			wantBranch: "main",
			wantSHA:    "abc123",
			wantActor:  "lisiqi",
			raw: `{"ref":"refs/heads/main","after":"abc123",
			       "repository":{"full_name":"me/demo"},"sender":{"login":"lisiqi"}}`,
		},
		{
			// 这个 payload 没有顶层 ref —— 旧项目在这里恒返回 400
			name:       "pull_request 无顶层 ref",
			event:      "pull_request",
			wantRepo:   "me/demo",
			wantRef:    "refs/heads/feature",
			wantBranch: "feature",
			wantSHA:    "deadbeef",
			wantActor:  "lisiqi",
			raw: `{"action":"opened",
			       "pull_request":{"number":7,"head":{"ref":"feature","sha":"deadbeef"},"base":{"ref":"main"}},
			       "repository":{"full_name":"me/demo"},"sender":{"login":"lisiqi"}}`,
		},
		{
			name:       "release",
			event:      "release",
			wantRepo:   "me/demo",
			wantRef:    "refs/tags/v1.2.3",
			wantBranch: "v1.2.3",
			wantActor:  "lisiqi",
			raw: `{"action":"published","release":{"tag_name":"v1.2.3"},
			       "repository":{"full_name":"me/demo"},"sender":{"login":"lisiqi"}}`,
		},
		{
			name:       "create 事件的 ref 没有前缀",
			event:      "create",
			wantRepo:   "me/demo",
			wantRef:    "v2.0.0",
			wantBranch: "v2.0.0",
			wantActor:  "lisiqi",
			raw: `{"ref":"v2.0.0","ref_type":"tag",
			       "repository":{"full_name":"me/demo"},"sender":{"login":"lisiqi"}}`,
		},
		{
			// 完全陌生的事件也不应让请求失败。仓库和触发者不受事件类型影响，
			// 只有 ref/branch/sha 因为没有对应字段而留空。
			name:      "未知事件且没有 ref",
			event:     "deployment_status",
			wantRepo:  "me/demo",
			wantActor: "lisiqi",
			raw:       `{"repository":{"full_name":"me/demo"},"sender":{"login":"lisiqi"}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := parsePayload([]byte(tc.raw))
			if p == nil {
				t.Fatal("payload 应当能被解析")
			}

			got := ExtractContext(tc.event, p)
			if got.Repo != tc.wantRepo {
				t.Errorf("Repo：期望 %q，得到 %q", tc.wantRepo, got.Repo)
			}
			if got.Ref != tc.wantRef {
				t.Errorf("Ref：期望 %q，得到 %q", tc.wantRef, got.Ref)
			}
			if got.Branch != tc.wantBranch {
				t.Errorf("Branch：期望 %q，得到 %q", tc.wantBranch, got.Branch)
			}
			if got.SHA != tc.wantSHA {
				t.Errorf("SHA：期望 %q，得到 %q", tc.wantSHA, got.SHA)
			}
			if got.Actor != tc.wantActor {
				t.Errorf("Actor：期望 %q，得到 %q", tc.wantActor, got.Actor)
			}
		})
	}
}

func TestFullRepoNameFallbacks(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"标准情况", `{"repository":{"full_name":"me/demo"}}`, "me/demo"},
		{"缺 full_name 时拼装", `{"repository":{"name":"demo","owner":{"login":"me"}}}`, "me/demo"},
		{"什么都没有", `{}`, ""},
		{"只有 name", `{"repository":{"name":"demo"}}`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := parsePayload([]byte(tc.raw))
			if p == nil {
				t.Fatal("应当能解析")
			}
			if got := p.FullRepoName(); got != tc.want {
				t.Fatalf("期望 %q，得到 %q", tc.want, got)
			}
		})
	}
}

func TestParsePayloadRejectsInvalidJSON(t *testing.T) {
	for _, raw := range []string{"", "not json", "{", "[]"} {
		// "[]" 是合法 JSON 但不是对象，反序列化到结构体会失败
		if p := parsePayload([]byte(raw)); p != nil && raw == "[]" {
			t.Fatalf("输入 %q 不应被解析成 payload", raw)
		}
	}
	if p := parsePayload([]byte("not json")); p != nil {
		t.Fatal("非法 JSON 应当返回 nil")
	}
}

func TestTrimRefPrefix(t *testing.T) {
	cases := map[string]string{
		"refs/heads/main":   "main",
		"refs/tags/v1.0.0":  "v1.0.0",
		"refs/pull/1/merge": "pull/1/merge",
		"main":              "main",
		"":                  "",
		"refs/heads/feat/x": "feat/x",
	}
	for in, want := range cases {
		if got := trimRefPrefix(in); got != want {
			t.Errorf("trimRefPrefix(%q) = %q，期望 %q", in, got, want)
		}
	}
}
