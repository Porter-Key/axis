package lsp_capacity

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Porter-Key/axis/internal/config"
	"github.com/Porter-Key/axis/internal/lsp/client"
	"github.com/Porter-Key/axis/internal/lsp/positions"
)

func init() {
	Register(&csharpProvider{})
}

// ---- csharpProvider: C# (csharp-ls / Roslyn) ----
//
// csharp-ls 独特事实:
//   - 官方 Roslyn language server 无 NuGet dotnet tool; 采用 csharp-ls (社区, 0.27.0)
//   - capabilities 平铺 (与 pyright 类似, OmniSharp 风格)
//   - 支持 implementation/typeDefinition (Roslyn 语义模型强)
//   - 无 sourcelink 的 NuGet 包: 跳转落到 dll/无源码 → hover 可能失败, 需降级 (见 handle_signature)
type csharpProvider struct{}

func (p *csharpProvider) LangID() LanguageID { return LangCSharp }

func (p *csharpProvider) MatchFile(path string) bool {
	return strings.HasSuffix(path, ".cs")
}

func (p *csharpProvider) Markers() []string { return []string{"*.csproj", "*.sln"} }

// Adapter csharp-ls 内置默认配置真相。
func (p *csharpProvider) Adapter() config.LangAdapter {
	return config.LangAdapter{
		LanguageID:   "csharp",
		Command:      "csharp-ls", // 真实绝对路径在 ~/.config/mcp/axis/config.yaml (代码默认不路径化)
		FilePatterns: []string{"**/*.cs"},
		Markers:      p.Markers(),
		TimeoutSec:   60, // csharp-ls 冷启动慢 (dotnet)
	}
}

func (p *csharpProvider) Spawn(ctx context.Context, root string, timeout time.Duration) (ProcessHandle, error) {
	conn, err := lspclient.Spawn(ctx, p.Adapter(), root, timeout)
	if err != nil {
		return nil, err
	}
	// csharp-ls 特化握手: 必须显式通知它加载 solution/project,
	// 否则它不自动发现 (solutionPathOverride=None 时会等通知而拖慢首个请求)。
	// 参照 Serena: solution/open (单个 URI) + project/open (URI 列表), 均为自定义 notification。
	if sln := findCSharpSolution(root); sln != "" {
		_ = conn.Notify("solution/open", map[string]any{"solution": pathToFileURI(sln)})
	}
	if projs := findCSharpProjects(root); len(projs) > 0 {
		uris := make([]string, 0, len(projs))
		for _, pj := range projs {
			uris = append(uris, pathToFileURI(pj))
		}
		_ = conn.Notify("project/open", map[string]any{"projects": uris})
	}
	return &csharpHandle{Conn: conn}, nil
}

// ---- csharp-ls 自定义通知辅助 (csharp provider 私有, 自包含) ----

// findCSharpSolution 在 root 下找第一个 .sln/.slnx (浅层优先)。
func findCSharpSolution(root string) string {
	found := ""
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() {
			// 跳过 .git/bin/obj 等
			n := d.Name()
			if n == ".git" || n == "bin" || n == "obj" || n == ".vs" || n == "packages" || n == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".slnx") || strings.HasSuffix(path, ".sln") {
			found = path
		}
		return nil
	})
	return found
}

// findCSharpProjects 在 root 下收集所有 .csproj。
func findCSharpProjects(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			n := d.Name()
			if n == ".git" || n == "bin" || n == "obj" || n == ".vs" || n == "packages" || n == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".csproj") {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// pathToFileURI 本地路径 → file:// URI (csharp provider 私有)。
func pathToFileURI(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	return "file://" + filepath.ToSlash(abs)
}

// csharpHandle: csharpProvider 私有进程句柄 (自包含)。
type csharpHandle struct{ *lspclient.Conn }

func (h *csharpHandle) Pid() int           { return h.ProcessPID() }
func (h *csharpHandle) IsAlive() bool      { return h.ProcessPID() > 0 }
func (h *csharpHandle) Kill() error        { return h.Close() }
func (h *csharpHandle) Touch()             {}
func (h *csharpHandle) LastUse() time.Time { return h.Conn.LastUse() }

var _ ProcessHandle = (*csharpHandle)(nil)

func (p *csharpProvider) Supports(cap Capability) bool {
	switch cap {
	case CapImplementation, CapTypeDefinition:
		return true // Roslyn 语义模型支持
	default:
		return true
	}
}

// ParseLocations csharp-ls 特化解析。
// csharp-ls (Roslyn) definition/implementation 返回 Location/LocationLink/数组;
// 特化注记: interface 实现跳转可能返回多处 impl, 无 sourcelink 的 NuGet 位置可能为空。
func (p *csharpProvider) ParseLocations(raw json.RawMessage) ([]LocationDTO, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		out := make([]LocationDTO, 0, len(arr))
		for _, item := range arr {
			if loc, ok := p.parseCSharpLocation(item); ok {
				out = append(out, loc)
			}
		}
		return out, nil
	}
	if loc, ok := p.parseCSharpLocation(raw); ok {
		return []LocationDTO{loc}, nil
	}
	return nil, nil
}

// csRawLoc csharp-ls 位置原始结构 (csharp 私有)。
type csRawLoc struct {
	URI         string      `json:"uri"`
	TargetURI   string      `json:"targetUri"`
	Range       *csRawRange `json:"range"`
	TargetRange *csRawRange `json:"targetRange"`
}

type csRawRange struct {
	Start struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"start"`
	End struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"end"`
}

func (p *csharpProvider) parseCSharpLocation(item json.RawMessage) (LocationDTO, bool) {
	var rl csRawLoc
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
		Path:      csURIToPath(uri),
		Range:     RangeDTO{StartLine: rg.Start.Line, StartCharacter: rg.Start.Character, EndLine: rg.End.Line, EndCharacter: rg.End.Character},
		FileLevel: csIsFileLevel(rg.Start.Line, rg.Start.Character, rg.End.Line, rg.End.Character),
		Raw:       rawMap,
	}, true
}

// csURIToPath URI → 本地路径 (csharp 私有)。csharp-ls 可能返回 windows 路径格式。
func csURIToPath(uri string) string {
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

// csIsFileLevel 文件级范围判断 (csharp 私有)。
func csIsFileLevel(sl, sc, el, ec int) bool {
	if el-sl > 20 {
		return true
	}
	if sl == 0 && sc == 0 && el >= 3 {
		return true
	}
	return false
}

func (p *csharpProvider) PositionParams(pos PositionDTO) map[string]any {
	return map[string]any{"line": pos.Line, "character": pos.Character}
}

// ---- 签名 hover 解析 (csharp-ls 特化) ----
//
// csharp-ls hover markdown 形态 (实测): 单 ```csharp 围栏, 内为完整成员签名
// (成员式, 显式接口/方法带返回类型):
//
//	```csharp
//	string IGreeter.Greet(string name)
//	```
//
// Signature 获取符号签名 (csharp 特化): hover 是唯一来源, 经 src 查询后自解析。
func (p *csharpProvider) Signature(ctx context.Context, src SignatureSource, path string, line, char int) (SignatureDTO, bool) {
	if src == nil {
		return SignatureDTO{}, false
	}
	md, ok := src.HoverMarkdown(ctx, path, line, char)
	if !ok {
		return SignatureDTO{}, false
	}
	return p.ParseHover(md)
}

// ParseHover 从 csharp-ls hover markdown 解析签名 (csharp 特化)。
// 特化: 取围栏内首行 (可能多行折行) 作签名; 无 docstring 时 doc 留空。
func (p *csharpProvider) ParseHover(markdown string) (SignatureDTO, bool) {
	sig, ok := csFenceSig(markdown)
	if !ok || sig == "" {
		return SignatureDTO{}, false
	}
	return SignatureDTO{
		Symbol:    csSymbolName(sig),
		Signature: sig,
		Source:    "hover",
	}, true
}

// csFenceSig 取首个 ```csharp 围栏内全部代码行作签名。
func csFenceSig(value string) (string, bool) {
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
	return strings.Join(code, "\n"), true
}

// csSymbolName 从 csharp 成员签名提取符号名 (末尾成员名, 如 string IGreeter.Greet( → Greet)。
func csSymbolName(sig string) string {
	first := sig
	if i := strings.IndexAny(first, "\n"); i >= 0 {
		first = first[:i]
	}
	s := strings.TrimSpace(first)
	// 找 "(" 前的最后一段: "string IGreeter.Greet" → 取 ".Greet" 或 " Greet"
	cut := s
	if i := strings.IndexAny(s, "(<{"); i > 0 {
		cut = s[:i]
	}
	cut = strings.TrimSpace(cut)
	if i := strings.LastIndex(cut, "."); i >= 0 {
		cut = cut[i+1:]
	}
	cut = strings.TrimSpace(cut)
	// 去返回类型部分 (空格分隔的最后 token 通常是方法名, 但 "string Greet" 中 Greet 是最后)
	fields := strings.Fields(cut)
	if len(fields) > 0 {
		return fields[len(fields)-1]
	}
	return cut
}

// ---- 位置编码 (csharp: 自包含决策 + positions 成熟实现) ----

// ServerEncoding csharp-ls 的位置编码: utf-16。
// csharp-ls 未声明 positionEncodings, 按 LSP 默认 utf-16 处理。若将来实测协商出 utf-8,
// 只改这里一处返回值, 换算与响应适配自动跟随。
func (p *csharpProvider) ServerEncoding() string { return "utf-16" }

// ToServerChar 客户端列 (UTF-8) → csharp-ls 列。实现见 positions。
func (p *csharpProvider) ToServerChar(lineText string, clientChar int) int {
	return positions.ToServer(p.ServerEncoding(), lineText, clientChar)
}

// FromServerChar csharp-ls 列 → 客户端列 (UTF-8)。
func (p *csharpProvider) FromServerChar(lineText string, serverChar int) int {
	return positions.FromServer(p.ServerEncoding(), lineText, serverChar)
}

// AdaptPositionsToClient csharp-ls 位置响应 → 客户端单位。walker 见 positions.AdaptPositions。
func (p *csharpProvider) AdaptPositionsToClient(raw json.RawMessage, srcPath string, readLine ReadLine) json.RawMessage {
	return positions.AdaptPositions(raw, srcPath, p.ServerEncoding(), readLine)
}

// ---- 智能 workflow (csharp 自实现, 完全自包含) ----

// csSimulateBudget csharp-ls 合成 didChange 后诊断推送等待上限 (冷启动慢, 预算放宽)。
const csSimulateBudget = 15 * time.Second

// csWorkflowRefs 上限: 引用列表只带前 N 条 (路径+行), 总数另计 (防 blast_radius 爆炸)。
const csWorkflowRefs = 30

// csTestPath Go 测试文件判定 (*_test.go)。
// csTestPath C# 测试文件判定 (tests//test/ 目录或文件名含 Test)。
func csTestPath(path string) bool {
	return strings.Contains(path, "/tests/") || strings.Contains(path, "/test/") ||
		strings.Contains(filepath.Base(path), "Test")
}

// csDiagSig 诊断可比签名 (count + 消息序列, 供 simulate前后对比)。
func csDiagSig(diag map[string]any) string {
	if diag == nil {
		return "nil"
	}
	b, _ := json.Marshal([]any{diag["count"], diag["diagnostics"]})
	return string(b)
}

// csDiagItems 诊断精简条目 (取前 n 条的 severity/message/位置)。
func csDiagItems(diag map[string]any, n int) []map[string]any {
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

// csRefEntry 引用落点精简条目 (路径+行, 客户端单位)。
func csRefEntry(loc LocationDTO) map[string]any {
	return map[string]any{"path": loc.Path, "line": loc.Range.StartLine, "character": loc.Range.StartCharacter}
}

// BlastRadius 影响面: 定义(+签名) + 全部引用(测试/非测试分区) + 诊断摘要。
func (p *csharpProvider) BlastRadius(ctx context.Context, src WorkflowSource, path string, line, char int) (map[string]any, error) {
	out := map[string]any{"tool": "blast_radius", "language": "csharp", "path": path, "line": line, "character": char}
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
				e := csRefEntry(loc)
				if csTestPath(loc.Path) {
					test = append(test, e)
				} else {
					nontest = append(nontest, e)
				}
			}
			out["references"] = map[string]any{
				"total": len(locs), "testCount": len(test), "nonTestCount": len(nontest),
				"test": csCapRefs(test), "nonTest": csCapRefs(nontest),
				"truncated": len(test)+len(nontest) > 2*csWorkflowRefs,
			}
		} else {
			out["references"] = "unavailable: 解析失败: " + err.Error()
		}
	} else {
		out["references"] = "unavailable: references 查询失败 (动手改代码前请用 get_references 复核)"
	}
	if diag, ok := src.Diagnostics(ctx, path); ok {
		out["diagnostics"] = map[string]any{"cached": true, "count": diag["count"], "items": csDiagItems(diag, 10)}
	} else {
		out["diagnostics"] = map[string]any{"cached": false, "note": "尚无推送诊断 (不是零报错)"}
	}
	return out, nil
}

// csCapRefs 引用列表截断 (各分区最多 csWorkflowRefs 条)。
func csCapRefs(in []map[string]any) []map[string]any {
	if len(in) > csWorkflowRefs {
		return in[:csWorkflowRefs]
	}
	if in == nil {
		return []map[string]any{}
	}
	return in
}

// ExploreSymbol 符号理解: hover 签名 + 定义落点 + 引用计数/前 N 条, 一把梭。
func (p *csharpProvider) ExploreSymbol(ctx context.Context, src WorkflowSource, path string, line, char int) (map[string]any, error) {
	out := map[string]any{"tool": "explore_symbol", "language": "csharp", "path": path, "line": line, "character": char}
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
				first = append(first, csRefEntry(loc))
			}
			out["references"] = map[string]any{"total": len(locs), "first": first}
		}
	}
	return out, nil
}

// VerifyChain 修改后验证: 文件诊断 + go 构建/测试提示 (build/test 由 agent 跑)。
func (p *csharpProvider) VerifyChain(ctx context.Context, src WorkflowSource, path string) (map[string]any, error) {
	out := map[string]any{"tool": "verify_chain", "language": "csharp", "path": path,
		"buildHint": "dotnet build", "testHint": "dotnet test"}
	if diag, ok := src.Diagnostics(ctx, path); ok {
		out["diagnostics"] = map[string]any{"cached": true, "count": diag["count"], "items": csDiagItems(diag, 20)}
	} else {
		out["diagnostics"] = map[string]any{"cached": false, "note": "尚无推送诊断: 先用 get_diagnostics 触发一次查询再验证"}
	}
	return out, nil
}

// csPkgDir 文件所在包目录 (testHint 用)。
func csPkgDir(path string) string {
	d := filepath.Dir(path)
	if d == "" {
		return "."
	}
	return d
}

// SimulateEdit 安全编辑预览: edits 作用于磁盘内容得合成文本 → PreviewText
// (不落盘) → 等诊断 → diff → RestoreFile。应用由 agent 落地。
func (p *csharpProvider) SimulateEdit(ctx context.Context, src WorkflowSource, path string, edits []TextEdit) (map[string]any, error) {
	out := map[string]any{"tool": "simulate_edit", "language": "csharp", "path": path, "edits": len(edits)}
	if len(edits) == 0 {
		return nil, fmt.Errorf("edits 为空")
	}
	disk, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	synthetic, err := csApplyEdits(string(disk), edits)
	if err != nil {
		return nil, err
	}
	before, _ := src.Diagnostics(ctx, path)
	beforeSig := csDiagSig(before)
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
	deadline := time.Now().Add(csSimulateBudget)
	for {
		if d, ok := src.Diagnostics(ctx, path); ok {
			after = d
			if csDiagSig(d) != beforeSig {
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
	out["before"] = map[string]any{"count": before["count"], "items": csDiagItems(before, 20)}
	out["after"] = map[string]any{"count": after["count"], "items": csDiagItems(after, 20)}
	out["restored"] = true
	if degraded != "" {
		out["note"] = degraded
	}
	return out, nil
}

// csApplyEdits 将 edits (LSP TextEdit 语义, 客户端列=行字节偏移) 作用于原文得合成文本。
// 行列越界/区间重叠直接报错 (不猜)。
func csApplyEdits(orig string, edits []TextEdit) (string, error) {
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
var _ Workflows = (*csharpProvider)(nil)
