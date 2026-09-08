package lsp_capacity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Porter-Key/axis/internal/config"
	"github.com/Porter-Key/axis/internal/lsp/client"
	"github.com/Porter-Key/axis/internal/lsp/positions"
)

func init() {
	Register(&javascriptProvider{})
}

// ---- javascriptProvider: JavaScript (typescript-language-server) ----
//
// 与 TS 同服务器, 但 file_patterns 不同 (js 系) + marker 为 package.json。
type javascriptProvider struct{}

func (p *javascriptProvider) LangID() LanguageID { return LangJavaScript }

func (p *javascriptProvider) MatchFile(path string) bool {
	for _, suf := range []string{".js", ".jsx", ".mjs", ".cjs"} {
		if strings.HasSuffix(path, suf) {
			return true
		}
	}
	return false
}

func (p *javascriptProvider) Markers() []string { return []string{"package.json"} }

// Adapter JS 内置默认配置真相 (与 TS 同服务器, 但 file_patterns/marker 不同)。
func (p *javascriptProvider) Adapter() config.LangAdapter {
	return config.LangAdapter{
		LanguageID:   "javascript",
		Command:      "typescript-language-server",
		Args:         []string{"--stdio"},
		FilePatterns: []string{"**/*.js", "**/*.jsx", "**/*.mjs", "**/*.cjs"},
		Markers:      p.Markers(),
		TimeoutSec:   30,
	}
}

func (p *javascriptProvider) Spawn(ctx context.Context, root string, timeout time.Duration) (ProcessHandle, error) {
	conn, err := lspclient.Spawn(ctx, p.Adapter(), root, timeout)
	if err != nil {
		return nil, err
	}
	return &jsHandle{Conn: conn}, nil
}

// jsHandle: javascriptProvider 私有进程句柄 (自包含)。
type jsHandle struct{ *lspclient.Conn }

func (h *jsHandle) Pid() int           { return h.ProcessPID() }
func (h *jsHandle) IsAlive() bool      { return h.ProcessPID() > 0 }
func (h *jsHandle) Kill() error        { return h.Close() }
func (h *jsHandle) Touch()             {}
func (h *jsHandle) LastUse() time.Time { return h.Conn.LastUse() }

var _ ProcessHandle = (*jsHandle)(nil)

func (p *javascriptProvider) Supports(cap Capability) bool {
	switch cap {
	case CapImplementation, CapTypeDefinition:
		return true
	default:
		return true
	}
}

// ParseLocations JS (tsserver) 特化解析。
// 与 TS 同服务器同结构; 特化文件独立以便未来 js 特有 (如 .d.ts / cjs 解析) 演进。
func (p *javascriptProvider) ParseLocations(raw json.RawMessage) ([]LocationDTO, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		out := make([]LocationDTO, 0, len(arr))
		for _, item := range arr {
			if loc, ok := p.parseJSLocation(item); ok {
				out = append(out, loc)
			}
		}
		return out, nil
	}
	if loc, ok := p.parseJSLocation(raw); ok {
		return []LocationDTO{loc}, nil
	}
	return nil, nil
}

// jsRawLoc tsserver 位置原始结构 (javascript 私有)。
type jsRawLoc struct {
	URI         string      `json:"uri"`
	TargetURI   string      `json:"targetUri"`
	Range       *jsRawRange `json:"range"`
	TargetRange *jsRawRange `json:"targetRange"`
}

type jsRawRange struct {
	Start struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"start"`
	End struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"end"`
}

func (p *javascriptProvider) parseJSLocation(item json.RawMessage) (LocationDTO, bool) {
	var rl jsRawLoc
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
		Path:      jsURIToPath(uri),
		Range:     RangeDTO{StartLine: rg.Start.Line, StartCharacter: rg.Start.Character, EndLine: rg.End.Line, EndCharacter: rg.End.Character},
		FileLevel: jsIsFileLevel(rg.Start.Line, rg.Start.Character, rg.End.Line, rg.End.Character),
		Raw:       rawMap,
	}, true
}

// jsURIToPath URI → 本地路径 (javascript 私有)。
func jsURIToPath(uri string) string {
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

// jsIsFileLevel 文件级范围判断 (javascript 私有)。
func jsIsFileLevel(sl, sc, el, ec int) bool {
	if el-sl > 20 {
		return true
	}
	if sl == 0 && sc == 0 && el >= 3 {
		return true
	}
	return false
}

func (p *javascriptProvider) PositionParams(pos PositionDTO) map[string]any {
	return map[string]any{"line": pos.Line, "character": pos.Character}
}

// Signature 获取符号签名 (javascript 特化): hover 是唯一来源, 经 src 查询后自解析。
func (p *javascriptProvider) Signature(ctx context.Context, src SignatureSource, path string, line, char int) (SignatureDTO, bool) {
	if src == nil {
		return SignatureDTO{}, false
	}
	md, ok := src.HoverMarkdown(ctx, path, line, char)
	if !ok {
		return SignatureDTO{}, false
	}
	return p.ParseHover(md)
}

// ---- 签名 hover 解析 (javascript 特化; 与 typescript 同源 tsserver, 自包含复制) ----
func (p *javascriptProvider) ParseHover(markdown string) (SignatureDTO, bool) {
	sig, ok := jsFenceSig(markdown)
	if !ok || sig == "" {
		return SignatureDTO{}, false
	}
	return SignatureDTO{
		Symbol:    jsSymbolName(sig),
		Signature: sig,
		Source:    "hover",
	}, true
}

// jsFenceSig 取首个 ```javascript 围栏内代码行, 去 "(method)"/"(function)" 等前缀。
func jsFenceSig(value string) (string, bool) {
	lines := strings.Split(value, "\n")
	inFence := false
	var code []string
	for _, l := range lines {
		tr := strings.TrimRight(l, "\r")
		if strings.HasPrefix(tr, "```") {
			if !inFence {
				inFence = true
				continue
			}
			inFence = false
			continue
		}
		if inFence && strings.TrimSpace(tr) != "" {
			code = append(code, strings.TrimSpace(tr))
		}
	}
	if len(code) == 0 {
		return "", false
	}
	sig := strings.Join(code, "\n")
	if strings.HasPrefix(sig, "(") {
		if i := strings.Index(sig, ") "); i >= 0 && i < 20 {
			sig = strings.TrimSpace(sig[i+2:])
		}
	}
	return sig, true
}

// jsSymbolName 从 js 签名提取符号名。
func jsSymbolName(sig string) string {
	first := sig
	if i := strings.IndexAny(first, "\n"); i >= 0 {
		first = first[:i]
	}
	s := strings.TrimSpace(first)
	if strings.HasPrefix(s, "class ") {
		s = strings.TrimSpace(s[6:])
	}
	cut := s
	if i := strings.IndexAny(cut, "(<:"); i > 0 {
		cut = cut[:i]
	}
	cut = strings.TrimSpace(cut)
	if i := strings.LastIndex(cut, "."); i >= 0 {
		cut = cut[i+1:]
	}
	cut = strings.TrimSpace(cut)
	fields := strings.Fields(cut)
	if len(fields) > 0 {
		return fields[len(fields)-1]
	}
	return cut
}

// ---- 位置编码 (javascript: 自包含决策 + positions 成熟实现) ----

// ServerEncoding JS (tsserver) 的位置编码: utf-16 (tsserver 只认 utf-16)。
func (p *javascriptProvider) ServerEncoding() string { return "utf-16" }

// ToServerChar 客户端列 (UTF-8) → tsserver 列。实现见 positions。
func (p *javascriptProvider) ToServerChar(lineText string, clientChar int) int {
	return positions.ToServer(p.ServerEncoding(), lineText, clientChar)
}

// FromServerChar tsserver 列 → 客户端列 (UTF-8)。
func (p *javascriptProvider) FromServerChar(lineText string, serverChar int) int {
	return positions.FromServer(p.ServerEncoding(), lineText, serverChar)
}

// AdaptPositionsToClient tsserver 位置响应 → 客户端单位。walker 见 positions.AdaptPositions。
func (p *javascriptProvider) AdaptPositionsToClient(raw json.RawMessage, srcPath string, readLine ReadLine) json.RawMessage {
	return positions.AdaptPositions(raw, srcPath, p.ServerEncoding(), readLine)
}

// ---- 智能 workflow (javascript 自实现, 完全自包含) ----

// jsSimulateBudget typescript-language-server 合成 didChange 后诊断推送等待上限。
const jsSimulateBudget = 8 * time.Second

// jsWorkflowRefs 上限: 引用列表只带前 N 条 (路径+行), 总数另计 (防 blast_radius 爆炸)。
const jsWorkflowRefs = 30

// jsTestPath Go 测试文件判定 (*_test.go)。
// jsTestPath JavaScript 测试文件判定 (*.test.*/*.spec.*/__tests__/)。
func jsTestPath(path string) bool {
	for _, s := range []string{".test.js", ".test.jsx", ".test.mjs", ".test.cjs", ".spec.js", ".spec.jsx", "/__tests__/"} {
		if strings.Contains(path, s) {
			return true
		}
	}
	return false
}

// jsDiagSig 诊断可比签名 (count + 消息序列, 供 simulate前后对比)。
func jsDiagSig(diag map[string]any) string {
	if diag == nil {
		return "nil"
	}
	b, _ := json.Marshal([]any{diag["count"], diag["diagnostics"]})
	return string(b)
}

// jsDiagItems 诊断精简条目 (取前 n 条的 severity/message/位置)。
func jsDiagItems(diag map[string]any, n int) []map[string]any {
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

// jsRefEntry 引用落点精简条目 (路径+行, 客户端单位)。
func jsRefEntry(loc LocationDTO) map[string]any {
	return map[string]any{"path": loc.Path, "line": loc.Range.StartLine, "character": loc.Range.StartCharacter}
}

// BlastRadius 影响面: 定义(+签名) + 全部引用(测试/非测试分区) + 诊断摘要。
func (p *javascriptProvider) BlastRadius(ctx context.Context, src WorkflowSource, path string, line, char int) (map[string]any, error) {
	out := map[string]any{"tool": "blast_radius", "language": "javascript", "path": path, "line": line, "character": char}
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
				e := jsRefEntry(loc)
				if jsTestPath(loc.Path) {
					test = append(test, e)
				} else {
					nontest = append(nontest, e)
				}
			}
			out["references"] = map[string]any{
				"total": len(locs), "testCount": len(test), "nonTestCount": len(nontest),
				"test": jsCapRefs(test), "nonTest": jsCapRefs(nontest),
				"truncated": len(test)+len(nontest) > 2*jsWorkflowRefs,
			}
		} else {
			out["references"] = "unavailable: 解析失败: " + err.Error()
		}
	} else {
		out["references"] = "unavailable: references 查询失败 (动手改代码前请用 get_references 复核)"
	}
	if diag, ok := src.Diagnostics(ctx, path); ok {
		out["diagnostics"] = map[string]any{"cached": true, "count": diag["count"], "items": jsDiagItems(diag, 10)}
	} else {
		out["diagnostics"] = map[string]any{"cached": false, "note": "尚无推送诊断 (不是零报错)"}
	}
	return out, nil
}

// jsCapRefs 引用列表截断 (各分区最多 jsWorkflowRefs 条)。
func jsCapRefs(in []map[string]any) []map[string]any {
	if len(in) > jsWorkflowRefs {
		return in[:jsWorkflowRefs]
	}
	if in == nil {
		return []map[string]any{}
	}
	return in
}

// ExploreSymbol 符号理解: hover 签名 + 定义落点 + 引用计数/前 N 条, 一把梭。
func (p *javascriptProvider) ExploreSymbol(ctx context.Context, src WorkflowSource, path string, line, char int) (map[string]any, error) {
	out := map[string]any{"tool": "explore_symbol", "language": "javascript", "path": path, "line": line, "character": char}
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
				first = append(first, jsRefEntry(loc))
			}
			out["references"] = map[string]any{"total": len(locs), "first": first}
		}
	}
	return out, nil
}

// VerifyChain 修改后验证: 文件诊断 + go 构建/测试提示 (build/test 由 agent 跑)。
func (p *javascriptProvider) VerifyChain(ctx context.Context, src WorkflowSource, path string) (map[string]any, error) {
	out := map[string]any{"tool": "verify_chain", "language": "javascript", "path": path,
		"buildHint": "node --check " + path, "testHint": "npm test"}
	if diag, ok := src.Diagnostics(ctx, path); ok {
		out["diagnostics"] = map[string]any{"cached": true, "count": diag["count"], "items": jsDiagItems(diag, 20)}
	} else {
		out["diagnostics"] = map[string]any{"cached": false, "note": "尚无推送诊断: 先用 get_diagnostics 触发一次查询再验证"}
	}
	return out, nil
}

// jsPkgDir 文件所在包目录 (testHint 用)。
func jsPkgDir(path string) string {
	d := filepath.Dir(path)
	if d == "" {
		return "."
	}
	return d
}

// SimulateEdit 安全编辑预览: edits 作用于磁盘内容得合成文本 → PreviewText
// (不落盘) → 等诊断 → diff → RestoreFile。应用由 agent 落地。
func (p *javascriptProvider) SimulateEdit(ctx context.Context, src WorkflowSource, path string, edits []TextEdit) (map[string]any, error) {
	out := map[string]any{"tool": "simulate_edit", "language": "javascript", "path": path, "edits": len(edits)}
	if len(edits) == 0 {
		return nil, fmt.Errorf("edits 为空")
	}
	disk, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	synthetic, err := jsApplyEdits(string(disk), edits)
	if err != nil {
		return nil, err
	}
	before, _ := src.Diagnostics(ctx, path)
	beforeSig := jsDiagSig(before)
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
	degraded := ""
	deadline := time.Now().Add(jsSimulateBudget)
	for {
		if d, ok := src.Diagnostics(ctx, path); ok {
			after = d
			if jsDiagSig(d) != beforeSig {
				break
			}
		}
		if time.Now().After(deadline) {
			degraded = "诊断在预算内无变化 (服务器未推送或改动无新诊断), diff 可能为空"
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
	out["before"] = map[string]any{"count": before["count"], "items": jsDiagItems(before, 20)}
	out["after"] = map[string]any{"count": after["count"], "items": jsDiagItems(after, 20)}
	out["restored"] = true
	if degraded != "" {
		out["note"] = degraded
	}
	return out, nil
}

// jsApplyEdits 将 edits (LSP TextEdit 语义, 客户端列=行字节偏移) 作用于原文得合成文本。
// 行列越界/区间重叠直接报错 (不猜)。
func jsApplyEdits(orig string, edits []TextEdit) (string, error) {
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
var _ Workflows = (*javascriptProvider)(nil)
