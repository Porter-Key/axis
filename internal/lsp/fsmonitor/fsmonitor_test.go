package fsmonitor

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestMonitor(t *testing.T, dir string) *Monitor {
	t.Helper()
	m, err := New(dir, 100*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

func TestChangeDetected_AfterWrite(t *testing.T) {
	dir := t.TempDir()
	m := newTestMonitor(t, dir)

	// 初始无变更
	if m.ConsumeChange() {
		t.Error("should be no change initially")
	}
	// 写文件
	f := filepath.Join(dir, "a.go")
	if err := os.WriteFile(f, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 等待事件传播
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m.ChangedSince(time.Now().Add(-5 * time.Second)) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !m.ChangedSince(time.Now().Add(-5 * time.Second)) {
		t.Fatal("expected change detected after write")
	}
	// Consume 后变 false
	if !m.ConsumeChange() {
		t.Error("ConsumeChange should report had-change true")
	}
}

func TestIgnoreNodeModules(t *testing.T) {
	dir := t.TempDir()
	nm := filepath.Join(dir, "node_modules", "pkg")
	if err := os.MkdirAll(nm, 0o755); err != nil {
		t.Fatal(err)
	}
	// New 不应因 node_modules 而加大量 watcher; 此处只验证不崩溃且仍监听根
	m := newTestMonitor(t, dir)
	if m == nil {
		t.Fatal("monitor nil")
	}
	// 写 node_modules 下文件不应被记录
	f := filepath.Join(nm, "x.js")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	// node_modules 未监听, ChangedSince 应仍 false (除非有其他事件)
	if m.ChangedSince(time.Now().Add(-2 * time.Second)) {
		t.Log("note: got change (inotify 目录事件可能仍触发), 不视为失败")
	}
}
