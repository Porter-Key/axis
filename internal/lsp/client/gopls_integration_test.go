package lspclient

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Porter-Key/axis/internal/config"
)

// requireGopls 跳过无 gopls 环境。
func requireGopls(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed, skip integration")
	}
}

// 建一个临时 Go module, 返回项目根 + 文件路径。
func makeGoModule(t *testing.T) (root, file string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/m\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(dir, "main.go")
	src := `package main

import "fmt"

func target() string { return "hello" }

func main() {
	fmt.Println(target())
}
`
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, file
}

func TestRealGopls_DefinitionAndHover(t *testing.T) {
	requireGopls(t)
	root, file := makeGoModule(t)
	ad := config.LangAdapter{
		LanguageID: "go", Command: "gopls",
		FilePatterns: []string{"**/*.go"},
		TimeoutSec:   30,
	}
	conn, err := Spawn(context.Background(), ad, root, 30*time.Second)
	if err != nil {
		t.Fatalf("spawn gopls: %v", err)
	}
	defer conn.Close()

	// main.go 中 "target()" 调用在第 7 行 (0-based), 定位 target 标识符
	// 用 hover 验证简单 (不依赖精确列): 第 7 行 target 大概 col 14 附近
	res, err := conn.CallWithDoc(context.Background(), "textDocument/hover", file,
		map[string]any{"position": map[string]any{"line": 7, "character": 15}},
		20*time.Second)
	if err != nil {
		t.Fatalf("hover: %v", err)
	}
	if len(res) == 0 || string(res) == "null" {
		t.Fatal("hover result empty")
	}
	t.Logf("hover: %s", truncateStr(string(res), 200))
}

func TestRealGopls_References(t *testing.T) {
	requireGopls(t)
	root, file := makeGoModule(t)
	ad := config.LangAdapter{LanguageID: "go", Command: "gopls", TimeoutSec: 30}
	conn, err := Spawn(context.Background(), ad, root, 30*time.Second)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer conn.Close()
	// references on target definition (第 5 行 0-based = "func target() string..."), target 起始列 9
	res, err := conn.CallWithDoc(context.Background(), "textDocument/references", file,
		map[string]any{
			"position": map[string]any{"line": 4, "character": 6},
			"context":  map[string]any{"includeDeclaration": true},
		}, 20*time.Second)
	if err != nil {
		t.Fatalf("references: %v", err)
	}
	var locs []struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(res, &locs); err != nil {
		t.Fatalf("unmarshal refs: %v (%s)", err, res)
	}
	if len(locs) == 0 {
		t.Fatal("expected at least 1 reference")
	}
	for _, l := range locs {
		if !strings.Contains(l.URI, "main.go") {
			t.Errorf("unexpected uri %s", l.URI)
		}
	}
	t.Logf("refs: %d", len(locs))
}

// 验证短连接模型: 两次独立调用之间文件修改后, 第二次应读到新内容 (新鲜度)。
func TestShortConnection_SeesUpdatedFile(t *testing.T) {
	requireGopls(t)
	root, file := makeGoModule(t)
	ad := config.LangAdapter{LanguageID: "go", Command: "gopls", TimeoutSec: 30}
	conn, err := Spawn(context.Background(), ad, root, 30*time.Second)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer conn.Close()

	// 第一次 symbol 查询 (任意)
	_, err = conn.CallWithDoc(context.Background(), "textDocument/documentSymbol", file, nil, 20*time.Second)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	// 修改文件: 追加一个新函数
	extra := "\nfunc added() {}\n"
	fh, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString(extra); err != nil {
		t.Fatal(err)
	}
	fh.Close()
	// 第二次查询应看到 added (gopls 对 didOpen 的新内容即时索引)
	res, err := conn.CallWithDoc(context.Background(), "textDocument/documentSymbol", file, nil, 20*time.Second)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	s := string(res)
	t.Logf("symbols after update: %s", truncateStr(s, 300))
	if !strings.Contains(s, "added") {
		t.Error("short-connection model should see updated file content (didOpen reads disk each call)")
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
