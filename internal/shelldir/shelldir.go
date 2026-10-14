// Package shelldir 是程序里唯一允许接触脚本文件的入口。
//
// 这个项目刻意不在网页上提供"编写命令"的能力：可执行内容必须由运维人员
// 自己上传到 shell 目录，本包只负责从中读取和执行。所有路径判断都收敛在这里，
// 目的是让"能不能执行某个文件"这件事只有一处实现、一处测试。
package shelldir

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxPreviewBytes 限制只读预览的读取量。脚本文件正常都很小，
// 设这个上限只是为了挡住有人往目录里丢一个几 GB 的文件把内存吃光。
const maxPreviewBytes = 512 << 10

var (
	// ErrInvalidName 名称形状不合法（空、绝对路径、"." 或 ".."）。
	ErrInvalidName = errors.New("脚本文件名不合法")
	// ErrNotFound 文件不存在或无法解析。
	ErrNotFound = errors.New("脚本文件不存在")
	// ErrOutsideDir 解析后的真实路径落在脚本目录之外。
	ErrOutsideDir = errors.New("脚本文件超出脚本目录范围")
	// ErrNotRegularFile 不是普通文件（目录、FIFO、设备文件等）。
	ErrNotRegularFile = errors.New("不是普通文件")
)

// Script 是脚本目录里的一个文件。
type Script struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	Mode       string    `json:"mode"`
	Executable bool      `json:"executable"`
	ModTime    time.Time `json:"mod_time"`
}

// Dir 是一个已解析软链的脚本目录。
type Dir struct {
	root string
}

// New 构造一个 Dir。root 必须是已 EvalSymlinks 解析过的绝对路径，
// 否则下面的前缀比较会因为路径形态不一致而失效。
func New(root string) (*Dir, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s 不是目录", real)
	}
	return &Dir{root: real}, nil
}

// Root 返回脚本目录的真实路径。
func (d *Dir) Root() string { return d.root }

// Resolve 把配置里存的脚本文件名解析成一个可以安全执行的绝对路径。
//
// 四道校验，全部通过才返回：
//  1. 形状检查：拒绝空名、绝对路径、"."、".."
//  2. 拼成绝对路径后必须落在脚本目录内（挡住 "../" 的各种变体）
//  3. 解掉软链后再判断一次落点（挡住 shell/evil.sh -> /etc/passwd）
//  4. 必须是普通文件（挡住目录、FIFO、设备文件）
//
// 注意调用方应当在**每次执行前**重新调用本方法，不要缓存结果、
// 也不要相信数据库里的值：即使有人直接改了库或导入了一份恶意配置，
// 也逃不出脚本目录。
func (d *Dir) Resolve(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("%w：文件名为空", ErrInvalidName)
	}

	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || filepath.IsAbs(clean) {
		return "", fmt.Errorf("%w：%q", ErrInvalidName, name)
	}

	// 第 2 道：filepath.Join 会做归一化，所以 "../etc/passwd" 会拼成
	// 脚本目录的上一级。先对这条未解析的路径判一次越界 —— 这样即便
	// 目标不存在，报的也是"超出范围"而不是"不存在"，原因更准。
	abs := filepath.Join(d.root, clean)
	if !d.contains(abs) {
		return "", fmt.Errorf("%w：%s", ErrOutsideDir, name)
	}

	// 第 3 道：解软链后的最终落点才算数。
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w：%s", ErrNotFound, clean)
		}
		return "", fmt.Errorf("%w：%s（%v）", ErrNotFound, clean, err)
	}
	if !d.contains(real) {
		return "", fmt.Errorf("%w：%s", ErrOutsideDir, name)
	}

	// 第 4 道
	fi, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("%w：%s", ErrNotFound, clean)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%w：%s", ErrNotRegularFile, clean)
	}

	return real, nil
}

// contains 判断一个**已解析**的路径是否真的位于脚本目录内。
//
// 前缀必须带上路径分隔符，否则 /opt/hookexec/shell-evil 会被误判成
// 在 /opt/hookexec/shell 里面。目录本身也不算命中。
func (d *Dir) contains(p string) bool {
	return strings.HasPrefix(p, d.root+string(os.PathSeparator))
}

// List 列出脚本目录第一层的普通文件，按文件名排序。
// 不递归，子目录既不展开也不可选。
func (d *Dir) List() ([]Script, error) {
	entries, err := os.ReadDir(d.root)
	if err != nil {
		return nil, err
	}

	scripts := make([]Script, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		// 读不到信息的条目（比如刚被删掉的软链）直接跳过，不让一个坏条目
		// 把整个列表搞挂。
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		scripts = append(scripts, Script{
			Name:       e.Name(),
			Size:       info.Size(),
			Mode:       info.Mode().Perm().String(),
			Executable: isExecutable(info.Mode()),
			ModTime:    info.ModTime(),
		})
	}

	sort.Slice(scripts, func(i, j int) bool { return scripts[i].Name < scripts[j].Name })
	return scripts, nil
}

// Stat 返回单个脚本的信息，同样走完整的路径校验。
func (d *Dir) Stat(name string) (Script, error) {
	real, err := d.Resolve(name)
	if err != nil {
		return Script{}, err
	}
	info, err := os.Stat(real)
	if err != nil {
		return Script{}, fmt.Errorf("%w：%s", ErrNotFound, name)
	}
	return Script{
		Name:       filepath.Base(real),
		Size:       info.Size(),
		Mode:       info.Mode().Perm().String(),
		Executable: isExecutable(info.Mode()),
		ModTime:    info.ModTime(),
	}, nil
}

// Read 只读地取出脚本内容用于页面预览，超过 maxPreviewBytes 就截断。
// 它复用 Resolve，所以读不出脚本目录之外的东西。
func (d *Dir) Read(name string) (content string, truncated bool, info Script, err error) {
	real, err := d.Resolve(name)
	if err != nil {
		return "", false, Script{}, err
	}

	f, err := os.Open(real)
	if err != nil {
		return "", false, Script{}, fmt.Errorf("%w：%s", ErrNotFound, name)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return "", false, Script{}, err
	}

	buf, err := io.ReadAll(io.LimitReader(f, maxPreviewBytes+1))
	if err != nil {
		return "", false, Script{}, err
	}
	if len(buf) > maxPreviewBytes {
		buf = buf[:maxPreviewBytes]
		truncated = true
	}

	info = Script{
		Name:       filepath.Base(real),
		Size:       fi.Size(),
		Mode:       fi.Mode().Perm().String(),
		Executable: isExecutable(fi.Mode()),
		ModTime:    fi.ModTime(),
	}
	return string(buf), truncated, info, nil
}

// IsExecutable 检查文件是否有可执行位。采用直接 exec（依赖 shebang）的方式，
// 没有可执行位就跑不起来，所以要提前把这件事告诉用户，而不是等执行时
// 抛一个晦涩的 permission denied。
func (d *Dir) IsExecutable(name string) (bool, error) {
	real, err := d.Resolve(name)
	if err != nil {
		return false, err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return false, fmt.Errorf("%w：%s", ErrNotFound, name)
	}
	return isExecutable(fi.Mode()), nil
}

func isExecutable(m os.FileMode) bool { return m.Perm()&0o111 != 0 }
