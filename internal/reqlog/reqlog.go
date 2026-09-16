// Package reqlog 请求日志：内存环形缓冲 + 持久化文件（JSONL），控制台展示用。
package reqlog

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"
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

// Logger 环形缓冲 + 文件追加。
type Logger struct {
	mu      sync.Mutex
	entries []*Entry
	max     int
	fp      *os.File
	w       *bufio.Writer
}

// New 构建日志器；file 非空则同时追加写入（JSONL）。
func New(file string, maxEntries int) *Logger {
	if maxEntries <= 0 {
		maxEntries = 500
	}
	l := &Logger{max: maxEntries}
	if file != "" {
		if f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			l.fp = f
			l.w = bufio.NewWriter(f)
		}
	}
	return l
}

// Log 追加一条（非阻塞语义：锁内快速完成）。
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
	if l.w != nil {
		if raw, err := json.Marshal(e); err == nil {
			l.w.Write(raw)
			l.w.WriteByte('\n')
			// 小缓冲：每条都 flush，防容器异常退出丢日志；量小可接受
			l.w.Flush()
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
