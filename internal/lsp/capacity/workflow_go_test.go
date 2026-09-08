package lsp_capacity

import (
	"context"
	"encoding/json"
	"strings"
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

func TestGoSortRefs(t *testing.T) {
	in := []map[string]any{
		{"path": "/a/z/deep/x.go"},
		{"path": "/a/y.go"},
		{"path": "/a/z/w.go"},
		{"path": "/a/q.go"},
	}
	goSortRefs("/a/q.go", in)
	// 同目录 (/a) 优先且保持原始相对顺序, 深路径沉底
	want := []string{"/a/y.go", "/a/q.go", "/a/z/w.go", "/a/z/deep/x.go"}
	for i, w := range want {
		if in[i]["path"] != w {
			t.Fatalf("order[%d]=%v want %q (full %+v)", i, in[i]["path"], w, in)
		}
	}
}

func TestGoDiagDiff(t *testing.T) {
	mk := func(msgs ...string) map[string]any {
		raw := []any{}
		for _, m := range msgs {
			raw = append(raw, map[string]any{"message": m, "severity": 1})
		}
		return map[string]any{"count": len(raw), "diagnostics": raw}
	}
	// nil 安全
	if intr, reso := goDiagDiff(nil, nil); len(intr) != 0 || len(reso) != 0 {
		t.Fatalf("nil 差集应为空: intr=%v reso=%v", intr, reso)
	}
	// 多重集差分: before=[A A B] after=[A C] → introduced=[C] resolved=[A B]
	intr, reso := goDiagDiff(mk("A", "A", "B"), mk("A", "C"))
	if len(intr) != 1 || intr[0] != "C" {
		t.Fatalf("introduced 错误: %v", intr)
	}
	if len(reso) != 2 {
		t.Fatalf("resolved 错误: %v", reso)
	}
	seen := map[string]int{}
	for _, m := range reso {
		seen[m]++
	}
	if seen["A"] != 1 || seen["B"] != 1 {
		t.Fatalf("resolved 多重集错误: %v", reso)
	}
}

// TestVerifyChainHints 全语言 hint 可执行断言: buildHint/testHint 非空且以预期可执行命令开头
// (回归 43ae1f3 前 go testHint 曾为不可执行的绝对路径+`/...` 拼接)。
func TestVerifyChainHints(t *testing.T) {
	src := &mockWorkflowSrc{
		diag: map[string]any{"count": 0, "diagnostics": []any{}}, diagOK: true}
	cases := []struct {
		name     string
		p        Workflows
		path     string
		buildPre string
		testPre  string
	}{
		{"go", &goProvider{}, "/a/x.go", "go build", "go test"},
		{"rust", &rustProvider{}, "/a/src/main.rs", "cargo check", "cargo test"},
		{"python", &pythonProvider{}, "/a/pkg/mod.py", "python -m py_compile", "pytest"},
		{"typescript", &typescriptProvider{}, "/a/src/x.ts", "npx tsc", "npm test"},
		{"javascript", &javascriptProvider{}, "/a/src/x.js", "node --check", "npm test"},
		{"csharp", &csharpProvider{}, "/a/Proj/Foo.cs", "dotnet build", "dotnet test"},
	}
	for _, c := range cases {
		out, err := c.p.VerifyChain(context.Background(), src, c.path)
		if err != nil {
			t.Fatalf("%s VerifyChain 失败: %v", c.name, err)
		}
		bh, _ := out["buildHint"].(string)
		th, _ := out["testHint"].(string)
		if !strings.HasPrefix(bh, c.buildPre) {
			t.Errorf("%s buildHint 不可执行: %q want prefix %q", c.name, bh, c.buildPre)
		}
		if !strings.HasPrefix(th, c.testPre) {
			t.Errorf("%s testHint 不可执行: %q want prefix %q", c.name, th, c.testPre)
		}
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
