package lsp_capacity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Porter-Key/axis/internal/config"
	"github.com/Porter-Key/axis/internal/lsp/client"
	"github.com/Porter-Key/axis/internal/lsp/positions"
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

// ---- 位置编码 (rust: 自包含决策 + positions 成熟实现) ----

// ServerEncoding rust-analyzer 的位置编码: utf-8 (LSP 3.17, 文件头注释)。
// 客户端口径同为 UTF-8 (见 ClientCharEncoding), positions 内走快路直通;
// 若 ServerEncoding 变更, 换算与响应适配自动跟随 (只改这里一处)。
func (p *rustProvider) ServerEncoding() string { return "utf-8" }

// ToServerChar 客户端列 → rust-analyzer 列。实现见 positions。
func (p *rustProvider) ToServerChar(lineText string, clientChar int) int {
	return positions.ToServer(p.ServerEncoding(), lineText, clientChar)
}

// FromServerChar rust-analyzer 列 → 客户端列。
func (p *rustProvider) FromServerChar(lineText string, serverChar int) int {
	return positions.FromServer(p.ServerEncoding(), lineText, serverChar)
}

// AdaptPositionsToClient rust-analyzer 位置响应 → 客户端单位。walker 见 positions.AdaptPositions。
func (p *rustProvider) AdaptPositionsToClient(raw json.RawMessage, srcPath string, readLine ReadLine) json.RawMessage {
	return positions.AdaptPositions(raw, srcPath, p.ServerEncoding(), readLine)
}

// ---- 智能 workflow (rust 自实现, 完全自包含) ----

// rustSimulateBudget rust-analyzer 合成 didChange 后诊断推送等待上限。
const rustSimulateBudget = 8 * time.Second

// rustWorkflowRefs 上限: 引用列表只带前 N 条 (路径+行), 总数另计 (防 blast_radius 爆炸)。
const rustWorkflowRefs = 30

// rustTestPath Go 测试文件判定 (*_test.go)。
// rustTestPath Rust 测试文件判定 (tests/ 目录或 _test.rs)。
func rustTestPath(path string) bool {
	return strings.Contains(path, "/tests/") || strings.HasSuffix(path, "_test.rs")
}

// rustDiagSig 诊断可比签名 (count + 消息序列, 供 simulate前后对比)。
func rustDiagSig(diag map[string]any) string {
	if diag == nil {
		return "nil"
	}
	b, _ := json.Marshal([]any{diag["count"], diag["diagnostics"]})
	return string(b)
}

// rustDiagItems 诊断精简条目 (取前 n 条的 severity/message/位置)。
func rustDiagItems(diag map[string]any, n int) []map[string]any {
	out := []map[string]any{}
	if diag == nil {
		return out
	}
	raw, _ := diag["diagnostics"].([]any)
	for i, it := range raw {
		if i >= n {
			break
		}
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		entry := map[string]any{}
		if v, ok := m["message"]; ok {
			entry["message"] = v
		}
		if v, ok := m["severity"]; ok {
			entry["severity"] = v
		}
		if r, ok := m["range"].(map[string]any); ok {
			entry["range"] = r
		}
		out = append(out, entry)
	}
	return out
}

// rustRefEntry 引用落点精简条目 (路径+行, 客户端单位)。
func rustRefEntry(loc LocationDTO) map[string]any {
	return map[string]any{"path": loc.Path, "line": loc.Range.StartLine, "character": loc.Range.StartCharacter}
}

// BlastRadius 影响面: 定义(+签名) + 全部引用(测试/非测试分区) + 诊断摘要。
func (p *rustProvider) BlastRadius(ctx context.Context, src WorkflowSource, path string, line, char int) (map[string]any, error) {
	out := map[string]any{"tool": "blast_radius", "language": "rust", "path": path, "line": line, "character": char}
	if raw, ok := src.Definition(ctx, path, line, char); ok {
		if locs, err := p.ParseLocations(raw); err == nil {
			for _, loc := range locs {
				if loc.FileLevel {
					continue
				}
				def := map[string]any{"path": loc.Path, "line": loc.Range.StartLine, "character": loc.Range.StartCharacter}
				if dto, ok2 := p.Signature(ctx, src, loc.Path, loc.Range.StartLine, loc.Range.StartCharacter); ok2 {
					def["symbol"] = dto.Symbol
					def["signature"] = dto.Signature
				}
				out["definition"] = def
				break
			}
			if _, has := out["definition"]; !has && len(locs) > 0 {
				out["definitionNote"] = "仅文件级跳转 (包/模块), 无符号签名意义"
			}
		} else {
			out["definition"] = "unavailable: 解析失败: " + err.Error()
		}
	} else {
		out["definition"] = "unavailable: definition 查询失败"
	}
	if raw, ok := src.References(ctx, path, line, char); ok {
		if locs, err := p.ParseLocations(raw); err == nil {
			var test, nontest []map[string]any
			for _, loc := range locs {
				e := rustRefEntry(loc)
				if rustTestPath(loc.Path) {
					test = append(test, e)
				} else {
					nontest = append(nontest, e)
				}
			}
			rustSortRefs(path, test)
			rustSortRefs(path, nontest)
			out["references"] = map[string]any{
				"total": len(locs), "testCount": len(test), "nonTestCount": len(nontest),
				"test": rustCapRefs(test), "nonTest": rustCapRefs(nontest),
				"truncated": len(test)+len(nontest) > 2*rustWorkflowRefs,
			}
		} else {
			out["references"] = "unavailable: 解析失败: " + err.Error()
		}
	} else {
		out["references"] = "unavailable: references 查询失败 (动手改代码前请用 get_references 复核)"
	}
	if diag, ok := src.Diagnostics(ctx, path); ok {
		out["diagnostics"] = map[string]any{"cached": true, "count": diag["count"], "items": rustDiagItems(diag, 10)}
	} else {
		out["diagnostics"] = map[string]any{"cached": false, "note": "尚无推送诊断 (不是零报错)"}
	}
	return out, nil
}

// rustCapRefs 引用列表截断 (各分区最多 rustWorkflowRefs 条)。
func rustCapRefs(in []map[string]any) []map[string]any {
	if len(in) > rustWorkflowRefs {
		return in[:rustWorkflowRefs]
	}
	if in == nil {
		return []map[string]any{}
	}
	return in
}

// ExploreSymbol 符号理解: hover 签名 + 定义落点 + 引用计数/前 N 条, 一把梭。
func (p *rustProvider) ExploreSymbol(ctx context.Context, src WorkflowSource, path string, line, char int) (map[string]any, error) {
	out := map[string]any{"tool": "explore_symbol", "language": "rust", "path": path, "line": line, "character": char}
	if md, ok := src.HoverMarkdown(ctx, path, line, char); ok {
		if dto, ok2 := p.ParseHover(md); ok2 {
			out["symbol"] = dto.Symbol
			out["signature"] = dto.Signature
			if dto.Doc != "" {
				out["doc"] = dto.Doc
			}
		} else {
			out["signature"] = "unavailable: hover 无可解析签名"
		}
	} else {
		out["signature"] = "unavailable: hover 查询失败"
	}
	if raw, ok := src.Definition(ctx, path, line, char); ok {
		if locs, err := p.ParseLocations(raw); err == nil && len(locs) > 0 {
			first := locs[0]
			out["definition"] = map[string]any{"path": first.Path, "line": first.Range.StartLine, "character": first.Range.StartCharacter, "fileLevel": first.FileLevel}
		}
	}
	if raw, ok := src.References(ctx, path, line, char); ok {
		if locs, err := p.ParseLocations(raw); err == nil {
			var first []map[string]any
			for i, loc := range locs {
				if i >= 10 {
					break
				}
				first = append(first, rustRefEntry(loc))
			}
			out["references"] = map[string]any{"total": len(locs), "first": first}
		}
	}
	return out, nil
}

// VerifyChain 修改后验证: 文件诊断 + go 构建/测试提示 (build/test 由 agent 跑)。
func (p *rustProvider) VerifyChain(ctx context.Context, src WorkflowSource, path string) (map[string]any, error) {
	out := map[string]any{"tool": "verify_chain", "language": "rust", "path": path,
		"buildHint": "cargo check", "testHint": "cargo test"}
	if diag, ok := src.Diagnostics(ctx, path); ok {
		out["diagnostics"] = map[string]any{"cached": true, "count": diag["count"], "items": rustDiagItems(diag, 20)}
	} else {
		out["diagnostics"] = map[string]any{"cached": false, "note": "尚无推送诊断: 先用 get_diagnostics 触发一次查询再验证"}
	}
	return out, nil
}

// rustPkgDir 文件所在包目录 (testHint 用)。
func rustPkgDir(path string) string {
	d := filepath.Dir(path)
	if d == "" {
		return "."
	}
	return d
}

// SimulateEdit 安全编辑预览: edits 作用于磁盘内容得合成文本 → PreviewText
// (不落盘) → 等诊断 → diff → RestoreFile。应用由 agent 落地。
func (p *rustProvider) SimulateEdit(ctx context.Context, src WorkflowSource, path string, edits []TextEdit) (map[string]any, error) {
	out := map[string]any{"tool": "simulate_edit", "language": "rust", "path": path, "edits": len(edits)}
	if len(edits) == 0 {
		return nil, fmt.Errorf("edits 为空")
	}
	disk, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	synthetic, err := rustApplyEdits(string(disk), edits)
	if err != nil {
		return nil, err
	}
	before, _ := src.Diagnostics(ctx, path)
	beforeSig := rustDiagSig(before)
	if err := src.PreviewText(ctx, path, synthetic); err != nil {
		return nil, fmt.Errorf("合成 didChange 失败: %w", err)
	}
	restored := false
	defer func() {
		if !restored {
			_ = src.RestoreFile(context.Background(), path)
		}
	}()
	after := before
	changed := false
	deadline := time.Now().Add(rustSimulateBudget)
	for {
		if d, ok := src.Diagnostics(ctx, path); ok {
			after = d
			if rustDiagSig(d) != beforeSig {
				changed = true
				break
			}
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	if err := src.RestoreFile(ctx, path); err != nil {
		return nil, fmt.Errorf("恢复磁盘内容失败: %w", err)
	}
	restored = true
	introduced, resolved := rustDiagDiff(before, after)
	out["before"] = map[string]any{"count": before["count"], "items": rustDiagItems(before, 20)}
	out["after"] = map[string]any{"count": after["count"], "items": rustDiagItems(after, 20)}
	out["errors_introduced"] = introduced
	out["errors_resolved"] = resolved
	out["net_delta"] = len(introduced) - len(resolved)
	out["restored"] = true
	// 置信度 (参照 agent-lsp preview_edit: 观测到新推送=high, 否则 low+原因)。
	// net_delta<=0 且 high → 可安全应用; 其他情况人工复核。
	if changed {
		out["confidence"] = "high"
		out["confidenceReason"] = "预览后观测到诊断推送变化"
	} else {
		out["confidence"] = "low"
		out["confidenceReason"] = "预算内诊断无变化 (服务器未推送或改动无新诊断)，结论仅供参考"
	}
	return out, nil
}

// rustApplyEdits 将 edits (LSP TextEdit 语义, 客户端列=行字节偏移) 作用于原文得合成文本。
// 行列越界/区间重叠直接报错 (不猜)。
func rustApplyEdits(orig string, edits []TextEdit) (string, error) {
	lines := strings.Split(orig, "\n")
	// 字节偏移表
	offs := make([]int, len(lines)+1)
	for i, l := range lines {
		offs[i+1] = offs[i] + len(l) + 1 // +1 换行
	}
	type span struct{ s, e int }
	var spans []span
	repls := make([]string, len(edits))
	for i, e := range edits {
		if e.StartLine < 0 || e.EndLine >= len(lines) || e.StartLine > e.EndLine {
			return "", fmt.Errorf("edit[%d] 行越界", i)
		}
		if e.StartChar < 0 || e.EndChar < 0 || e.StartChar > len(lines[e.StartLine]) || e.EndChar > len(lines[e.EndLine]) {
			return "", fmt.Errorf("edit[%d] 列越界 (UTF-8 字节偏移)", i)
		}
		s := offs[e.StartLine] + e.StartChar
		en := offs[e.EndLine] + e.EndChar
		if s > en {
			return "", fmt.Errorf("edit[%d] 起止倒置", i)
		}
		spans = append(spans, span{s, en})
		repls[i] = e.NewText
	}
	for i := 0; i < len(spans); i++ {
		for j := i + 1; j < len(spans); j++ {
			a, b := spans[i], spans[j]
			if a.s < b.e && b.s < a.e {
				return "", fmt.Errorf("edit[%d] 与 edit[%d] 重叠", i, j)
			}
		}
	}
	var b strings.Builder
	pos := 0
	order := make([]int, len(spans))
	for i := range order {
		order[i] = i
	}
	for i := 0; i < len(order); i++ {
		for j := i + 1; j < len(order); j++ {
			if spans[order[j]].s < spans[order[i]].s {
				order[i], order[j] = order[j], order[i]
			}
		}
	}
	for _, i := range order {
		b.WriteString(orig[pos:spans[i].s])
		b.WriteString(repls[i])
		pos = spans[i].e
	}
	b.WriteString(orig[pos:])
	return b.String(), nil
}

// 编译期断言: go provider 满足 workflow 契约。
var _ Workflows = (*rustProvider)(nil)

// rustSortRefs 引用排序: 同目录优先 → 路径浅优先 → 服务器原始顺序保持
// (截断时保重要引用, 非返回顺序)。
func rustSortRefs(queryPath string, in []map[string]any) {
	qd := filepath.Dir(queryPath)
	sort.SliceStable(in, func(a, b int) bool {
		pa, _ := in[a]["path"].(string)
		pb, _ := in[b]["path"].(string)
		sa := filepath.Dir(pa) == qd
		sb := filepath.Dir(pb) == qd
		if sa != sb {
			return sa
		}
		da := strings.Count(pa, string(filepath.Separator))
		db := strings.Count(pb, string(filepath.Separator))
		return da < db
	})
}

// rustDiagDiff 诊断差集 (before→after): introduced=新增消息, resolved=消除消息 (按 message 多重集差分)。
func rustDiagDiff(before, after map[string]any) (introduced, resolved []string) {
	counts := func(diag map[string]any) map[string]int {
		set := map[string]int{}
		if diag == nil {
			return set
		}
		raw, _ := diag["diagnostics"].([]any)
		for _, it := range raw {
			m, _ := it.(map[string]any)
			if m == nil {
				continue
			}
			msg, _ := m["message"].(string)
			set[msg]++
		}
		return set
	}
	b, a := counts(before), counts(after)
	introduced, resolved = []string{}, []string{}
	for msg, n := range a {
		for i := b[msg]; i < n; i++ {
			introduced = append(introduced, msg)
		}
	}
	for msg, n := range b {
		for i := a[msg]; i < n; i++ {
			resolved = append(resolved, msg)
		}
	}
	return introduced, resolved
}
