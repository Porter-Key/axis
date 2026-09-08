package langs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Porter-Key/axis/internal/config"
)

func TestDetect(t *testing.T) {
	cfg := config.Default()
	cases := []struct{ path, want string }{
		{"/a/b/main.go", "go"},
		{"/a/b/lib.rs", "rust"},
		{"/a/b/Program.cs", "csharp"},
		{"/a/b/app.py", "python"},
		{"/a/b/comp.ts", "typescript"},
		{"/a/b/comp.tsx", "typescript"},
		{"/a/b/index.js", "javascript"},
		{"/a/b/app.jsx", "javascript"},
		{"/a/b/readme.md", ""},
	}
	for _, c := range cases {
		if got := Detect(cfg, c.path); got != c.want {
			t.Errorf("Detect(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestFindProjectRoot_GoMod(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "myproj")
	if err := os.MkdirAll(filepath.Join(proj, "sub", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(proj, "sub", "deep", "main.go")
	cfg := config.Default()
	root, marker, ok := FindProjectRoot(cfg, "go", file)
	if !ok {
		t.Fatal("expected project root found")
	}
	if root != proj {
		t.Errorf("root = %q, want %q", root, proj)
	}
	if marker != "go.mod" {
		t.Errorf("marker = %q, want go.mod", marker)
	}
}

func TestFindProjectRoot_GlobMarker(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "csproj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "App.csproj"), []byte("<Project/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(proj, "Program.cs")
	cfg := config.Default()
	root, marker, ok := FindProjectRoot(cfg, "csharp", file)
	if !ok {
		t.Fatal("expected csproj root found")
	}
	if root != proj || marker != "App.csproj" {
		t.Errorf("root=%q marker=%q", root, marker)
	}
}

func TestDefaultHasAllLanguages(t *testing.T) {
	cfg := config.Default()
	for _, lang := range []string{"go", "rust", "csharp", "python", "typescript", "javascript"} {
		if _, ok := cfg.Adapters[lang]; !ok {
			t.Errorf("default config missing adapter %q", lang)
		}
	}
}

func TestFindProjectRootBounded(t *testing.T) {
	cfg := config.Default()
	tmp := t.TempDir()
	// tmp/go.mod, tmp/sub/deep/f.go (sub 内无 marker)
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module x"), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(tmp, "sub", "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(deep, "f.go")
	sub := filepath.Join(tmp, "sub")

	// 不限界: 找到祖先 tmp
	if root, _, ok := FindProjectRootBounded(cfg, "go", file, ""); !ok || root != tmp {
		t.Errorf("unbounded: root=%q ok=%v, want %q", root, ok, tmp)
	}
	// 限界到 sub: marker 在边界外 → 不得上浮, 返回 false (调用方兜底 boundary)
	if _, _, ok := FindProjectRootBounded(cfg, "go", file, sub); ok {
		t.Error("bounded(sub): marker above boundary must not match")
	}
	// 限界到 tmp 自身: 本层 marker 有效
	if root, _, ok := FindProjectRootBounded(cfg, "go", file, tmp); !ok || root != tmp {
		t.Errorf("bounded(tmp): root=%q ok=%v, want %q", root, ok, tmp)
	}
	// 边界内侧 marker 优先: sub/go.mod → 定根 sub
	if err := os.WriteFile(filepath.Join(sub, "go.mod"), []byte("module y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if root, _, ok := FindProjectRootBounded(cfg, "go", file, sub); !ok || root != sub {
		t.Errorf("bounded inner marker: root=%q ok=%v, want %q", root, ok, sub)
	}
}
