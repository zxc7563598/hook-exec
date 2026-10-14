package executor

import (
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestTailBufferKeepsLastBytes(t *testing.T) {
	tb := NewTailBuffer(10)
	if _, err := tb.Write([]byte("0123456789ABCDEF")); err != nil {
		t.Fatal(err)
	}
	if got := tb.String(); got != "6789ABCDEF" {
		t.Fatalf("期望尾部 10 字节，得到 %q", got)
	}
	if !tb.Truncated() {
		t.Error("应当标记为已截断")
	}
}

func TestTailBufferAccumulatesSmallWrites(t *testing.T) {
	tb := NewTailBuffer(100)
	for _, s := range []string{"aaa", "bbb", "ccc"} {
		if _, err := tb.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if got := tb.String(); got != "aaabbbccc" {
		t.Fatalf("期望 aaabbbccc，得到 %q", got)
	}
	if tb.Truncated() {
		t.Error("未超限不应标记截断")
	}
}

func TestTailBufferTruncatesAcrossWrites(t *testing.T) {
	tb := NewTailBuffer(8)
	tb.Write([]byte("12345"))
	if tb.Truncated() {
		t.Error("此时还不该截断")
	}
	tb.Write([]byte("67890"))
	if got := tb.String(); got != "34567890" {
		t.Fatalf("期望 34567890，得到 %q", got)
	}
	if !tb.Truncated() {
		t.Error("应当标记为已截断")
	}
}

// 从多字节字符中间截断时必须对齐到字符边界，否则页面首行全是乱码。
func TestTailBufferAlignsUTF8Boundary(t *testing.T) {
	// 每个汉字 3 字节
	text := strings.Repeat("中", 10) // 30 字节
	if len(text) != 30 {
		t.Fatalf("测试前提有误，len=%d", len(text))
	}

	// 容量 10：截断起点会落在第 20 字节，而 20 不是 3 的倍数，
	// 正好落在某个汉字的中间。
	tb := NewTailBuffer(10)
	tb.Write([]byte(text))

	got := tb.String()
	if !utf8.ValidString(got) {
		t.Fatalf("输出不是合法 UTF-8：%q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("输出含替换字符（边界未对齐）：%q", got)
	}
	// 对齐后会少掉最多 2 个字节，剩下的应当全是完整的"中"
	for _, r := range got {
		if r != '中' {
			t.Fatalf("出现意外字符 %q", r)
		}
	}
}

// 二进制输出（非法 UTF-8）不应让程序崩溃，原样保留即可。
func TestTailBufferHandlesBinaryOutput(t *testing.T) {
	tb := NewTailBuffer(16)
	raw := []byte{0xff, 0xfe, 0xfd, 0x00, 0x01, 0x02, 0x80, 0x81}
	tb.Write(raw)

	if got := tb.String(); got != string(raw) {
		t.Fatalf("二进制内容应原样保留")
	}
}

// stdout 与 stderr 共用同一个 buffer，必须能扛住并发写。
// 跑 -race 时这个用例才有意义。
func TestTailBufferConcurrentWrites(t *testing.T) {
	tb := NewTailBuffer(1024)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				tb.Write([]byte("line of output\n"))
			}
		}()
	}
	wg.Wait()

	if tb.Len() != 1024 {
		t.Fatalf("并发写后应当正好填满容量，得到 %d", tb.Len())
	}
	if !tb.Truncated() {
		t.Error("并发写这么多内容后应当标记为已截断")
	}
}

// 单次写入超过容量时不应 panic，也不该越界。
func TestTailBufferSingleHugeWrite(t *testing.T) {
	tb := NewTailBuffer(5)
	tb.Write([]byte("HELLOWORLD"))
	if got := tb.String(); got != "WORLD" {
		t.Fatalf("期望 WORLD，得到 %q", got)
	}
}
