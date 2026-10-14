package shelldir

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newTestDir 建一个带几个文件的脚本目录，返回 Dir 和它的真实路径。
func newTestDir(t *testing.T) (*Dir, string) {
	t.Helper()
	// 用 EvalSymlinks 解掉 /var -> /private/var 之类的软链，
	// 否则测试里的路径比较会和 Dir 内部不一致。
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("解析临时目录失败：%v", err)
	}
	root := filepath.Join(base, "shell")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("建脚本目录失败：%v", err)
	}

	write(t, filepath.Join(root, "deploy.sh"), "#!/bin/sh\necho deploy\n", 0o755)
	write(t, filepath.Join(root, "notexec.sh"), "#!/bin/sh\necho hi\n", 0o644)
	write(t, filepath.Join(base, "outside.sh"), "#!/bin/sh\necho outside\n", 0o755)

	d, err := New(root)
	if err != nil {
		t.Fatalf("New 失败：%v", err)
	}
	return d, root
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
	// WriteFile 受 umask 影响，显式再 chmod 一次保证权限符合预期。
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod 失败：%v", err)
	}
}

func TestResolveAcceptsRegularFileInsideDir(t *testing.T) {
	d, root := newTestDir(t)

	got, err := d.Resolve("deploy.sh")
	if err != nil {
		t.Fatalf("期望成功，得到 %v", err)
	}
	want := filepath.Join(root, "deploy.sh")
	if got != want {
		t.Fatalf("期望 %q，得到 %q", want, got)
	}
}

// 归一化后仍落在目录内的相对路径应当放行。
func TestResolveNormalizesInnerRelativePath(t *testing.T) {
	d, root := newTestDir(t)
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := d.Resolve("sub/../deploy.sh")
	if err != nil {
		t.Fatalf("期望成功，得到 %v", err)
	}
	if want := filepath.Join(root, "deploy.sh"); got != want {
		t.Fatalf("期望 %q，得到 %q", want, got)
	}
}

// 这是本包存在的理由：一系列必须被拒绝的名字。
func TestResolveRejectsEscapes(t *testing.T) {
	d, root := newTestDir(t)

	// 前缀伪装：造一个和 shell 目录同前缀的兄弟目录，
	// 确保 contains 的分隔符判断没漏。
	sibling := root + "-evil"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(sibling, "pwn.sh"), "#!/bin/sh\necho pwn\n", 0o755)

	cases := []struct {
		name string
		in   string
		want error
	}{
		{"空字符串", "", ErrInvalidName},
		{"只有空白", "   ", ErrInvalidName},
		{"点", ".", ErrInvalidName},
		{"双点", "..", ErrInvalidName},
		{"绝对路径 /etc/passwd", "/etc/passwd", ErrInvalidName},
		{"绝对路径指向兄弟目录", filepath.Join(sibling, "pwn.sh"), ErrInvalidName},
		{"上级目录逃逸", "../outside.sh", ErrOutsideDir},
		{"多级上级逃逸", "../../etc/passwd", ErrOutsideDir},
		{"经过子目录的逃逸", "sub/../../outside.sh", ErrOutsideDir},
		{"前缀伪装的兄弟目录", "../shell-evil/pwn.sh", ErrOutsideDir},
		{"不存在的文件", "nope.sh", ErrNotFound},
		{"子目录本身", "sub", ErrNotRegularFile},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// sub 目录只在部分用例里需要
			if strings.Contains(tc.in, "sub") && tc.want != ErrInvalidName {
				if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			_, err := d.Resolve(tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("输入 %q：期望 %v，得到 %v", tc.in, tc.want, err)
			}
		})
	}
}

// 软链指向目录外时必须被拒 —— 光看名字是看不出来的。
func TestResolveRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建软链需要额外权限")
	}
	d, root := newTestDir(t)

	// 指向目录外的一个真实文件
	if err := os.Symlink(filepath.Join(root, "..", "outside.sh"), filepath.Join(root, "evil.sh")); err != nil {
		t.Fatalf("建软链失败：%v", err)
	}
	// 指向 /etc/passwd 这类系统文件
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "passwd.sh")); err != nil {
		t.Fatalf("建软链失败：%v", err)
	}
	// 指向一个目录
	if err := os.Symlink(root, filepath.Join(root, "selfdir")); err != nil {
		t.Fatalf("建软链失败：%v", err)
	}

	for _, name := range []string{"evil.sh", "passwd.sh"} {
		t.Run(name, func(t *testing.T) {
			if _, err := d.Resolve(name); !errors.Is(err, ErrOutsideDir) {
				t.Fatalf("期望 %v，得到 %v", ErrOutsideDir, err)
			}
		})
	}

	// 指向目录内的软链是允许的 —— 它没有逃出去
	inside := filepath.Join(root, "alias.sh")
	if err := os.Symlink(filepath.Join(root, "deploy.sh"), inside); err != nil {
		t.Fatalf("建软链失败：%v", err)
	}
	if _, err := d.Resolve("alias.sh"); err != nil {
		t.Fatalf("目录内的软链应当放行，得到 %v", err)
	}

	// 指向脚本目录本身的软链，解析后落在目录上而非目录内的文件，
	// 由包含判断拦下（contains 明确把目录自身排除在外）
	if _, err := d.Resolve("selfdir"); !errors.Is(err, ErrOutsideDir) {
		t.Fatalf("期望 %v，得到 %v", ErrOutsideDir, err)
	}
}

// 目录和 FIFO 都不是普通文件。
func TestResolveRejectsNonRegularFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建 FIFO 不受支持")
	}
	d, root := newTestDir(t)

	if err := os.MkdirAll(filepath.Join(root, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Resolve("adir"); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("目录：期望 %v，得到 %v", ErrNotRegularFile, err)
	}

	fifo := filepath.Join(root, "apipe")
	if err := mkfifo(fifo, 0o644); err != nil {
		t.Skipf("无法创建 FIFO，跳过：%v", err)
	}
	if _, err := d.Resolve("apipe"); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("FIFO：期望 %v，得到 %v", ErrNotRegularFile, err)
	}
}

func TestListOnlyRegularFilesShallow(t *testing.T) {
	d, root := newTestDir(t)
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "sub", "nested.sh"), "#!/bin/sh\n", 0o755)

	scripts, err := d.List()
	if err != nil {
		t.Fatalf("List 失败：%v", err)
	}

	names := make([]string, 0, len(scripts))
	for _, s := range scripts {
		names = append(names, s.Name)
	}
	// deploy.sh 与 notexec.sh，按文件名排序；子目录不展开
	want := []string{"deploy.sh", "notexec.sh"}
	if len(names) != len(want) {
		t.Fatalf("期望 %v，得到 %v", want, names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("期望 %v，得到 %v", want, names)
		}
	}
}

func TestListReportsExecutableBit(t *testing.T) {
	d, _ := newTestDir(t)
	scripts, err := d.List()
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]Script{}
	for _, s := range scripts {
		byName[s.Name] = s
	}

	if !byName["deploy.sh"].Executable {
		t.Error("deploy.sh 应当被标记为可执行")
	}
	if byName["notexec.sh"].Executable {
		t.Error("notexec.sh 不应当被标记为可执行")
	}
}

func TestReadTruncatesLargeFile(t *testing.T) {
	d, root := newTestDir(t)

	big := strings.Repeat("A", maxPreviewBytes+1024)
	write(t, filepath.Join(root, "big.sh"), big, 0o755)

	content, truncated, info, err := d.Read("big.sh")
	if err != nil {
		t.Fatalf("Read 失败：%v", err)
	}
	if !truncated {
		t.Error("超限文件应当标记为已截断")
	}
	if len(content) != maxPreviewBytes {
		t.Errorf("期望截断到 %d 字节，得到 %d", maxPreviewBytes, len(content))
	}
	if info.Size != int64(len(big)) {
		t.Errorf("info.Size 应当是原始大小 %d，得到 %d", len(big), info.Size)
	}
}

// Read 必须和 Resolve 走同一套校验，不能成为另一个任意文件读取入口。
func TestReadRefusesEscape(t *testing.T) {
	d, _ := newTestDir(t)

	for _, name := range []string{"../outside.sh", "/etc/passwd", "..", ""} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := d.Read(name); err == nil {
				t.Fatalf("Read(%q) 应当失败", name)
			}
		})
	}
}

func TestNewRejectsNonDirectory(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(base, "afile")
	write(t, f, "x", 0o644)

	if _, err := New(f); err == nil {
		t.Fatal("对一个普通文件调用 New 应当失败")
	}
}
