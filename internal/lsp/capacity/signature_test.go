package lsp_capacity

import (
	"context"
	"testing"
)

// ---- go provider 签名解析 (从 mcpserver 迁入: splitSigDoc/parseSymbolName/moduleImportPath) ----

func TestGoSplitFenceSigDoc(t *testing.T) {
	md := "```go\nfunc NewMCPServer(name, version string, opts ...ServerOption) *Server\n```\n\n---\n\n[`server` on pkg.go.dev](https://pkg.go.dev/...)  \nNewMCPServer 创建一个 MCP server。"
	sig, doc, ok := goSplitFenceSigDoc(md)
	if !ok {
		t.Fatal("ok=false")
	}
	if sig != "func NewMCPServer(name, version string, opts ...ServerOption) *Server" {
		t.Errorf("sig mismatch: %q", sig)
	}
	if doc == "" {
		t.Error("doc empty")
	}
}

func TestGoSplitPlainDecl(t *testing.T) {
	// go doc 纯文本 decl: 首行签名, // 注释行是 doc
	decl := "func NewMCPServer(name, version string, opts ...ServerOption) *Server\n// NewMCPServer 创建 server\nfunc Other()"
	sig, doc := goSplitPlainDecl(decl)
	if sig == "" {
		t.Fatal("sig empty")
	}
	if doc == "" {
		t.Error("doc empty, got:", doc)
	}
}

func TestGoParseSymbolName(t *testing.T) {
	cases := []struct{ sig, want string }{
		{"func (c *Conn) CallWithDoc(ctx context.Context, method string) (json.RawMessage, error)", "CallWithDoc"},
		{"func NewMCPServer(name, version string, opts ...ServerOption)", "NewMCPServer"},
		{"func main()", "main"},
		{"type Greeter interface {", "Greeter"},
		// 多行: gopls hover 类型定义后附方法列表 → 取首行主声明
		{"type EnglishGreeter struct{} // size=0\nfunc (g EnglishGreeter) Greet(name string) string", "EnglishGreeter"},
		{"type Greeter interface {\nGreet(name string) string\n}", "Greeter"},
	}
	for _, c := range cases {
		if got := goParseSymbolName(c.sig); got != c.want {
			t.Errorf("goParseSymbolName(%q) = %q, want %q", c.sig, got, c.want)
		}
	}
}

func TestGoModuleImportPath(t *testing.T) {
	imp := goModuleImportPath("/home/user/go/pkg/mod/github.com/mark3labs/mcp-go@v1.0.0/server/server.go")
	want := "github.com/mark3labs/mcp-go/server"
	if imp != want {
		t.Errorf("imp = %q, want %q", imp, want)
	}
	if imp2 := goModuleImportPath("/home/user/proj/x.go"); imp2 != "" {
		t.Errorf("expected empty, got %q", imp2)
	}
}

func TestGoIsModuleCache(t *testing.T) {
	if !goIsModuleCache("/home/user/go/pkg/mod/x/y.go") {
		t.Error("should detect module cache")
	}
	if goIsModuleCache("/home/user/proj/x.go") {
		t.Error("should not detect non-cache")
	}
}

// ---- Signature 编排: mock src ----

type mockSrc struct {
	md  string
	ok  bool
	got string
}

func (m *mockSrc) HoverMarkdown(ctx context.Context, path string, line, char int) (string, bool) {
	return m.md, m.ok
}

func TestGoSignature_Hover(t *testing.T) {
	p := &goProvider{}
	src := &mockSrc{md: "```go\nfunc FormatName(prefix string, name string) string\n```\n\n---\n\nFormatName 格式化名字。", ok: true}
	dto, ok := p.Signature(context.Background(), src, "/x/helper.go", 3, 5)
	if !ok {
		t.Fatal("ok=false")
	}
	if dto.Signature != "func FormatName(prefix string, name string) string" {
		t.Errorf("sig: %q", dto.Signature)
	}
	if dto.Symbol != "FormatName" {
		t.Errorf("symbol: %q", dto.Symbol)
	}
	if dto.Doc == "" {
		t.Error("doc empty")
	}
}

func TestGoSignature_NoHover(t *testing.T) {
	p := &goProvider{}
	src := &mockSrc{ok: false}
	dto, ok := p.Signature(context.Background(), src, "/home/user/proj/x.go", 1, 1)
	if ok {
		t.Error("expected ok=false for non-module-cache no-hover, got:", dto)
	}
}

func TestPythonSignature_Hover(t *testing.T) {
	p := &pythonProvider{}
	md := "```python\n(function) def make_greeter(\n    prefix: str = \"Hello\",\n    *,\n    loud: bool = False\n) -> ((name: str) -> str)\n```\n---\n创建一个带前缀的问候函数。"
	src := &mockSrc{md: md, ok: true}
	dto, ok := p.Signature(context.Background(), src, "/x/main.py", 21, 9)
	if !ok {
		t.Fatal("ok=false")
	}
	// 完整多行签名必须保留 (含续行与返回值)
	if dto.Signature == "def make_greeter(" {
		t.Errorf("sig 被截断: %q", dto.Signature)
	}
	if dto.Symbol != "make_greeter" {
		t.Errorf("symbol: %q", dto.Symbol)
	}
	if dto.Doc == "" {
		t.Error("doc empty")
	}
}

func TestRustSignature_DeclFenceSelected(t *testing.T) {
	p := &rustProvider{}
	// 第一围栏是路径, 第二才是签名 → 应选第二个
	md := "```rust\nmultilang::Greeter\n```\n\n```rust\ntrait Greeter\nfn greet(&self, name: &str) -> String\n```"
	src := &mockSrc{md: md, ok: true}
	dto, ok := p.Signature(context.Background(), src, "/x/main.rs", 1, 8)
	if !ok {
		t.Fatal("ok=false")
	}
	if dto.Signature != "trait Greeter\nfn greet(&self, name: &str) -> String" {
		t.Errorf("sig: %q", dto.Signature)
	}
	if dto.Symbol != "Greeter" {
		t.Errorf("symbol: %q", dto.Symbol)
	}
}

func TestCSharpSignature(t *testing.T) {
	p := &csharpProvider{}
	src := &mockSrc{md: "```csharp\nstring IGreeter.Greet(string name)\n```", ok: true}
	dto, ok := p.Signature(context.Background(), src, "/x/Program.cs", 2, 11)
	if !ok {
		t.Fatal("ok=false")
	}
	if dto.Signature != "string IGreeter.Greet(string name)" {
		t.Errorf("sig: %q", dto.Signature)
	}
	if dto.Symbol != "Greet" {
		t.Errorf("symbol: %q", dto.Symbol)
	}
}

func TestTSSignature(t *testing.T) {
	p := &typescriptProvider{}
	src := &mockSrc{md: "```typescript\n(method) EnglishGreeter.greet(name: string): string\n```", ok: true}
	dto, ok := p.Signature(context.Background(), src, "/x/greeter.ts", 5, 3)
	if !ok {
		t.Fatal("ok=false")
	}
	if dto.Symbol != "greet" {
		t.Errorf("symbol: %q", dto.Symbol)
	}
	if dto.Signature == "" {
		t.Error("sig empty")
	}
}

func TestJSSignature(t *testing.T) {
	p := &javascriptProvider{}
	src := &mockSrc{md: "```javascript\n(function) Greeter(prefix)\n```", ok: true}
	dto, ok := p.Signature(context.Background(), src, "/x/main.js", 0, 6)
	if !ok {
		t.Fatal("ok=false")
	}
	if dto.Symbol != "Greeter" {
		t.Errorf("symbol: %q", dto.Symbol)
	}
}
