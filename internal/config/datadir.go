package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// 数据目录下的固定布局。
const (
	DBFileName   = "hook-exec.db"
	ShellDirName = "shell"
)

// Dirs 是解析好的运行时目录。
//
// Root 与 ShellDir 都是经 filepath.EvalSymlinks 解析过的真实路径。这一点很关键：
// shell 目录的路径包含校验是靠字符串前缀比较做的，如果路径里还残留软链
// （macOS 的 /tmp -> /private/tmp、systemd 下 /opt 可能是软链），
// 前缀比较会全部失效。
type Dirs struct {
	Root     string // 数据目录
	DBPath   string // SQLite 文件路径
	ShellDir string // 脚本目录
}

// ResolveDataDir 定位数据目录、校验可写性，并确保 shell 子目录存在。
//
// override 为空时使用可执行文件所在目录。之所以支持覆盖：
// systemd 下二进制通常在 /usr/local/bin，而数据应该待在 /opt 之类的地方；
// go run 时可执行文件在临时目录，也必须能改。
func ResolveDataDir(override string) (*Dirs, error) {
	base, err := baseDir(override)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录 %s 失败：%w", base, err)
	}

	root, err := filepath.EvalSymlinks(base)
	if err != nil {
		return nil, fmt.Errorf("解析数据目录 %s 失败：%w", base, err)
	}

	// 早点把"目录不可写"暴露出来。否则会在建库或写记录时才炸，
	// 那时候进程已经起来了一半，排查起来更费劲。
	if err := checkWritable(root); err != nil {
		return nil, fmt.Errorf("数据目录 %s 不可写：%w；请用 -data-dir 指定一个可写目录", root, err)
	}

	shell := filepath.Join(root, ShellDirName)
	if err := os.MkdirAll(shell, 0o755); err != nil {
		return nil, fmt.Errorf("创建脚本目录 %s 失败：%w", shell, err)
	}
	shellReal, err := filepath.EvalSymlinks(shell)
	if err != nil {
		return nil, fmt.Errorf("解析脚本目录 %s 失败：%w", shell, err)
	}

	return &Dirs{
		Root:     root,
		DBPath:   filepath.Join(root, DBFileName),
		ShellDir: shellReal,
	}, nil
}

func baseDir(override string) (string, error) {
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("解析 -data-dir %s 失败：%w", override, err)
		}
		return abs, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("无法定位可执行文件路径：%w；请用 -data-dir 显式指定数据目录", err)
	}
	// os.Executable 可能返回软链，先解掉再取目录，避免数据落到软链指向之外的地方。
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
}

func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".hook-exec-write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}
