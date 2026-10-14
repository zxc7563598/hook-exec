//go:build !unix

package shelldir

import "errors"

// 非 Unix 平台没有 FIFO，测试里会因此跳过对应用例。
func mkfifo(string, uint32) error { return errors.New("unsupported") }
