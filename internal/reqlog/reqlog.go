// Package reqlog 请求日志：内存环形缓冲 + 持久化文件（JSONL），控制台展示用。
// 文件按大小自动轮转：requests.jsonl → requests.jsonl.1 → … → requests.jsonl.<N>。
package reqlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// 默认轮转参数（可被 New 的参数覆盖）。
const (
	DefaultMaxBytes     = 10 << 20 // 10MB 单文件上限
	DefaultMaxBackups   = 2        // 保留历史文件数
)

// Entry 单条请求日志。
type Entry struct {
	Time      time.Time `json:"time"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Model     string    `json:"model,omitempty"`
	UID       string    `json:"uid,omitempty"`      // 命中的账号
	Status    int       `json:"status"`             // 返回给客户端的状态码
	Duration  float64   `json:"duration_ms"`        // 耗时毫秒
	Stream    bool      `json:"stream"`
	ReqTokens int       `json:"req_tokens,omitempty"`
	RespTokens int      `json:"resp_tokens,omitempty"`
	ClientIP  string    `json:"client_ip,omitempty"`
	Error     string    `json:"error,omitempty"`    // 上游错误摘要
}

// Logger 环形缓冲 + 文件追加（带轮转）。
type Logger struct {
	mu        sync.Mutex
	entries   []*Entry
	max       int
	fp        *os.File
	w         *bufio.Writer
	file      string
	maxBytes  int64
	maxBackup int
	written   int64 // 当前文件已写字节
	writeErr  error // 最近一次写错误（诊断用）
}

// New 构建日志器；file 非空则追加写入并启用轮转。
func New(file string, maxEntries int) *Logger {
	return NewWithRotate(file, maxEntries, DefaultMaxBytes, DefaultMaxBackups)
}

// NewWithRotate 自定义轮转参数。
func NewWithRotate(file string, maxEntries, maxBytes, maxBackups int) *Logger {
	if maxEntries <= 0 {
		maxEntries = 500
	}
	l := &Logger{max: maxEntries, file: file, maxBytes: int64(maxBytes), maxBackup: maxBackups}
	if file != "" {
		l.open()
	}
	return l
}

// open 打开（或续写）日志文件，统计已有大小。
func (l *Logger) open() {
	if st, err := os.Stat(l.file); err == nil {
		l.written = st.Size()
	}
	fp, err := os.OpenFile(l.file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		l.writeErr = err
		return
	}
	l.fp = fp
	l.w = bufio.NewWriter(fp)
	l.writeErr = nil
}

// rotate 关闭当前文件，滚动命名，重开新文件。调用方持锁。
func (l *Logger) rotate() {
	if l.w != nil {
		l.w.Flush()
	}
	if l.fp != nil {
		l.fp.Close()
	}
	// requests.jsonl.(N-1) → requests.jsonl.N … requests.jsonl → requests.jsonl.1
	for i := l.maxBackup; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", l.file, i)
		if i == l.maxBackup {
			os.Remove(src) // 最老的历史直接删除
			continue
		}
		os.Rename(src, fmt.Sprintf("%s.%d", l.file, i+1))
	}
	if l.written > 0 {
		os.Rename(l.file, l.file+".1")
	}
	l.written = 0
	l.open()
}

// Log 追加一条（锁内快速完成；磁盘错误静默但记录在 writeErr）。
func (l *Logger) Log(e *Entry) {
	if e == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
	if len(l.entries) > l.max {
		// 滑动窗口裁剪
		l.entries = l.entries[len(l.entries)-l.max:]
	}
	if l.w == nil {
		return
	}
	if raw, err := json.Marshal(e); err == nil {
		n, _ := l.w.Write(raw)
		l.w.WriteByte('\n')
		l.written += int64(n) + 1
		// 每条 flush：防容器异常退出丢日志；量小可接受
		if err := l.w.Flush(); err != nil {
			l.writeErr = err
			return
		}
		// 超限轮转（在 flush 之后，保证当前文件完整）
		if l.written >= l.maxBytes {
			l.rotate()
		}
	}
}

// Recent 返回最近 n 条（新→旧）；n<=0 返回全部。
func (l *Logger) Recent(n int) []*Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > len(l.entries) {
		n = len(l.entries)
	}
	out := make([]*Entry, n)
	// 倒序拷贝
	for i := 0; i < n; i++ {
		out[i] = l.entries[len(l.entries)-1-i]
	}
	return out
}

// WriteErr 返回最近一次文件写错误（nil = 健康）。
func (l *Logger) WriteErr() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.writeErr
}

// Close 关闭文件。
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w != nil {
		l.w.Flush()
	}
	if l.fp != nil {
		l.fp.Close()
	}
}
