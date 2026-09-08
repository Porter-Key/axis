package lsp_capacity

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/Porter-Key/axis/internal/config"
	"github.com/Porter-Key/axis/internal/lsp/client"
)

func init() {
	Register(&rustProvider{})
}

// ---- rustProvider: Rust (rust-analyzer) ----
//
// rust-analyzer 独特事实:
//   - capabilities 嵌套 textDocument.*
//   - 支持 implementation (trait impl 查询) / typeDefinition — 区别于 gopls
//   - 位置编码: 支持 utf-8 (LSP 3.17)
type rustProvider struct{}

func (p *rustProvider) LangID() LanguageID { return LangRust }

func (p *rustProvider) MatchFile(path string) bool {
	return strings.HasSuffix(path, ".rs")
}

func (p *rustProvider) Markers() []string { return []string{"Cargo.toml"} }

// Adapter rust-analyzer 内置默认配置真相。
func (p *rustProvider) Adapter() config.LangAdapter {
	return config.LangAdapter{
		LanguageID:   "rust",
		Command:      "rust-analyzer",
		FilePatterns: []string{"**/*.rs"},
		Markers:      p.Markers(),
		TimeoutSec:   30,
	}
}

func (p *rustProvider) Spawn(ctx context.Context, root string, timeout time.Duration) (ProcessHandle, error) {
	conn, err := lspclient.Spawn(ctx, p.Adapter(), root, timeout)
	if err != nil {
		return nil, err
	}
	return &rustHandle{Conn: conn}, nil
}

// rustHandle: rustProvider 私有进程句柄 (自包含)。
type rustHandle struct{ *lspclient.Conn }

func (h *rustHandle) Pid() int           { return h.ProcessPID() }
func (h *rustHandle) IsAlive() bool      { return h.ProcessPID() > 0 }
func (h *rustHandle) Kill() error        { return h.Close() }
func (h *rustHandle) Touch()             {}
func (h *rustHandle) LastUse() time.Time { return h.Conn.LastUse() }

var _ ProcessHandle = (*rustHandle)(nil)

func (p *rustProvider) Supports(cap Capability) bool {
	switch cap {
	case CapImplementation, CapTypeDefinition:
		return true // rust-analyzer 支持 trait impl / type def 跳转
	default:
		return true
	}
}

// ParseLocations rust-analyzer 特化解析。
// rust-analyzer implementation (trait impl) / definition / typeDefinition 返回
// Location/LocationLink/数组; 特化注记:
//   - trait impl 查询结果用 LocationLink {targetUri,targetRange} 指向 impl 块
//   - 跳到 trait 定义位置 (trait X {}) 时 range 只覆盖 trait 名 — 符号级, 可签名
func (p *rustProvider) ParseLocations(raw json.RawMessage) ([]LocationDTO, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		out := make([]LocationDTO, 0, len(arr))
		for _, item := range arr {
			if loc, ok := p.parseRustLocation(item); ok {
				out = append(out, loc)
			}
		}
		return out, nil
	}
	if loc, ok := p.parseRustLocation(raw); ok {
		return []LocationDTO{loc}, nil
	}
	return nil, nil
}

// rustRawLoc rust-analyzer 位置原始结构 (rust 私有)。
type rustRawLoc struct {
	URI         string        `json:"uri"`
	TargetURI   string        `json:"targetUri"`
	Range       *rustRawRange `json:"range"`
	TargetRange *rustRawRange `json:"targetRange"`
}

type rustRawRange struct {
	Start struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"start"`
	End struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"end"`
}

func (p *rustProvider) parseRustLocation(item json.RawMessage) (LocationDTO, bool) {
	var rl rustRawLoc
	if err := json.Unmarshal(item, &rl); err != nil {
		return LocationDTO{}, false
	}
	uri := rl.URI
	rg := rl.Range
	if uri == "" {
		uri = rl.TargetURI
		rg = rl.TargetRange
	}
	if uri == "" || rg == nil {
		return LocationDTO{}, false
	}
	var rawMap map[string]any
	_ = json.Unmarshal(item, &rawMap)
	return LocationDTO{
		URI:       uri,
		Path:      rustURIToPath(uri),
		Range:     RangeDTO{StartLine: rg.Start.Line, StartCharacter: rg.Start.Character, EndLine: rg.End.Line, EndCharacter: rg.End.Character},
		FileLevel: rustIsFileLevel(rg.Start.Line, rg.Start.Character, rg.End.Line, rg.End.Character),
		Raw:       rawMap,
	}, true
}

// rustURIToPath URI → 本地路径 (rust 私有)。
func rustURIToPath(uri string) string {
	if !strings.HasPrefix(uri, "file://") {
		return ""
	}
	pu := strings.TrimPrefix(uri, "file://")
	pu = strings.TrimPrefix(pu, "/")
	if !strings.HasPrefix(pu, "/") {
		pu = "/" + pu
	}
	return filepath.Clean(pu)
}

// rustIsFileLevel 文件级范围判断 (rust 私有)。
func rustIsFileLevel(sl, sc, el, ec int) bool {
	if el-sl > 20 {
		return true
	}
	if sl == 0 && sc == 0 && el >= 3 {
		return true
	}
	return false
}

func (p *rustProvider) PositionParams(pos PositionDTO) map[string]any {
	return map[string]any{"line": pos.Line, "character": pos.Character}
}

// ---- 签名 hover 解析 (rust-analyzer 特化) ----
//
// rust-analyzer hover markdown 形态 (实测): 多个 ```rust 围栏,
// 第一个常为符号全路径 (如 "multilang::Greeter") 或类型, 后面才是声明签名:
//
//	```rust
//	multilang::Greeter
//	```
//	```rust
//	trait Greeter
//	fn greet(&self, name: &str) -> String
//	```
//
// Signature 获取符号签名 (rust 特化): hover 是唯一来源, 经 src 查询后自解析。
func (p *rustProvider) Signature(ctx context.Context, src SignatureSource, path string, line, char int) (SignatureDTO, bool) {
	if src == nil {
		return SignatureDTO{}, false
	}
	md, ok := src.HoverMarkdown(ctx, path, line, char)
	if !ok {
		return SignatureDTO{}, false
	}
	return p.ParseHover(md)
}

// ParseHover 从 rust-analyzer hover markdown 解析签名 (rust 特化)。
// 特化: 遍历围栏, 选**含声明关键字** (fn/struct/trait/enum/impl/type/const/static/use)
// 的围栏作签名 (可能有多个围栏各含部分, 合并)。
func (p *rustProvider) ParseHover(markdown string) (SignatureDTO, bool) {
	sig, doc, ok := rustFenceSigDoc(markdown)
	if !ok || sig == "" {
		return SignatureDTO{}, false
	}
	return SignatureDTO{
		Symbol:    rustSymbolName(sig),
		Signature: sig,
		Doc:       doc,
		Source:    "hover",
	}, true
}

// rustDeclKeywords 声明签名特征关键字。
var rustDeclKeywords = []string{"fn ", "struct ", "trait ", "enum ", "impl ", "type ", "const ", "static ", "unsafe fn", "pub fn", "pub struct", "pub trait", "pub enum"}

// rustIsKeywordDoc 判断 hover markdown 是否为 rust 语言关键字文档 (impl/fn/struct 等
// 关键字的语言参考)。这类文档末尾常带完整示例代码 (如 "## Inherent Implementations"
// 用 struct Example 演示), 示例围栏会被 rustDeclKeywords 误选中 → 必须整体拒绝,
// 让调用方走降级/失败, 而不是把语言文档示例当符号签名。
func rustIsKeywordDoc(markdown string) bool {
	low := strings.ToLower(markdown)
	for _, marker := range []string{
		"the `impl` keyword",
		"the `fn` keyword",
		"the `struct` keyword",
		"the `trait` keyword",
		"the `enum` keyword",
		"the `type` keyword",
		"the `const` keyword",
		"the `use` keyword",
		"implementations of functionality for a type",
		"inherent implementations",
		"a function pointer",
		"anonymous function",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

func rustFenceSigDoc(value string) (sig, doc string, ok bool) {
	if rustIsKeywordDoc(value) {
		return "", "", false // 关键字语言文档: 不是符号签名
	}
	lines := strings.Split(value, "\n")
	inFence := false
	var fences [][]string
	var cur []string
	for _, l := range lines {
		tr := strings.TrimRight(l, "\r")
		if strings.HasPrefix(tr, "```") {
			if !inFence {
				inFence = true
				cur = nil
				continue
			}
			inFence = false
			if len(cur) > 0 {
				fences = append(fences, cur)
			}
			continue
		}
		if inFence {
			if strings.TrimSpace(tr) != "" {
				cur = append(cur, strings.TrimSpace(tr))
			}
			continue
		}
		// 围栏外非空文本进 doc (去 --- 分隔)
		if !strings.HasPrefix(tr, "---") && strings.TrimSpace(tr) != "" {
			if doc == "" {
				doc = strings.TrimSpace(tr)
			}
		}
	}
	// 选含声明关键字的围栏 (取第一个命中; 全部无命中用首个围栏)
	best := -1
	for i, f := range fences {
		joined := strings.Join(f, " ")
		for _, kw := range rustDeclKeywords {
			if strings.Contains(joined, kw) {
				best = i
				break
			}
		}
		if best >= 0 {
			break
		}
	}
	if best < 0 && len(fences) > 0 {
		best = 0
	}
	if best < 0 {
		return "", "", false
	}
	sig = strings.Join(fences[best], "\n")
	if len(doc) > 400 {
		doc = doc[:400] + "..."
	}
	return sig, doc, sig != ""
}

// rustSymbolName 从 rust 签名提取符号名。
func rustSymbolName(sig string) string {
	first := sig
	if i := strings.IndexAny(first, "\n"); i >= 0 {
		first = first[:i]
	}
	s := strings.TrimSpace(first)
	// trait Greeter / struct English / enum X / type Y = ... / fn greet / impl Greeter
	for _, kw := range []string{"unsafe fn ", "pub fn ", "fn ", "struct ", "trait ", "enum ", "type ", "const ", "static "} {
		if strings.HasPrefix(s, kw) {
			s = s[len(kw):]
			break
		}
	}
	// 去掉泛型/参数 (取首个分隔符前的标识符)
	if i := strings.IndexAny(s, "<("); i > 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
