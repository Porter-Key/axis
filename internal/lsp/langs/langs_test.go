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
