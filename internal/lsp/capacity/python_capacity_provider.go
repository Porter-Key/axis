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
	Register(&pythonProvider{})
}

// ---- pythonProvider: Python (pyright-langserver) ----
//
// pyright 独特事实 (本机实测 v1.1.411):
//   - capabilities 平铺在顶层 (无 textDocument 嵌套): typeDefinitionProvider 等直接在 result.capabilities
//   - initialize result: {capabilities:{...}} (caps 包在 capabilities 键下)
//   - 支持 typeDefinition/declaration, 不支持 implementation
//   - 顶层有 codeActionProvider (codeActionKinds: quickfix/source.organizeImports)
//   - 位置编码: 不支持 utf-8 声明 → 默认 utf-16
type pythonProvider struct{}

func (p *pythonProvider) LangID() LanguageID { return LangPython }

func (p *pythonProvider) MatchFile(path string) bool {
	return strings.HasSuffix(path, ".py")
}

func (p *pythonProvider) Markers() []string {
	return []string{"pyproject.toml", "requirements.txt", "setup.py", "Pipfile"}
}

// Adapter pyright 内置默认配置真相。
func (p *pythonProvider) Adapter() config.LangAdapter {
	return config.LangAdapter{
		LanguageID:   "python",
		Command:      "pyright-langserver",
		Args:         []string{"--stdio"},
		FilePatterns: []string{"**/*.py"},
		Markers:      p.Markers(),
		TimeoutSec:   30,
	}
}

func (p *pythonProvider) Spawn(ctx context.Context, root string, timeout time.Duration) (ProcessHandle, error) {
	conn, err := lspclient.Spawn(ctx, p.Adapter(), root, timeout)
	if err != nil {
		return nil, err
	}
	return &pythonHandle{Conn: conn}, nil
}

// pythonHandle: pythonProvider 私有进程句柄 (自包含)。
type pythonHandle struct{ *lspclient.Conn }

func (h *pythonHandle) Pid() int           { return h.ProcessPID() }
func (h *pythonHandle) IsAlive() bool      { return h.ProcessPID() > 0 }
func (h *pythonHandle) Kill() error        { return h.Close() }
func (h *pythonHandle) Touch()             {}
func (h *pythonHandle) LastUse() time.Time { return h.Conn.LastUse() }

var _ ProcessHandle = (*pythonHandle)(nil)

// Supports pyright: 平铺 caps, 支持 typeDefinition, 不支持 implementation。
func (p *pythonProvider) Supports(cap Capability) bool {
	switch cap {
	case CapImplementation:
		return false
	case CapTypeDefinition:
		return true
	default:
		return true
	}
}

// ParseLocations pyright 特化解析。
// pyright definition/typeDefinition 返回标准 Location/LocationLink/数组。
// 特化注记: pyright 对 import 语句跳转返回模块文件级位置 (range 覆盖整个文件);
//
//	typeshed 位置也可能出现 (site-packages 内) — 不在此过滤, 由上层决定。
func (p *pythonProvider) ParseLocations(raw json.RawMessage) ([]LocationDTO, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		out := make([]LocationDTO, 0, len(arr))
		for _, item := range arr {
			if loc, ok := p.parsePythonLocation(item); ok {
				out = append(out, loc)
			}
		}
		return out, nil
	}
	if loc, ok := p.parsePythonLocation(raw); ok {
		return []LocationDTO{loc}, nil
	}
	return nil, nil
}

// pyRawLoc pyright 位置原始结构 (Location/LocationLink 同构, python 私有)。
type pyRawLoc struct {
	URI         string      `json:"uri"`
	TargetURI   string      `json:"targetUri"`
	Range       *pyRawRange `json:"range"`
	TargetRange *pyRawRange `json:"targetRange"`
}

type pyRawRange struct {
	Start struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"start"`
	End struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"end"`
}

func (p *pythonProvider) parsePythonLocation(item json.RawMessage) (LocationDTO, bool) {
	var rl pyRawLoc
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
		Path:      pythonURIToPath(uri),
		Range:     RangeDTO{StartLine: rg.Start.Line, StartCharacter: rg.Start.Character, EndLine: rg.End.Line, EndCharacter: rg.End.Character},
		FileLevel: pythonIsFileLevel(rg.Start.Line, rg.Start.Character, rg.End.Line, rg.End.Character),
		Raw:       rawMap,
	}, true
}

// pythonURIToPath pyright URI → 本地路径 (python 私有)。
func pythonURIToPath(uri string) string {
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

// pythonIsFileLevel 文件级范围判断 (python 私有)。
func pythonIsFileLevel(sl, sc, el, ec int) bool {
	if el-sl > 20 {
		return true
	}
	if sl == 0 && sc == 0 && el >= 3 {
		return true
	}
	return false
}

// PositionParams pyright 默认 utf-16 (与 LSP 默认一致, 直接透传)。
func (p *pythonProvider) PositionParams(pos PositionDTO) map[string]any {
	return map[string]any{"line": pos.Line, "character": pos.Character}
}

// Signature 获取符号签名 (python 特化): hover 是唯一来源, 经 src 查询后自解析。
func (p *pythonProvider) Signature(ctx context.Context, src SignatureSource, path string, line, char int) (SignatureDTO, bool) {
	if src == nil {
		return SignatureDTO{}, false
	}
	md, ok := src.HoverMarkdown(ctx, path, line, char)
	if !ok {
		return SignatureDTO{}, false
	}
	return p.ParseHover(md)
}

// ParseHover 从 pyright hover markdown 解析签名 (python 特化)。
//
// pyright hover markdown 形态 (实测): ```python 围栏内为完整签名, 但**多行**:
//
//	(function) def make_greeter(
//	    prefix: str = "Hello",
//	    *,
//	    loud: bool = False
//	) -> ((name: str) -> str)
//
// 围栏后 "---" 分隔, 其后为 docstring。
// 特化: 围栏内全部行 = 签名 (去 "(function)"/"(method)" 等类型前缀; 保留多行结构)。
func (p *pythonProvider) ParseHover(markdown string) (SignatureDTO, bool) {
	sig, doc, ok := pythonFenceSigDoc(markdown)
	if !ok || sig == "" {
		return SignatureDTO{}, false
	}
	return SignatureDTO{
		Symbol:    pythonSymbolName(sig),
		Signature: sig,
		Doc:       doc,
		Source:    "hover",
	}, true
}

// pythonFenceSigDoc 取首个 ```python 围栏内全部行作签名, --- 后 doc。
func pythonFenceSigDoc(value string) (sig, doc string, ok bool) {
	lines := strings.Split(value, "\n")
	inFence := false
	var code, docLines []string
	for _, l := range lines {
		tr := strings.TrimRight(l, "\r")
		if strings.HasPrefix(tr, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			code = append(code, tr)
			continue
		}
		if strings.HasPrefix(tr, "---") {
			continue
		}
		if strings.TrimSpace(tr) != "" {
			docLines = append(docLines, strings.TrimSpace(tr))
		}
	}
	if len(code) == 0 {
		return "", "", false
	}
	// 合并: 保留换行的完整签名 (trim 头尾空行)
	sig = strings.TrimSpace(strings.Join(code, "\n"))
	// 去 "(function)"/"(method)" 前缀 (pyright 类型标注)
	sig = pythonStripPrefix(sig)
	if len(docLines) > 0 {
		doc = docLines[0]
		if len(doc) > 400 {
			doc = doc[:400] + "..."
		}
	}
	return sig, doc, sig != ""
}

// pythonStripPrefix 去 pyright 前缀 "(function)"/"(method)"/"(property)" 等。
func pythonStripPrefix(sig string) string {
	// 形态: "(function) def make_greeter(" → "def make_greeter("
	// 也有 "(method) def run(...)" 或 "class Foo" (无前缀)
	if strings.HasPrefix(sig, "(") {
		if i := strings.Index(sig, ") "); i >= 0 && i < 30 {
			sig = strings.TrimSpace(sig[i+2:])
		}
	}
	return sig
}

// pythonSymbolName 从签名提取符号名。
func pythonSymbolName(sig string) string {
	s := strings.TrimSpace(sig)
	// def name(...) → name; class Name(...) → Name
	if i := strings.Index(s, "def "); i >= 0 {
		s = s[i+4:]
	} else if i := strings.Index(s, "class "); i >= 0 {
		s = s[i+6:]
	} else if i := strings.Index(s, "async def "); i >= 0 {
		s = s[i+10:]
	}
	if i := strings.IndexAny(s, "(:"); i > 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// ---- 位置编码 (python: 自包含决策 + positions 成熟实现) ----

// ServerEncoding pyright 的位置编码: utf-16 (LSP 默认, pyright 不声明 utf-8)。
func (p *pythonProvider) ServerEncoding() string { return "utf-16" }

// ToServerChar 客户端列 (UTF-8) → pyright 列。实现见 positions。
func (p *pythonProvider) ToServerChar(lineText string, clientChar int) int {
	return positions.ToServer(p.ServerEncoding(), lineText, clientChar)
}

// FromServerChar pyright 列 → 客户端列 (UTF-8)。
func (p *pythonProvider) FromServerChar(lineText string, serverChar int) int {
	return positions.FromServer(p.ServerEncoding(), lineText, serverChar)
}

// AdaptPositionsToClient pyright 位置响应 → 客户端单位。walker 见 positions.AdaptPositions。
func (p *pythonProvider) AdaptPositionsToClient(raw json.RawMessage, srcPath string, readLine ReadLine) json.RawMessage {
	return positions.AdaptPositions(raw, srcPath, p.ServerEncoding(), readLine)
}

// ---- 智能 workflow (python 自实现, 完全自包含) ----

// pythonSimulateBudget pyright 合成 didChange 后诊断推送等待上限。
const pythonSimulateBudget = 8 * time.Second

// pythonWorkflowRefs 上限: 引用列表只带前 N 条 (路径+行), 总数另计 (防 blast_radius 爆炸)。
const pythonWorkflowRefs = 30

// pythonTestPath Go 测试文件判定 (*_test.go)。
// pythonTestPath Python 测试文件判定 (test_*.py/*_test.py/tests//test/)。
func pythonTestPath(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") ||
		strings.Contains(path, "/tests/") || strings.Contains(path, "/test/")
}

// pythonDiagSig 诊断可比签名 (count + 消息序列, 供 simulate前后对比)。
func pythonDiagSig(diag map[string]any) string {
	if diag == nil {
		return "nil"
	}
	b, _ := json.Marshal([]any{diag["count"], diag["diagnostics"]})
	return string(b)
}

// pythonDiagItems 诊断精简条目 (取前 n 条的 severity/message/位置)。
func pythonDiagItems(diag map[string]any, n int) []map[string]any {
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

// pythonRefEntry 引用落点精简条目 (路径+行, 客户端单位)。
func pythonRefEntry(loc LocationDTO) map[string]any {
	return map[string]any{"path": loc.Path, "line": loc.Range.StartLine, "character": loc.Range.StartCharacter}
}

// BlastRadius 影响面: 定义(+签名) + 全部引用(测试/非测试分区) + 诊断摘要。
func (p *pythonProvider) BlastRadius(ctx context.Context, src WorkflowSource, path string, line, char int) (map[string]any, error) {
	out := map[string]any{"tool": "blast_radius", "language": "python", "path": path, "line": line, "character": char}
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
				e := pythonRefEntry(loc)
				if pythonTestPath(loc.Path) {
					test = append(test, e)
				} else {
					nontest = append(nontest, e)
				}
			}
			pythonSortRefs(path, test)
			pythonSortRefs(path, nontest)
			out["references"] = map[string]any{
				"total": len(locs), "testCount": len(test), "nonTestCount": len(nontest),
				"test": pythonCapRefs(test), "nonTest": pythonCapRefs(nontest),
				"truncated": len(test)+len(nontest) > 2*pythonWorkflowRefs,
			}
		} else {
			out["references"] = "unavailable: 解析失败: " + err.Error()
		}
	} else {
		out["references"] = "unavailable: references 查询失败 (动手改代码前请用 get_references 复核)"
	}
	if diag, ok := src.Diagnostics(ctx, path); ok {
		out["diagnostics"] = map[string]any{"cached": true, "count": diag["count"], "items": pythonDiagItems(diag, 10)}
	} else {
		out["diagnostics"] = map[string]any{"cached": false, "note": "尚无推送诊断 (不是零报错)"}
	}
	return out, nil
}

// pythonCapRefs 引用列表截断 (各分区最多 pythonWorkflowRefs 条)。
func pythonCapRefs(in []map[string]any) []map[string]any {
	if len(in) > pythonWorkflowRefs {
		return in[:pythonWorkflowRefs]
	}
	if in == nil {
		return []map[string]any{}
	}
	return in
}

// ExploreSymbol 符号理解: hover 签名 + 定义落点 + 引用计数/前 N 条, 一把梭。
func (p *pythonProvider) ExploreSymbol(ctx context.Context, src WorkflowSource, path string, line, char int) (map[string]any, error) {
	out := map[string]any{"tool": "explore_symbol", "language": "python", "path": path, "line": line, "character": char}
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
				first = append(first, pythonRefEntry(loc))
			}
			out["references"] = map[string]any{"total": len(locs), "first": first}
		}
	}
	return out, nil
}

// VerifyChain 修改后验证: 文件诊断 + go 构建/测试提示 (build/test 由 agent 跑)。
func (p *pythonProvider) VerifyChain(ctx context.Context, src WorkflowSource, path string) (map[string]any, error) {
	out := map[string]any{"tool": "verify_chain", "language": "python", "path": path,
		"buildHint": "python -m py_compile " + path, "testHint": "pytest " + pythonPkgDir(path)}
	if diag, ok := src.Diagnostics(ctx, path); ok {
		out["diagnostics"] = map[string]any{"cached": true, "count": diag["count"], "items": pythonDiagItems(diag, 20)}
	} else {
		out["diagnostics"] = map[string]any{"cached": false, "note": "尚无推送诊断: 先用 get_diagnostics 触发一次查询再验证"}
	}
	return out, nil
}

// pythonPkgDir 文件所在包目录 (testHint 用)。
func pythonPkgDir(path string) string {
	d := filepath.Dir(path)
	if d == "" {
		return "."
	}
	return d
}

// SimulateEdit 安全编辑预览: edits 作用于磁盘内容得合成文本 → PreviewText
// (不落盘) → 等诊断 → diff → RestoreFile。应用由 agent 落地。
func (p *pythonProvider) SimulateEdit(ctx context.Context, src WorkflowSource, path string, edits []TextEdit) (map[string]any, error) {
	out := map[string]any{"tool": "simulate_edit", "language": "python", "path": path, "edits": len(edits)}
	if len(edits) == 0 {
		return nil, fmt.Errorf("edits 为空")
	}
	disk, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	synthetic, err := pythonApplyEdits(string(disk), edits)
	if err != nil {
		return nil, err
	}
	before, _ := src.Diagnostics(ctx, path)
	beforeSig := pythonDiagSig(before)
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
	deadline := time.Now().Add(pythonSimulateBudget)
	for {
		if d, ok := src.Diagnostics(ctx, path); ok {
			after = d
			if pythonDiagSig(d) != beforeSig {
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
	introduced, resolved := pythonDiagDiff(before, after)
	out["before"] = map[string]any{"count": before["count"], "items": pythonDiagItems(before, 20)}
	out["after"] = map[string]any{"count": after["count"], "items": pythonDiagItems(after, 20)}
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

// pythonApplyEdits 将 edits (LSP TextEdit 语义, 客户端列=行字节偏移) 作用于原文得合成文本。
// 行列越界/区间重叠直接报错 (不猜)。
func pythonApplyEdits(orig string, edits []TextEdit) (string, error) {
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
var _ Workflows = (*pythonProvider)(nil)

// pythonSortRefs 引用排序: 同目录优先 → 路径浅优先 → 服务器原始顺序保持
// (截断时保重要引用, 非返回顺序)。
func pythonSortRefs(queryPath string, in []map[string]any) {
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

// pythonDiagDiff 诊断差集 (before→after): introduced=新增消息, resolved=消除消息 (按 message 多重集差分)。
func pythonDiagDiff(before, after map[string]any) (introduced, resolved []string) {
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
