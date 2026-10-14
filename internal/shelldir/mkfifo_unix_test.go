//go:build unix

package shelldir

import "syscall"

func mkfifo(path string, mode uint32) error {
	return syscall.Mkfifo(path, mode)
}
