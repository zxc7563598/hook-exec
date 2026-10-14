package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyHMACAcceptsCorrectSignature(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	if !verifyHMAC("s3cret", body, sign("s3cret", body)) {
		t.Fatal("正确签名应当通过")
	}
}

func TestVerifyHMACRejects(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	good := sign("s3cret", body)

	cases := []struct {
		name   string
		secret string
		header string
	}{
		{"密钥不对", "wrong", good},
		{"空密钥", "", good},
		{"空签名头", "s3cret", ""},
		{"缺少 sha256= 前缀", "s3cret", strings.TrimPrefix(good, "sha256=")},
		{"前缀写错", "s3cret", "sha1=" + strings.TrimPrefix(good, "sha256=")},
		{"非法 hex", "s3cret", "sha256=zzzznothex"},
		{"hex 长度不对", "s3cret", "sha256=abcdef"},
		{"空 hex", "s3cret", "sha256="},
		{"body 被改过", "s3cret", sign("s3cret", []byte(`{"hello":"tampered"}`))},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if verifyHMAC(tc.secret, body, tc.header) {
				t.Fatal("应当拒绝")
			}
		})
	}
}

// 同一个 secret 只应计算一次 HMAC（同仓库的多个项目常常共用 secret）
func TestVerifierCachesBySecret(t *testing.T) {
	body := []byte(`{"a":1}`)
	v := newVerifier(body, sign("shared", body))

	if !v.ok("shared") {
		t.Fatal("shared 应当通过")
	}
	first := v.cache["shared"]
	if !first {
		t.Fatal("结果应当被缓存")
	}

	// 再次查询命中缓存，结果一致
	if !v.ok("shared") {
		t.Fatal("重复查询结果应当一致")
	}
	if !v.cache["shared"] {
		t.Fatal("缓存应当保持")
	}

	// 另一个 secret 不被前一个的结果影响
	if v.ok("other") {
		t.Fatal("other 不应通过")
	}
}

// secret 为空的项目永远不能通过认证 —— 否则一个空 secret 就能匹配一切
func TestVerifierRejectsEmptySecret(t *testing.T) {
	body := []byte(`{"a":1}`)
	v := newVerifier(body, sign("", body))
	if v.ok("") {
		t.Fatal("空 secret 不应当通过，否则空 secret 等于匹配一切")
	}
}
