package lsp_capacity

import (
	"context"
	"encoding/json"
	"testing"
)

// mockWorkflowSrc workflow 测试桩 (host 行为可配)。
type mockWorkflowSrc struct {
	def, refs, hov json.RawMessage
	defOK, refsOK  bool
	diag           map[string]any
	diagOK         bool
	previewed      string
	restored       int
}

func (m *mockWorkflowSrc) HoverMarkdown(ctx context.Context, path string, line, char int) (string, bool) {
	return "```go\nfunc Foo(a int) string\n```\n\nFoo 文档。", true
}
func (m *mockWorkflowSrc) Definition(ctx context.Context, path string, line, char int) (json.RawMessage, bool) {
	return m.def, m.defOK
}
func (m *mockWorkflowSrc) References(ctx context.Context, path string, line, char int) (json.RawMessage, bool) {
	return m.refs, m.refsOK
}
func (m *mockWorkflowSrc) Hover(ctx context.Context, path string, line, char int) (json.RawMessage, bool) {
	return m.hov, true
}
func (m *mockWorkflowSrc) Diagnostics(ctx context.Context, path string) (map[string]any, bool) {
	return m.diag, m.diagOK
}
func (m *mockWorkflowSrc) Supports(cap Capability) bool { return true }
func (m *mockWorkflowSrc) PreviewText(ctx context.Context, path, text string) error {
	m.previewed = text
	return nil
}
func (m *mockWorkflowSrc) RestoreFile(ctx context.Context, path string) error {
	m.restored++
	return nil
}

func TestGoApplyEdits(t *testing.T) {
	orig := "line0\nline1\nline2\n"
	got, err := goApplyEdits(orig, []TextEdit{
		{StartLine: 1, StartChar: 0, EndLine: 1, EndChar: 5, NewText: "LINE1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "line0\nLINE1\nline2\n" {
		t.Fatalf("got %q", got)
	}
	// 多段 (乱序传入也按位置应用)
	got, err = goApplyEdits(orig, []TextEdit{
		{StartLine: 2, StartChar: 4, EndLine: 2, EndChar: 5, NewText: "2"},
		{StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 4, NewText: "L0"},
	})
	if err != nil || got != "L00\nline1\nline2\n" {
		t.Fatalf("got %q err %v", got, err)
	}
	// 越界 / 重叠报错
	if _, err := goApplyEdits(orig, []TextEdit{{StartLine: 9, EndLine: 9}}); err == nil {
		t.Error("行越界应报错")
	}
	if _, err := goApplyEdits(orig, []TextEdit{
		{StartLine: 0, StartChar: 0, EndLine: 1, EndChar: 0},
		{StartLine: 0, StartChar: 3, EndLine: 0, EndChar: 5},
	}); err == nil {
		t.Error("重叠应报错")
	}
}

func TestGoBlastRadiusPartition(t *testing.T) {
	p := &goProvider{}
	def := json.RawMessage(`{"uri":"file:///a/x.go","range":{"start":{"line":10,"character":6},"end":{"line":10,"character":9}}}`)
	refs := json.RawMessage(`[
		{"uri":"file:///a/x.go","range":{"start":{"line":20,"character":2},"end":{"line":20,"character":5}}},
		{"uri":"file:///a/x_test.go","range":{"start":{"line":5,"character":1},"end":{"line":5,"character":4}}}
	]`)
	src := &mockWorkflowSrc{def: def, defOK: true, refs: refs, refsOK: true,
		diag: map[string]any{"count": 0, "diagnostics": []any{}}, diagOK: true}
	out, err := p.BlastRadius(context.Background(), src, "/a/x.go", 20, 2)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := out["definition"].(map[string]any)
	if d["symbol"] != "Foo" {
		t.Fatalf("definition 签名未 enrich: %+v", out["definition"])
	}
	r, _ := out["references"].(map[string]any)
	if r["total"] != 2 || r["testCount"] != 1 || r["nonTestCount"] != 1 {
		t.Fatalf("分区错误: %+v", out["references"])
	}
}
