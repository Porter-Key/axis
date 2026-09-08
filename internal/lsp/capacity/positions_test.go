package lsp_capacity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// 各家 ServerEncoding 声明值 (须与实际协商结果一致, 见各文件注释)。
func TestServerEncodingValues(t *testing.T) {
	cases := map[Provider]string{
		&goProvider{}:         "utf-16", // gopls v0.23.0 未声明, LSP 默认
		&rustProvider{}:       "utf-8",  // rust-analyzer 声明 utf-8
		&pythonProvider{}:     "utf-16",
		&typescriptProvider{}: "utf-16",
		&javascriptProvider{}: "utf-16",
		&csharpProvider{}:     "utf-16",
	}
	for p, want := range cases {
		if got := p.ServerEncoding(); got != want {
			t.Errorf("%T ServerEncoding = %q, want %q", p, got, want)
		}
	}
}

// 委托冒烟: go (utf-16 服务器) 中日韩行双向换算经 positions 生效。
// 行 "a中b": 字节 a0 中1..3 b4; 单元 a0 中1 b2。
func TestGoDelegation_CJK(t *testing.T) {
	p := &goProvider{}
	if got := p.ToServerChar("a中b", 4); got != 2 {
		t.Errorf("ToServerChar(4) = %d, want 2", got)
	}
	if got := p.FromServerChar("a中b", 1); got != 1 {
		t.Errorf("FromServerChar(1) = %d, want 1", got)
	}
	if got := p.FromServerChar("a中b", 2); got != 4 {
		t.Errorf("FromServerChar(2) = %d, want 4", got)
	}
}

// 委托冒烟: rust (utf-8, 与客户端同单位) 直通。
func TestRustDelegation_Passthrough(t *testing.T) {
	p := &rustProvider{}
	if got := p.ToServerChar("a中b", 3); got != 3 {
		t.Errorf("rust ToServerChar must passthrough: %d", got)
	}
	raw := json.RawMessage(`{"uri":"file:///x.rs","range":{"start":{"line":0,"character":3},"end":{"line":0,"character":4}}}`)
	rl := func(path string, line int) (string, bool) { return "", false }
	if got := string(p.AdaptPositionsToClient(raw, "/src", rl)); got != string(raw) {
		t.Errorf("rust Adapt must passthrough: got %q", got)
	}
}

// 委托冒烟: go Adapt 对 Location 数组真换算 (行 "fn 主() {}": 单元7'{'→字节9)。
func TestGoAdapt_LocationArray(t *testing.T) {
	p := &goProvider{}
	fp := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(fp, []byte("fn 主() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	uri := "file://" + fp
	raw := json.RawMessage(`[{"uri":` + strconv.Quote(uri) + `,"range":{"start":{"line":0,"character":3},"end":{"line":0,"character":7}}}]`)
	rl := func(path string, line int) (string, bool) {
		if path != fp || line != 0 {
			return "", false
		}
		return "fn 主() {}", true
	}
	out := p.AdaptPositionsToClient(raw, "/src", rl)
	var arr []struct {
		Range struct {
			Start struct {
				Character int `json:"character"`
			} `json:"start"`
			End struct {
				Character int `json:"character"`
			} `json:"end"`
		} `json:"range"`
	}
	if err := json.Unmarshal(out, &arr); err != nil || len(arr) != 1 {
		t.Fatalf("shape broken: %s", string(out))
	}
	// 单元3(主)→字节3; 单元7('{')→字节9
	if arr[0].Range.Start.Character != 3 || arr[0].Range.End.Character != 9 {
		t.Errorf("chars = %d,%d; want 3,9 (raw %s)",
			arr[0].Range.Start.Character, arr[0].Range.End.Character, string(out))
	}
}
