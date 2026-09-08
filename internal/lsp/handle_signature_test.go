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
		"https://example.com/x.go":     "",
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
