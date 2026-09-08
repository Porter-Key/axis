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
