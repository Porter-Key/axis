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
