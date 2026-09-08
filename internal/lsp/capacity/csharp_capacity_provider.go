package lsp_capacity

import (
	"context"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/Porter-Key/axis/internal/config"
	"github.com/Porter-Key/axis/internal/lsp/client"
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
