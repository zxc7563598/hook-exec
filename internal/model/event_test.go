package model

import (
	"reflect"
	"testing"
)

func TestEncodeEventsNormalizes(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"空列表", nil, ""},
		{"单个", []string{"push"}, "push"},
		{"去空白与去重", []string{" push ", "push", "release"}, "push,release"},
		{"排序稳定", []string{"release", "push"}, "push,release"},
		{"忽略空串", []string{"", "push", "  "}, "push"},
		// 通配符把其余项全部吸收，只剩一个 "*"
		{"通配符归一", []string{"push", "*", "release"}, "*"},
		{"只有通配符", []string{"*"}, "*"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EncodeEvents(tc.in); got != tc.want {
				t.Fatalf("期望 %q，得到 %q", tc.want, got)
			}
		})
	}
}

// 同一组事件必须只有一种存储形态，否则 "push,release" 和 "release,push"
// 会被当成两种配置，导出对比时看着像有差异。
func TestEncodeEventsIsStable(t *testing.T) {
	a := EncodeEvents([]string{"release", "push", "issues"})
	b := EncodeEvents([]string{"issues", "push", "release"})
	if a != b {
		t.Fatalf("同一组事件编码结果不一致：%q vs %q", a, b)
	}
}

func TestDecodeEventsReturnsEmptySliceNotNil(t *testing.T) {
	// 返回 nil 会让 JSON 序列化成 null，前端 v-for 就炸了
	for _, in := range []string{"", "   ", ","} {
		got := DecodeEvents(in)
		if got == nil {
			t.Fatalf("DecodeEvents(%q) 不应返回 nil", in)
		}
		if len(got) != 0 {
			t.Fatalf("DecodeEvents(%q) 期望空切片，得到 %v", in, got)
		}
	}
}

func TestDecodeEventsRoundTrip(t *testing.T) {
	in := []string{"push", "release"}
	got := DecodeEvents(EncodeEvents(in))
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("期望 %v，得到 %v", in, got)
	}
}

func TestMatchesEvent(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		event      string
		want       bool
	}{
		// 「没填」和「填了 *」行为必须一致，否则漏填事件的项目会静默地永不触发
		{"未配置视为全部", "", "push", true},
		{"只有空白视为全部", "   ", "anything", true},
		{"通配符匹配任意事件", "*", "pull_request", true},
		{"通配符在列表中", "push,*", "issues", true},
		{"精确匹配", "push", "push", true},
		{"不匹配", "push", "release", false},
		{"多值中命中", "push,release", "release", true},
		{"多值中未命中", "push,release", "issues", false},
		{"带空格的配置", " push , release ", "release", true},
		{"子串不算匹配", "push", "push_extra", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchesEvent(tc.configured, tc.event); got != tc.want {
				t.Fatalf("MatchesEvent(%q, %q) = %v，期望 %v", tc.configured, tc.event, got, tc.want)
			}
		})
	}
}

func TestIsTerminalStatus(t *testing.T) {
	// 这几个状态下页面不该继续轮询
	for _, s := range []string{StatusSuccess, StatusFailed, StatusTimeout, StatusInterrupted, StatusSkipped} {
		if !IsTerminalStatus(s) {
			t.Errorf("%s 应当被判定为终态", s)
		}
	}
	for _, s := range []string{StatusPending, StatusRunning} {
		if IsTerminalStatus(s) {
			t.Errorf("%s 不应被判定为终态", s)
		}
	}
}
