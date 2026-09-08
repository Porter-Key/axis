package positions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// 样本行 "a中😀b": 字节 a0 中1..3 😀4..7 b8 (长 9);
// UTF-16 单元 a0 中1 😀2..3 b4 (长 5); rune 序号 a0 中1 😀2 b3 (长 4)。

func TestToServer_UTF16(t *testing.T) {
	line := "a中😀b"
	cases := []struct{ client, server int }{
		{0, 0}, {1, 1}, {2, 2}, {3, 2}, {4, 2}, // 中间字节钳字符尾
		{5, 4}, {6, 4}, {7, 4}, {8, 4}, // 😀 中间字节钳尾
		{9, 5}, {99, 5}, {-1, 0},
	}
	for _, c := range cases {
		if got := ToServer(UTF16, line, c.client); got != c.server {
			t.Errorf("ToServer(utf-16,%d) = %d, want %d", c.client, got, c.server)
		}
	}
}

func TestFromServer_UTF16(t *testing.T) {
	line := "a中😀b"
	cases := []struct{ server, client int }{
		{0, 0}, {1, 1}, {2, 4}, {3, 8}, {4, 8}, {5, 9}, {99, 9}, {-1, 0},
	}
	for _, c := range cases {
		if got := FromServer(UTF16, line, c.server); got != c.client {
			t.Errorf("FromServer(utf-16,%d) = %d, want %d", c.server, got, c.client)
		}
	}
}

func TestUTF32(t *testing.T) {
	line := "a中😀b"
	if got := ToServer(UTF32, line, 4); got != 2 {
		t.Errorf("ToServer(utf-32,4) = %d, want 2", got)
	}
	if got := FromServer(UTF32, line, 2); got != 4 {
		t.Errorf("FromServer(utf-32,2) = %d, want 4", got)
	}
	if got := FromServer(UTF32, line, 3); got != 8 {
		t.Errorf("FromServer(utf-32,3) = %d, want 8", got)
	}
}

func TestUTF8IdentityAndClamp(t *testing.T) {
	if got := ToServer(UTF8, "a中b", 99); got != 5 {
		t.Errorf("utf-8 clamp: %d, want 5", got)
	}
	if got := FromServer(UTF8, "a中b", 2); got != 2 {
		t.Errorf("utf-8 identity: %d, want 2", got)
	}
}

func TestUnknownEncodingPassthrough(t *testing.T) {
	if got := ToServer("utf- Klingon", "a中", 3); got != 3 {
		t.Errorf("unknown enc must passthrough: %d", got)
	}
	if got := FromServer("", "a中", 3); got != 3 {
		t.Errorf("empty enc must passthrough: %d", got)
	}
}

func TestCRLFAndEdge(t *testing.T) {
	// 行尾 \r 不计入列
	if got := ToServer(UTF16, "ab\r", 5); got != 2 {
		t.Errorf("CRLF clamp: %d, want 2", got)
	}
	if got := FromServer(UTF16, "ab\r", 5); got != 2 {
		t.Errorf("CRLF from: %d, want 2", got)
	}
	// 空行
	if got := ToServer(UTF16, "", 3); got != 0 {
		t.Errorf("empty line: %d, want 0", got)
	}
	// 纯 ASCII 恒等
	if got := FromServer(UTF16, "hello", 3); got != 3 {
		t.Errorf("ascii: %d, want 3", got)
	}
}

func TestInvalidUTF8NoHang(t *testing.T) {
	line := "a\xffb" // 0xFF 非法字节
	if got := ToServer(UTF16, line, 2); got != 2 {
		t.Errorf("invalid byte to: %d, want 2", got)
	}
	if got := FromServer(UTF16, line, 2); got != 2 {
		t.Errorf("invalid byte from: %d, want 2", got)
	}
}

// ---- AdaptPositions 形状 ----

func adaptTestFile(t *testing.T, name, content string) (uri string, rl ReadLine) {
	t.Helper()
	fp := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	rl = func(path string, line int) (string, bool) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", false
		}
		lines := splitTestLines(string(data))
		if line < 0 || line >= len(lines) {
			return "", false
		}
		return lines[line], true
	}
	return "file://" + fp, rl
}

func splitTestLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func TestAdapt_LocationArray(t *testing.T) {
	// "fn 主() {}": 字节 f0 n1 空2 主3..5 (6 )7; 单元 f0 n1 空2 主3 (4
	uri, rl := adaptTestFile(t, "lib.rs", "fn 主() {}\n")
	raw := json.RawMessage(`[{"uri":` + strconv.Quote(uri) + `,"range":{"start":{"line":0,"character":3},"end":{"line":0,"character":7}}}]`)
	out := AdaptPositions(raw, "/src", UTF16, rl)
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

func TestAdapt_LocationLinkAndSymbols(t *testing.T) {
	uri, rl := adaptTestFile(t, "a.rs", "fn 主() {}\n")
	q := strconv.Quote(uri)
	// LocationLink: targetRange 用 targetUri 文件, originSelectionRange 用源文件
	raw := json.RawMessage(`{"targetUri":` + q + `,"targetRange":{"start":{"line":0,"character":3},"end":{"line":0,"character":3}},"originSelectionRange":{"start":{"line":0,"character":3},"end":{"line":0,"character":3}}}`)
	out := AdaptPositions(raw, "/src", UTF16, rl)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	tr := m["targetRange"].(map[string]any)
	if int(tr["start"].(map[string]any)["character"].(float64)) != 3 {
		t.Errorf("targetRange not converted: %s", string(out))
	}
	// DocumentSymbol 树 (无 uri, 归属 srcPath)
	raw2 := json.RawMessage(`{"name":"f","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"selectionRange":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"children":[{"name":"g","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"selectionRange":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}]}`)
	fp := uri[len("file://"):]
	out2 := AdaptPositions(raw2, fp, UTF16, rl)
	var d map[string]any
	if err := json.Unmarshal(out2, &d); err != nil {
		t.Fatal(err)
	}
	kids := d["children"].([]any)
	kr := kids[0].(map[string]any)["range"].(map[string]any)
	if int(kr["end"].(map[string]any)["character"].(float64)) != 1 {
		t.Errorf("children not converted: %s", string(out2))
	}
}

func TestAdapt_WorkspaceEdit(t *testing.T) {
	uri, rl := adaptTestFile(t, "b.rs", "ab中cd\n")
	q := strconv.Quote(uri)
	// changes: {uri: edits[]}; "ab中cd": 单元2(中)→字节2? 验证 edits 走了 uri 分支
	raw := json.RawMessage(`{"changes":{` + q + `:[{"range":{"start":{"line":0,"character":2},"end":{"line":0,"character":3}},"newText":"x"}]}}`)
	out := AdaptPositions(raw, "/src", UTF16, rl)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	edits := m["changes"].(map[string]any)[uri].([]any)
	rng := edits[0].(map[string]any)["range"].(map[string]any)
	// 单元2(中)→字节2; 单元3(c)→字节5
	if int(rng["start"].(map[string]any)["character"].(float64)) != 2 ||
		int(rng["end"].(map[string]any)["character"].(float64)) != 5 {
		t.Errorf("edits not converted: %s", string(out))
	}
	// documentChanges: TextDocumentEdit
	raw2 := json.RawMessage(`{"documentChanges":[{"textDocument":{"uri":` + q + `},"edits":[{"range":{"start":{"line":0,"character":3},"end":{"line":0,"character":3}}}]}]}`)
	out2 := AdaptPositions(raw2, "/src", UTF16, rl)
	var m2 map[string]any
	if err := json.Unmarshal(out2, &m2); err != nil {
		t.Fatal(err)
	}
	dc := m2["documentChanges"].([]any)
	ed2 := dc[0].(map[string]any)["edits"].([]any)
	r2 := ed2[0].(map[string]any)["range"].(map[string]any)
	if int(r2["start"].(map[string]any)["character"].(float64)) != 5 {
		t.Errorf("documentChanges not converted: %s", string(out2))
	}
}

func TestAdapt_Passthrough(t *testing.T) {
	rl := func(path string, line int) (string, bool) { return "", false }
	// utf-8 快路: 原样 (含 key 顺序)
	raw := `{"uri":"file:///x.rs","range":{"start":{"line":0,"character":5},"end":{"line":0,"character":6}}}`
	if got := string(AdaptPositions(json.RawMessage(raw), "/src", UTF8, rl)); got != raw {
		t.Errorf("utf-8 fast path must passthrough: got %q", got)
	}
	for _, r := range []string{`null`, ``, `{"foo":1}`, `[1,2]`} {
		if got := string(AdaptPositions(json.RawMessage(r), "/src", UTF16, rl)); got != r {
			t.Errorf("passthrough broken for %q: got %q", r, got)
		}
	}
	// 读不到行 → 值原样 (语义比对, Go map 序列化重排 key)
	raw2 := `{"uri":"file:///nope.rs","range":{"start":{"line":0,"character":5},"end":{"line":0,"character":6}}}`
	var want, got any
	_ = json.Unmarshal([]byte(raw2), &want)
	if err := json.Unmarshal(AdaptPositions(json.RawMessage(raw2), "/src", UTF16, rl), &got); err != nil {
		t.Fatal(err)
	}
	if string(mustJSON(want)) != string(mustJSON(got)) {
		t.Errorf("missing-file should passthrough values: got %v", got)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
