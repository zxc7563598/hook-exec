package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Response 是所有管理接口的统一响应体。
//
// 刻意不设 success 布尔字段：旧项目里 Error() 把 success 恒置为 true，
// 前端要同时判断 code 和 success 才敢用，结果两个都不可靠。
// 这里让 code 与 HTTP 状态码始终一致，判断一处就够了。
type Response struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// FieldError 是字段级校验错误，前端据此把提示挂到对应输入框旁边。
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

type errorData struct {
	Fields []FieldError `json:"fields,omitempty"`
}

func ok(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Response{Code: http.StatusOK, Message: "ok", Data: data})
}

// fail 返回错误。code 与 HTTP 状态码保持一致。
func fail(c *gin.Context, status int, message string) {
	c.JSON(status, Response{Code: status, Message: message})
}

// invalid 返回 400 并携带字段级错误。
func invalid(c *gin.Context, message string, fields []FieldError) {
	c.JSON(http.StatusBadRequest, Response{
		Code:    http.StatusBadRequest,
		Message: message,
		Data:    errorData{Fields: fields},
	})
}
