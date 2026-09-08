package codegraph

import (
	"context"
	"os/exec"
	"testing"
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
