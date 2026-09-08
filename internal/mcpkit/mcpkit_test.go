package mcpkit

import "testing"

func TestAbsArg(t *testing.T) {
	// 绝对路径通过
	if _, ok := AbsArg(map[string]any{"path": "/a/b/c.go"}, "path"); !ok {
		t.Error("abs path should pass")
	}
	// 相对路径拒绝
	if _, ok := AbsArg(map[string]any{"path": "a/b/c.go"}, "path"); ok {
		t.Error("rel path should reject")
	}
	// 空拒绝
	if _, ok := AbsArg(map[string]any{"path": ""}, "path"); ok {
		t.Error("empty should reject")
	}
	// 缺省拒绝
	if _, ok := AbsArg(map[string]any{}, "path"); ok {
		t.Error("missing should reject")
	}
}
