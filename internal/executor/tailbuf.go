package executor

import (
	"sync"
	"unicode/utf8"
)

// DefaultTailSize 是每条记录保留的输出字节数。
const DefaultTailSize = 32 << 10 // 32KB

// TailBuffer 是一个只保留末尾 N 字节的 io.Writer。
//
// 为什么不用 bytes.Buffer：它无上限增长，一个失控的脚本能直接把内存吃满。
// 为什么不用 bufio.Scanner 逐行读：它有 64KB 的行长上限，遇到一行超长
// 输出（比如压缩过的 JSON 或二进制数据）会静默截断甚至报错。
//
// 保留尾部而不是头部，是因为脚本失败时最有用的信息通常在最后几行。
type TailBuffer struct {
	mu        sync.Mutex
	buf       []byte
	max       int
	truncated bool
}

// NewTailBuffer 创建一个上限为 max 字节的缓冲。max 非正数时用默认值。
func NewTailBuffer(max int) *TailBuffer {
	if max <= 0 {
		max = DefaultTailSize
	}
	return &TailBuffer{max: max}
}

// Write 实现 io.Writer。永远返回 len(p), nil —— 我们从不希望
// 因为缓冲区满了就让子进程的写操作出错。
func (t *TailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// 单次写入就超过容量：只留这一段的尾部，前面的直接不要了。
	if len(p) >= t.max {
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		t.truncated = true
		return len(p), nil
	}

	// 加上新数据会溢出的话，从头部丢掉溢出的部分。
	// append(dst, src...) 在 src 与 dst 重叠时是安全的（底层用 memmove）。
	if overflow := len(t.buf) + len(p) - t.max; overflow > 0 {
		t.buf = append(t.buf[:0], t.buf[overflow:]...)
		t.truncated = true
	}

	t.buf = append(t.buf, p...)
	return len(p), nil
}

// String 返回缓冲内容。
//
// 因为是从任意字节位置开始截断的，开头很可能落在一个多字节字符的中间，
// 直接转字符串会得到替换字符乱码。这里往前跳过最多 3 个续字节，把起点
// 对齐到字符边界上。中文和 emoji 输出因此不会在拼接处花掉。
func (t *TailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	b := t.buf
	for i := 0; i < utf8.UTFMax-1 && i < len(b); i++ {
		if utf8.RuneStart(b[i]) {
			b = b[i:]
			break
		}
	}
	return string(b)
}

// Truncated 表示是否有内容因为超出上限被丢弃。
func (t *TailBuffer) Truncated() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.truncated
}

// Len 返回当前缓冲的字节数。
func (t *TailBuffer) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.buf)
}
