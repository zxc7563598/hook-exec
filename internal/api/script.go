package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// ListScripts GET /web/api/scripts
//
// 列出脚本目录里可选的脚本。表单里的下拉框就是这个接口喂的，
// 所以创建项目时选出来的值天然是合法的。
func (s *Server) ListScripts(c *gin.Context) {
	scripts, err := s.shellDir.List()
	if err != nil {
		fail(c, http.StatusInternalServerError, "读取脚本目录失败")
		return
	}
	ok(c, gin.H{
		"shell_dir": s.shellRoot,
		"scripts":   scripts,
	})
}

// GetScriptContent GET /web/api/scripts/content?name=deploy.sh
//
// 只读预览脚本正文。走的是和 Resolve 完全相同的路径校验，
// 所以读不出脚本目录之外的东西。
func (s *Server) GetScriptContent(c *gin.Context) {
	name := c.Query("name")
	if name == "" {
		invalid(c, "参数校验未通过", []FieldError{{Field: "name", Message: "缺少脚本文件名"}})
		return
	}

	content, truncated, info, err := s.shellDir.Read(name)
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}

	ok(c, gin.H{
		"name":      info.Name,
		"size":      info.Size,
		"mode":      info.Mode,
		"mod_time":  info.ModTime,
		"content":   content,
		"truncated": truncated,
	})
}
