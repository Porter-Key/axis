package lsp

import (
	"encoding/json"
	"testing"
)

// ---- splitSigDoc / hoverText 提取纯逻辑 ----

func TestURItoPath(t *testing.T) {
	cases := map[string]string{
		"file:///home/user/x.go":     "/home/user/x.go",
		"file:///home/user/a b/c.go": "/home/user/a b/c.go",
		"https://example.com/x.go":   "",
	}
	for in, want := range cases {
		if got := uriToPath(in); got != want {
			t.Errorf("uriToPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- hoverText 解析 ----

func TestHoverText_String(t *testing.T) {
	// contents 直接字符串
	v := hoverText(json.RawMessage(`"hello"`))
	if v != "hello" {
		t.Errorf("got %q", v)
	}
}

func TestHoverText_Markup(t *testing.T) {
	// contents 是 {kind, value}
	v := hoverText(json.RawMessage(`{"kind":"markdown","value":"# hi"}`))
	if v != "# hi" {
		t.Errorf("got %q", v)
	}
}

func TestWithinBoundary(t *testing.T) {
	cases := []struct {
		root, boundary string
		want           bool
	}{
		{"/a/b", "/a/b", true},   // 等于边界
		{"/a/b/c", "/a/b", true}, // 边界内
		{"/a", "/a/b", false},    // 祖先 (漂移场景)
		{"/a/c", "/a/b", false},  // 兄弟
		{"/a/bc", "/a/b", false}, // 前缀但非子路径
	}
	for _, c := range cases {
		if got := withinBoundary(c.root, c.boundary); got != c.want {
			t.Errorf("withinBoundary(%q,%q) = %v, want %v", c.root, c.boundary, got, c.want)
		}
	}
}
