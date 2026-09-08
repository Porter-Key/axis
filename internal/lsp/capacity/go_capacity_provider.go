package lsp_capacity

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Porter-Key/axis/internal/config"
	"github.com/Porter-Key/axis/internal/lsp/client"
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
