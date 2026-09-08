package codegraph

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func requireCodegraph(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("codegraph"); err != nil {
		t.Skip("codegraph not in PATH, skip")
	}
}

// 需要一个有索引的项目做集成测试; 无则跳过。
func TestQueryJSON(t *testing.T) {
	requireCodegraph(t)
	root := findIndexedProject(t)
	if root == "" {
		t.Skip("no indexed project found")
	}
	c := New(ConfigForTest())
	out, err := c.Query(context.Background(), root, "fn main", "", 5)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(out) == 0 || out[0] != '{' {
		t.Errorf("expected JSON object, got %s", string(out)[:min(50, len(out))])
	}
	t.Logf("query ok: %s", string(out)[:min(120, len(out))])
}

func TestStatusJSON(t *testing.T) {
	requireCodegraph(t)
	root := findIndexedProject(t)
	if root == "" {
		t.Skip("no indexed project found")
	}
	c := New(ConfigForTest())
	out, err := c.Status(context.Background(), root)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	t.Logf("status: %s", string(out)[:min(150, len(out))])
}

func findIndexedProject(t *testing.T) string {
	t.Helper()
	for _, cand := range []string{"/home/user/src/proj", "/home/user/services", "/home/user/projects/myproject"} {
		c := New(ConfigForTest())
		if c.HasIndex(cand) {
			return cand
		}
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestSyncGateSingleflight 回归 (P2-9): 并发查询同项目只 sync 一次,
// 且 10s 新鲜窗口内后续查询直接跳过。用 fake bin 脚本计数真实 fork 次数。
func TestSyncGateSingleflight(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".codegraph"), 0o755); err != nil {
		t.Fatal(err)
	}
	countFile := filepath.Join(dir, "count")
	script := filepath.Join(dir, "fake-cg.sh")
	scriptBody := "#!/bin/sh\necho x >> \"" + countFile + "\"\nsleep 0.3\nexit 0\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Client{bin: script, autoInit: false, syncOnChg: true, timeout: 10 * time.Second}

	// 10 并发 → 只应 fork 1 次 sync
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = c.syncGate(context.Background(), dir)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	data, _ := os.ReadFile(countFile)
	if n := len(bytes.Split(bytes.TrimSpace(data), []byte("\n"))); n != 1 {
		t.Errorf("10 并发只应 sync 1 次, 实际 %d 次", n)
	}
	// 新鲜窗口内再查 → 0 新增
	if err := c.syncGate(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(countFile)
	if n := len(bytes.Split(bytes.TrimSpace(data), []byte("\n"))); n != 1 {
		t.Errorf("新鲜窗口内应跳过 sync, 实际累计 %d 次", n)
	}
}
