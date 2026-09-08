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
	Register(&typescriptProvider{})
}

// ---- typescriptProvider: TypeScript (typescript-language-server) ----
//
// tsserver 独特事实:
//   - 支持 implementation/typeDefinition (interface/class 实现跳转)
//   - 位置编码默认 utf-16 (不支持 utf-8 协商)
//   - capabilities 嵌套 textDocument.*
type typescriptProvider struct{}

func (p *typescriptProvider) LangID() LanguageID { return LangTypeScript }

func (p *typescriptProvider) MatchFile(path string) bool {
	for _, suf := range []string{".ts", ".tsx", ".mts", ".cts"} {
		if strings.HasSuffix(path, suf) {
			return true
		}
	}
	return false
}

func (p *typescriptProvider) Markers() []string { return []string{"tsconfig.json"} }

// Adapter typescript-language-server 内置默认配置真相。
func (p *typescriptProvider) Adapter() config.LangAdapter {
	return config.LangAdapter{
		LanguageID:   "typescript",
		Command:      "typescript-language-server",
		Args:         []string{"--stdio"},
		FilePatterns: []string{"**/*.ts", "**/*.tsx", "**/*.mts", "**/*.cts"},
		Markers:      p.Markers(),
		TimeoutSec:   30,
	}
}

func (p *typescriptProvider) Spawn(ctx context.Context, root string, timeout time.Duration) (ProcessHandle, error) {
	conn, err := lspclient.Spawn(ctx, p.Adapter(), root, timeout)
	if err != nil {
		return nil, err
	}
	return &tsHandle{Conn: conn}, nil
}

// tsHandle: typescriptProvider 私有进程句柄 (自包含)。
type tsHandle struct{ *lspclient.Conn }

func (h *tsHandle) Pid() int           { return h.ProcessPID() }
func (h *tsHandle) IsAlive() bool      { return h.ProcessPID() > 0 }
func (h *tsHandle) Kill() error        { return h.Close() }
func (h *tsHandle) Touch()             {}
func (h *tsHandle) LastUse() time.Time { return h.Conn.LastUse() }

var _ ProcessHandle = (*tsHandle)(nil)

func (p *typescriptProvider) Supports(cap Capability) bool {
	switch cap {
	case CapImplementation, CapTypeDefinition:
		return true // tsserver 支持
	default:
		return true
	}
}

// ParseLocations tsserver 特化解析。
// typescript-language-server 对 implementation (interface→class) 返回 Location/LocationLink;
// 特化注记: TS 的 .d.ts 声明位置可能返回 {targetRange} 指向声明而非实现, 语义由 tsserver 定。
func (p *typescriptProvider) ParseLocations(raw json.RawMessage) ([]LocationDTO, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		out := make([]LocationDTO, 0, len(arr))
		for _, item := range arr {
			if loc, ok := p.parseTSLocation(item); ok {
				out = append(out, loc)
			}
		}
		return out, nil
	}
	if loc, ok := p.parseTSLocation(raw); ok {
		return []LocationDTO{loc}, nil
	}
	return nil, nil
}

// tsRawLoc tsserver 位置原始结构 (Location/LocationLink 同构, typescript 私有)。
type tsRawLoc struct {
	URI         string      `json:"uri"`
	TargetURI   string      `json:"targetUri"`
	Range       *tsRawRange `json:"range"`
	TargetRange *tsRawRange `json:"targetRange"`
}

type tsRawRange struct {
	Start struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"start"`
	End struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"end"`
}

func (p *typescriptProvider) parseTSLocation(item json.RawMessage) (LocationDTO, bool) {
	var rl tsRawLoc
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
		Path:      tsURIToPath(uri),
		Range:     RangeDTO{StartLine: rg.Start.Line, StartCharacter: rg.Start.Character, EndLine: rg.End.Line, EndCharacter: rg.End.Character},
		FileLevel: tsIsFileLevel(rg.Start.Line, rg.Start.Character, rg.End.Line, rg.End.Character),
		Raw:       rawMap,
	}, true
}

// tsURIToPath URI → 本地路径 (typescript 私有)。
func tsURIToPath(uri string) string {
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

// tsIsFileLevel 文件级范围判断 (typescript 私有)。
func tsIsFileLevel(sl, sc, el, ec int) bool {
	if el-sl > 20 {
		return true
	}
	if sl == 0 && sc == 0 && el >= 3 {
		return true
	}
	return false
}

func (p *typescriptProvider) PositionParams(pos PositionDTO) map[string]any {
	return map[string]any{"line": pos.Line, "character": pos.Character}
}

// ---- 签名 hover 解析 (typescript-language-server 特化) ----
//
// tsserver hover markdown 形态 (实测): 单个 ```typescript 围栏, 内为带前缀签名:
//
//	(method) EnglishGreeter.greet(name: string): string
//	(property) Foo.bar: number
//	(function) createX(...): X
//
// Signature 获取符号签名 (typescript 特化): hover 是唯一来源, 经 src 查询后自解析。
func (p *typescriptProvider) Signature(ctx context.Context, src SignatureSource, path string, line, char int) (SignatureDTO, bool) {
	if src == nil {
		return SignatureDTO{}, false
	}
	md, ok := src.HoverMarkdown(ctx, path, line, char)
	if !ok {
		return SignatureDTO{}, false
	}
	return p.ParseHover(md)
}

// ParseHover 从 tsserver hover markdown 解析签名 (typescript 特化)。
// 特化: 取围栏内全部行, 去 "(method)"/"(property)"/"(function)"/"(constructor)" 前缀。
func (p *typescriptProvider) ParseHover(markdown string) (SignatureDTO, bool) {
	sig, ok := tsFenceSig(markdown)
	if !ok || sig == "" {
		return SignatureDTO{}, false
	}
	return SignatureDTO{
		Symbol:    tsSymbolName(sig),
		Signature: sig,
		Source:    "hover",
	}, true
}

// tsFenceSig 取首个 ```typescript/javascript 围栏内全部代码行, 去类型前缀。
func tsFenceSig(value string) (string, bool) {
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
	// 去 "(method)"/"(property)"/"(function)"/"(constructor)"/"(alias)" 前缀
	if strings.HasPrefix(sig, "(") {
		if i := strings.Index(sig, ") "); i >= 0 && i < 20 {
			sig = strings.TrimSpace(sig[i+2:])
		}
	}
	return sig, true
}

// tsSymbolName 从 ts 签名提取符号名 (尾段: "EnglishGreeter.greet(" → greet; "class Foo" → Foo)。
func tsSymbolName(sig string) string {
	first := sig
	if i := strings.IndexAny(first, "\n"); i >= 0 {
		first = first[:i]
	}
	s := strings.TrimSpace(first)
	if strings.HasPrefix(s, "class ") {
		s = strings.TrimSpace(s[6:])
	} else if strings.HasPrefix(s, "interface ") {
		s = strings.TrimSpace(s[10:])
	}
	cut := s
	if i := strings.IndexAny(cut, "(<:"); i > 0 {
		cut = cut[:i]
	}
	cut = strings.TrimSpace(cut)
	// 取 "Foo.greet" 最后段 (成员) 或整体 (函数)
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
