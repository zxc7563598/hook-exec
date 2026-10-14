package api

import (
	"crypto/subtle"
	"net/http"

	"github.com/gin-gonic/gin"
)

// csrfHeaderValue 是写接口要求携带的自定义头。
//
// 为什么 Basic Auth 之外还要这个：浏览器会把 Basic Auth 凭证缓存下来，
// 之后任何指向本站的请求（包括第三方页面上的一个 form 或 img）都会自动
// 带上 Authorization。也就是说光有密码并不能阻止跨站写操作。
// 而自定义头受同源策略约束，浏览器不会为简单跨站请求自动附加它，
// 攻击者也没法通过 form/img 去设它。
const csrfHeaderValue = "XMLHttpRequest"

// basicAuth 返回 Basic Auth 中间件。
//
// 用户名和密码都比较，且**不做短路**：先比用户名、错了就不比密码的写法，
// 会让"用户名是否存在"通过响应耗时可被区分出来。
func basicAuth(username, password string) gin.HandlerFunc {
	wantUser := []byte(username)
	wantPass := []byte(password)

	return func(c *gin.Context) {
		user, pass, hasAuth := c.Request.BasicAuth()

		userOK := subtle.ConstantTimeCompare([]byte(user), wantUser) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), wantPass) == 1

		if !hasAuth || !userOK || !passOK {
			c.Header("WWW-Authenticate", `Basic realm="hook-exec"`)
			c.AbortWithStatusJSON(http.StatusUnauthorized, Response{
				Code:    http.StatusUnauthorized,
				Message: "需要认证",
			})
			return
		}
		c.Next()
	}
}

// csrfGuard 对非安全方法要求 X-Requested-With 头。
func csrfGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// 安全方法直接放行
		default:
			if c.GetHeader("X-Requested-With") != csrfHeaderValue {
				c.AbortWithStatusJSON(http.StatusForbidden, Response{
					Code:    http.StatusForbidden,
					Message: "缺少 X-Requested-With 头，请求已被拒绝",
				})
				return
			}
		}
		c.Next()
	}
}
