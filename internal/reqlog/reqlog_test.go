package reqlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestRotation 验证：超限自动轮转、历史文件滚动、最老被删、内容完整。
func TestRotation(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "requests.jsonl")

	// maxBytes=300（约 1-2 条/文件），maxBackups=2
	l := NewWithRotate(f, 100, 300, 2)
	defer l.Close()

	for i := 0; i < 30; i++ {
		l.Log(&Entry{Method: "POST", Path: "/v1/chat/completions", Model: "m", Status: 200})
	}

	// 当前文件 + 2 个历史
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	t.Logf("files: %v", names)

	if _, err := os.Stat(f); err != nil {
		t.Fatalf("current file missing: %v", err)
	}
	if _, err := os.Stat(f + ".1"); err != nil {
		t.Fatalf("backup .1 missing: %v", err)
	}
	if _, err := os.Stat(f + ".2"); err != nil {
		t.Fatalf("backup .2 missing: %v", err)
	}
	if _, err := os.Stat(f + ".3"); err == nil {
		t.Fatal("backup .3 should have been deleted")
	}

	// 当前文件不超过 maxBytes
	if st, _ := os.Stat(f); st.Size() > 300 {
		t.Fatalf("current file too big: %d", st.Size())
	}

	// 每行都是合法 JSON
	raw, _ := os.ReadFile(f)
	for i, line := range splitLines(string(raw)) {
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d not json: %v", i, err)
		}
	}

	// 内存缓冲完整（30 条）
	if got := len(l.Recent(0)); got != 30 {
		t.Fatalf("memory buffer lost entries: %d", got)
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
