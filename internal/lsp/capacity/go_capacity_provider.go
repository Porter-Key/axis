package lsp_capacity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Porter-Key/axis/internal/config"
	"github.com/Porter-Key/axis/internal/lsp/client"
	"github.com/Porter-Key/axis/internal/lsp/positions"
)

func init() { Register(&goProvider{}) }

// goProvider: Go (gopls) — 完全自包含。
//
// gopls 独特事实 (本机实测 v0.23.0, 2026-09-07 探针复测):
//   - capabilities 平铺在顶层 (非 textDocument 嵌套!): definitionProvider /
//     implementationProvider / typeDefinitionProvider / hoverProvider 等直接顶层 bool。
//     (早期误判"嵌套 textDocument.*"且"不支持 implementation" — 实为查错 key 路径,
//     顶层就是 definitionProvider 等; 与 pyright 同为平铺结构。)
//   - 支持 implementation/typeDefinition (顶层 bool true)
//   - 位置编码: 未在 general.positionEncodings 声明 (LSP 默认 utf-16)
//   - definition 对 import 包名跳转返回整个包所有文件级位置 (FileLevel)
type goProvider struct{}

func (p *goProvider) LangID() LanguageID { return LangGo }

func (p *goProvider) MatchFile(path string) bool {
	return strings.HasSuffix(path, ".go")
}

func (p *goProvider) Markers() []string { return []string{"go.mod", "go.work"} }

// Adapter gopls 内置默认配置真相。
func (p *goProvider) Adapter() config.LangAdapter {
	return config.LangAdapter{
		LanguageID:   "go",
		Command:      "gopls",
		FilePatterns: []string{"**/*.go"},
		Markers:      []string{"go.mod", "go.work"},
		TimeoutSec:   30,
	}
}

func (p *goProvider) Supports(cap Capability) bool {
	// 顶层平铺 caps: implementation/typeDefinition 均为 true (实测 v0.23.0)。
	return true
}

// ---- 进程生命周期 (自包含适配 lspclient.Conn) ----

func (p *goProvider) Spawn(ctx context.Context, root string, timeout time.Duration) (ProcessHandle, error) {
	conn, err := lspclient.Spawn(ctx, p.Adapter(), root, timeout)
	if err != nil {
		return nil, err
	}
	return &goHandle{Conn: conn}, nil
}

// goHandle: goProvider 私有进程句柄 (自包含, 不共享)。
type goHandle struct{ *lspclient.Conn }

func (h *goHandle) Pid() int           { return h.ProcessPID() }
func (h *goHandle) IsAlive() bool      { return h.ProcessPID() > 0 }
func (h *goHandle) Kill() error        { return h.Close() }
func (h *goHandle) Touch()             {}
func (h *goHandle) LastUse() time.Time { return h.Conn.LastUse() }

var _ ProcessHandle = (*goHandle)(nil)

// ---- DTO 特化解析 (自包含: 私有 raw 类型 + uri 转换 + 文件级判断) ----

// goRawLoc gopls 位置原始结构 (Location/LocationLink 同构)。
type goRawLoc struct {
	URI         string      `json:"uri"`
	TargetURI   string      `json:"targetUri"`
	Range       *goRawRange `json:"range"`
	TargetRange *goRawRange `json:"targetRange"`
}

type goRawRange struct {
	Start struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"start"`
	End struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"end"`
}

func (p *goProvider) ParseLocations(raw json.RawMessage) ([]LocationDTO, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		out := make([]LocationDTO, 0, len(arr))
		for _, item := range arr {
			if loc, ok := p.parseGoLocation(item); ok {
				out = append(out, loc)
			}
		}
		return out, nil
	}
	if loc, ok := p.parseGoLocation(raw); ok {
		return []LocationDTO{loc}, nil
	}
	return nil, nil
}

func (p *goProvider) parseGoLocation(item json.RawMessage) (LocationDTO, bool) {
	var rl goRawLoc
	if err := json.Unmarshal(item, &rl); err != nil {
		return LocationDTO{}, false
	}
	uri, rg := rl.URI, rl.Range
	if uri == "" {
		uri, rg = rl.TargetURI, rl.TargetRange
	}
	if uri == "" || rg == nil {
		return LocationDTO{}, false
	}
	var rawMap map[string]any
	_ = json.Unmarshal(item, &rawMap)
	return LocationDTO{
		URI:       uri,
		Path:      goURIToPath(uri),
		Range:     RangeDTO{StartLine: rg.Start.Line, StartCharacter: rg.Start.Character, EndLine: rg.End.Line, EndCharacter: rg.End.Character},
		FileLevel: goIsFileLevel(rg.Start.Line, rg.Start.Character, rg.End.Line, rg.End.Character),
		Raw:       rawMap,
	}, true
}

// goURIToPath gopls 位置 URI → 本地路径 (goProvider 私有)。
func goURIToPath(uri string) string {
	if !strings.HasPrefix(uri, "file://") {
		return ""
	}
	p := strings.TrimPrefix(uri, "file://")
	p = strings.TrimPrefix(p, "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return filepath.Clean(p)
}

// goIsFileLevel gopls 文件级范围判断 (goProvider 私有)。
func goIsFileLevel(sl, sc, el, ec int) bool {
	if el-sl > 20 {
		return true
	}
	if sl == 0 && sc == 0 && el >= 3 {
		return true
	}
	return false
}

func (p *goProvider) PositionParams(pos PositionDTO) map[string]any {
	return map[string]any{"line": pos.Line, "character": pos.Character}
}

// ---- 签名 (go 特化: 标准化入口, 全链决策私有) ----

// Signature 获取符号签名。策略 (go provider 私有决策):
//  1. 先经 src 发起 hover, 解析 ```go 围栏签名;
//  2. hover 无结果/无签名且文件在 module cache (外部依赖) → 自行 exec `go doc`
//     解析纯文本 decl (gopls hover 对 module cache 有时无上下文)。
func (p *goProvider) Signature(ctx context.Context, src SignatureSource, path string, line, char int) (SignatureDTO, bool) {
	if src != nil {
		if md, ok := src.HoverMarkdown(ctx, path, line, char); ok {
			if dto, ok2 := p.ParseHover(md); ok2 {
				return dto, true
			}
		}
	}
	if goIsModuleCache(path) {
		if sig, doc, ok := goDocDeclSignature(ctx, path); ok {
			sym := goParseSymbolName(sig)
			return SignatureDTO{Symbol: sym, Signature: sig, Doc: doc, Source: "go-doc"}, true
		}
		return SignatureDTO{Source: "go-doc", Note: "go doc 未能解析外部依赖签名"}, false
	}
	return SignatureDTO{}, false
}

// ParseHover 从 gopls hover markdown 解析签名 (go 特化)。
// gopls hover 形态 (实测): 单个 ```go 代码围栏 = 完整签名 (通常一行),
// "---" 分隔后为文档 (可能再带 pkg.go.dev 链接)。
func (p *goProvider) ParseHover(markdown string) (SignatureDTO, bool) {
	sig, doc, ok := goSplitFenceSigDoc(markdown)
	if !ok || sig == "" {
		return SignatureDTO{}, false
	}
	return SignatureDTO{
		Symbol:    goParseSymbolName(sig),
		Signature: sig,
		Doc:       doc,
		Source:    "hover",
	}, true
}

// goSplitFenceSigDoc 取首个 ``` 围栏内全部代码行作签名, "---" 后首段作 doc。
func goSplitFenceSigDoc(value string) (sig, doc string, ok bool) {
	lines := strings.Split(value, "\n")
	inFence := false
	var code []string
	docLines := []string{}
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
		if inFence {
			if strings.TrimSpace(tr) != "" {
				code = append(code, strings.TrimRight(tr, " \t"))
			}
			continue
		}
		if strings.HasPrefix(tr, "---") {
			continue // 分隔线不进 doc
		}
		if strings.TrimSpace(tr) != "" {
			docLines = append(docLines, strings.TrimSpace(tr))
		}
	}
	if len(code) == 0 {
		return "", "", false
	}
	sig = strings.Join(code, "\n")
	if len(docLines) > 0 {
		doc = docLines[0]
		if len(doc) > 400 {
			doc = doc[:400] + "..."
		}
	}
	return sig, doc, true
}

// goParseSymbolName 从 go 签名提取符号名 (func (r X) Name( → Name)。
func goParseSymbolName(sig string) string {
	s := strings.TrimSpace(sig)
	// 多行签名 (gopls hover 类型+方法列表): 主声明在首行 → 先切首行再解析,
	// 避免全文搜 func/type 误命中后续方法行 (如 type X struct{}\nfunc (X) M() 会误取 M)。
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	isType := false
	switch {
	case strings.HasPrefix(s, "func "):
		s = s[len("func "):]
	case strings.HasPrefix(s, "type "):
		s = s[len("type "):]
		isType = true
	default:
		// 容错: 允许前导短噪音 (注释/反引号等, <10 字符) 后跟声明关键字
		if i := strings.Index(s, "func "); i >= 0 && i < 10 {
			s = s[i+len("func "):]
		} else if i := strings.Index(s, "type "); i >= 0 && i < 10 {
			s = s[i+len("type "):]
			isType = true
		}
	}
	if strings.HasPrefix(s, "(") { // 方法接收者: (x X) Name(
		if i := strings.Index(s, ") "); i >= 0 {
			s = s[i+2:]
		}
	}
	if isType {
		// type Greeter interface/struct → Greeter (首个空白前)
		if i := strings.IndexAny(s, " \t{"); i > 0 {
			s = s[:i]
		}
	} else if i := strings.IndexAny(s, "([{"); i > 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t") {
		return "" // 提取失败由调用方 fallback
	}
	return s
}

// ---- go doc 降级 (go provider 私有: module cache 语义与 decl 解析全在此) ----

// goIsModuleCache 判断路径是否 Go module cache (~/go/pkg/mod/...)。
func goIsModuleCache(path string) bool {
	return strings.Contains(path, "/pkg/mod/")
}

// goDocDeclSignature 对 module cache 文件反推 import 路径, exec `go doc` 取首 decl 段签名。
func goDocDeclSignature(ctx context.Context, path string) (sig, doc string, ok bool) {
	imp := goModuleImportPath(path)
	if imp == "" {
		return "", "", false
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cmdCtx, "go", "doc", imp).CombinedOutput()
	if err != nil {
		return "", "", false
	}
	decl := goFirstDeclBlock(string(out))
	if decl == "" {
		return "", "", false
	}
	sig, doc = goSplitPlainDecl(decl)
	if sig == "" {
		return "", "", false
	}
	return sig, doc, true
}

// goModuleImportPath 从 module cache 文件路径反推 import 路径。
// ~/go/pkg/mod/<mod>@<ver>/<sub...>/x.go → <mod>/<sub...>
func goModuleImportPath(path string) string {
	idx := strings.Index(path, "/pkg/mod/")
	if idx < 0 {
		return ""
	}
	rest := path[idx+len("/pkg/mod/"):]
	at := strings.Index(rest, "@")
	if at <= 0 {
		return ""
	}
	modPart := rest[:at]
	afterVer := rest[at+1:]
	slash := strings.Index(afterVer, "/")
	if slash < 0 {
		sub := filepath.Dir(afterVer)
		if sub == "." || sub == "/" || sub == "" {
			return modPart
		}
		return modPart + "/" + sub
	}
	sub := afterVer[slash+1:]
	sub = filepath.ToSlash(filepath.Dir(sub))
	sub = strings.Trim(sub, "/")
	if sub == "." || sub == "" {
		return modPart
	}
	return modPart + "/" + sub
}

// goFirstDeclBlock 从 go doc 输出提取首个 decl 段 (func/type/var 声明 + 后续注释行)。
func goFirstDeclBlock(docOut string) string {
	lines := strings.Split(docOut, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "func ") || strings.HasPrefix(l, "type ") || strings.HasPrefix(l, "var ") || strings.HasPrefix(l, "const ") {
			start = i
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" && i+1 < len(lines) {
			next := strings.TrimSpace(lines[i+1])
			if strings.HasPrefix(next, "func ") || strings.HasPrefix(next, "type ") || strings.HasPrefix(next, "var ") || strings.HasPrefix(next, "const ") {
				end = i
				break
			}
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// goSplitPlainDecl 从纯文本 decl 分离签名与 doc (go doc 输出无 markdown 围栏):
// 首个 func/type 行为签名, 其后空行/注释行为 doc。
func goSplitPlainDecl(decl string) (sig, doc string) {
	lines := strings.Split(decl, "\n")
	sig = ""
	rest := []string{}
	for i, l := range lines {
		tr := strings.TrimSpace(l)
		if i == 0 {
			sig = strings.TrimSpace(l)
			continue
		}
		if tr != "" && !strings.HasPrefix(tr, "//") {
			// 多行签名续行 (参数跨行) — go doc 通常单行; 保守: 全部并入 sig
			sig += " " + tr
			continue
		}
		if strings.HasPrefix(tr, "//") {
			rest = append(rest, strings.TrimSpace(strings.TrimPrefix(tr, "//")))
		}
	}
	doc = strings.TrimSpace(strings.Join(rest, "\n"))
	if len(doc) > 400 {
		doc = doc[:400] + "..."
	}
	return sig, doc
}

// ---- 位置编码 (go: 自包含决策 + positions 成熟实现) ----

// ServerEncoding gopls 的位置编码: utf-16。
// 本机实测 gopls v0.23.0 未在 general.positionEncodings 声明 (LSP 默认 utf-16)。
// 若 gopls 未来声明 utf-8, 只改这里一处返回值, 换算与响应适配自动跟随。
func (p *goProvider) ServerEncoding() string { return "utf-16" }

// ToServerChar 客户端列 (UTF-8) → gopls 列。实现见 positions (全仓库唯一实现, 全面单测)。
func (p *goProvider) ToServerChar(lineText string, clientChar int) int {
	return positions.ToServer(p.ServerEncoding(), lineText, clientChar)
}

// FromServerChar gopls 列 → 客户端列 (UTF-8)。
func (p *goProvider) FromServerChar(lineText string, serverChar int) int {
	return positions.FromServer(p.ServerEncoding(), lineText, serverChar)
}

// AdaptPositionsToClient gopls 位置响应 → 客户端单位。walker 见 positions.AdaptPositions。
func (p *goProvider) AdaptPositionsToClient(raw json.RawMessage, srcPath string, readLine ReadLine) json.RawMessage {
	return positions.AdaptPositions(raw, srcPath, p.ServerEncoding(), readLine)
}

// ---- 智能 workflow (go 自实现, 完全自包含) ----

// goSimulateBudget gopls 合成 didChange 后诊断推送等待上限 (实测常 <2s)。
const goSimulateBudget = 5 * time.Second

// goWorkflowRefs 上限: 引用列表只带前 N 条 (路径+行), 总数另计 (防 blast_radius 爆炸)。
const goWorkflowRefs = 30

// goTestPath Go 测试文件判定 (*_test.go)。
func goTestPath(path string) bool { return strings.HasSuffix(path, "_test.go") }

// goDiagSig 诊断可比签名 (count + 消息序列, 供 simulate前后对比)。
func goDiagSig(diag map[string]any) string {
	if diag == nil {
		return "nil"
	}
	b, _ := json.Marshal([]any{diag["count"], diag["diagnostics"]})
	return string(b)
}

// goDiagItems 诊断精简条目 (取前 n 条的 severity/message/位置)。
func goDiagItems(diag map[string]any, n int) []map[string]any {
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

// goRefEntry 引用落点精简条目 (路径+行, 客户端单位)。
func goRefEntry(loc LocationDTO) map[string]any {
	return map[string]any{"path": loc.Path, "line": loc.Range.StartLine, "character": loc.Range.StartCharacter}
}

// BlastRadius 影响面: 定义(+签名) + 全部引用(测试/非测试分区) + 诊断摘要。
func (p *goProvider) BlastRadius(ctx context.Context, src WorkflowSource, path string, line, char int) (map[string]any, error) {
	out := map[string]any{"tool": "blast_radius", "language": "go", "path": path, "line": line, "character": char}
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
				e := goRefEntry(loc)
				if goTestPath(loc.Path) {
					test = append(test, e)
				} else {
					nontest = append(nontest, e)
				}
			}
			goSortRefs(path, test)
			goSortRefs(path, nontest)
			out["references"] = map[string]any{
				"total": len(locs), "testCount": len(test), "nonTestCount": len(nontest),
				"test": goCapRefs(test), "nonTest": goCapRefs(nontest),
				"truncated": len(test)+len(nontest) > 2*goWorkflowRefs,
			}
		} else {
			out["references"] = "unavailable: 解析失败: " + err.Error()
		}
	} else {
		out["references"] = "unavailable: references 查询失败 (动手改代码前请用 get_references 复核)"
	}
	if diag, ok := src.Diagnostics(ctx, path); ok {
		out["diagnostics"] = map[string]any{"cached": true, "count": diag["count"], "items": goDiagItems(diag, 10)}
	} else {
		out["diagnostics"] = map[string]any{"cached": false, "note": "尚无推送诊断 (不是零报错)"}
	}
	return out, nil
}

// goCapRefs 引用列表截断 (各分区最多 goWorkflowRefs 条)。
func goCapRefs(in []map[string]any) []map[string]any {
	if len(in) > goWorkflowRefs {
		return in[:goWorkflowRefs]
	}
	if in == nil {
		return []map[string]any{}
	}
	return in
}

// ExploreSymbol 符号理解: hover 签名 + 定义落点 + 引用计数/前 N 条, 一把梭。
func (p *goProvider) ExploreSymbol(ctx context.Context, src WorkflowSource, path string, line, char int) (map[string]any, error) {
	out := map[string]any{"tool": "explore_symbol", "language": "go", "path": path, "line": line, "character": char}
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
				first = append(first, goRefEntry(loc))
			}
			out["references"] = map[string]any{"total": len(locs), "first": first}
		}
	}
	return out, nil
}

// VerifyChain 修改后验证: 文件诊断 + go 构建/测试提示 (build/test 由 agent 跑)。
func (p *goProvider) VerifyChain(ctx context.Context, src WorkflowSource, path string) (map[string]any, error) {
	out := map[string]any{"tool": "verify_chain", "language": "go", "path": path,
		"buildHint": "go build ./...", "testHint": "go test ./...",
		"packageDir": goPkgDir(path)}
	if diag, ok := src.Diagnostics(ctx, path); ok {
		out["diagnostics"] = map[string]any{"cached": true, "count": diag["count"], "items": goDiagItems(diag, 20)}
	} else {
		out["diagnostics"] = map[string]any{"cached": false, "note": "尚无推送诊断: 先用 get_diagnostics 触发一次查询再验证"}
	}
	return out, nil
}

// goPkgDir 文件所在包目录 (testHint 用)。
func goPkgDir(path string) string {
	d := filepath.Dir(path)
	if d == "" {
		return "."
	}
	return d
}

// SimulateEdit 安全编辑预览: edits 作用于磁盘内容得合成文本 → PreviewText
// (不落盘) → 等诊断 → diff → RestoreFile。应用由 agent 落地。
func (p *goProvider) SimulateEdit(ctx context.Context, src WorkflowSource, path string, edits []TextEdit) (map[string]any, error) {
	out := map[string]any{"tool": "simulate_edit", "language": "go", "path": path, "edits": len(edits)}
	if len(edits) == 0 {
		return nil, fmt.Errorf("edits 为空")
	}
	disk, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	synthetic, err := goApplyEdits(string(disk), edits)
	if err != nil {
		return nil, err
	}
	before, _ := src.Diagnostics(ctx, path)
	beforeSig := goDiagSig(before)
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
	deadline := time.Now().Add(goSimulateBudget)
	for {
		if d, ok := src.Diagnostics(ctx, path); ok {
			after = d
			if goDiagSig(d) != beforeSig {
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
	introduced, resolved := goDiagDiff(before, after)
	out["before"] = map[string]any{"count": before["count"], "items": goDiagItems(before, 20)}
	out["after"] = map[string]any{"count": after["count"], "items": goDiagItems(after, 20)}
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

// goApplyEdits 将 edits (LSP TextEdit 语义, 客户端列=行字节偏移) 作用于原文得合成文本。
// 行列越界/区间重叠直接报错 (不猜)。
func goApplyEdits(orig string, edits []TextEdit) (string, error) {
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
var _ Workflows = (*goProvider)(nil)

// goSortRefs 引用排序: 同目录优先 → 路径浅优先 → 服务器原始顺序保持
// (截断时保重要引用, 非返回顺序)。
func goSortRefs(queryPath string, in []map[string]any) {
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

// goDiagDiff 诊断差集 (before→after): introduced=新增消息, resolved=消除消息 (按 message 多重集差分)。
func goDiagDiff(before, after map[string]any) (introduced, resolved []string) {
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
