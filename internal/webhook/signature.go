package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const signaturePrefix = "sha256="

// verifyHMAC 校验 GitHub 的 X-Hub-Signature-256。
//
// 先把十六进制解成原始 32 字节再比，而不是比对十六进制字符串 ——
// 后者在大小写、前缀、长度上有各种边界情况。非法 hex 直接判失败。
func verifyHMAC(secret string, body []byte, header string) bool {
	if secret == "" || header == "" {
		return false
	}
	if !strings.HasPrefix(header, signaturePrefix) {
		return false
	}
	given, err := hex.DecodeString(strings.TrimPrefix(header, signaturePrefix))
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(given, mac.Sum(nil))
}

// verifier 在同一个仓库下可能有多个项目、各自配了不同的 secret 时，
// 逐个验证并缓存结果。
//
// 两点考虑：
//
//  1. 同一个 secret 只算一次 HMAC（同仓库的多个项目常常共用同一个 secret）。
//  2. 调用方遍历所有候选项目时不要提前 break。若"验到第一个就返回"，
//     响应耗时会随匹配到第几个项目而变化，等于通过时间侧信道泄露了
//     "哪个项目的 secret 是对的"。
type verifier struct {
	body   []byte
	header string
	cache  map[string]bool
}

func newVerifier(body []byte, header string) *verifier {
	return &verifier{body: body, header: header, cache: make(map[string]bool)}
}

// ok 判断某个 secret 是否能通过校验。结果会被缓存。
func (v *verifier) ok(secret string) bool {
	if r, found := v.cache[secret]; found {
		return r
	}
	r := verifyHMAC(secret, v.body, v.header)
	v.cache[secret] = r
	return r
}
