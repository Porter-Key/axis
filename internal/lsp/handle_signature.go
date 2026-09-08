package lsp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/Porter-Key/axis/internal/logger"
	"github.com/Porter-Key/axis/internal/lsp/capacity"
	"github.com/Porter-Key/axis/internal/lsp/langs"
)

// ---------- 位置结果签名增强 ----------

// attachSignaturesToLocations 解析 LSP 位置结果 (Location | Location[] | LocationLink[]),
// 对每个落点文件提取签名, 以结构化块附加返回。
// 不做全量常开: 只作用于 definition/references (跳转落点 = 用户断链失焦高发场景)。
// 性能保护: 最多增强 maxEnrich 个位置 (references 可能上百条, 逐个 hover 不可行)。
const maxEnrich = 5

func (l *LSP) attachSignaturesToLocations(ctx context.Context, srcRoot string, raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return raw
	}
	// 尝试解析为数组 (references / 多定义)
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		enriched := 0
		out := make([]json.RawMessage, 0, len(arr))
		for _, item := range arr {
			if enriched < maxEnrich {
				item = l.enrichOneLocation(ctx, srcRoot, item)
				enriched++
			}
			out = append(out, item)
		}
		if enriched == 0 {
			return raw
		}
		merged, err := json.Marshal(out)
		if err != nil {
			return raw
		}
		return merged
	}
	// 单个 Location / LocationLink
	return l.enrichOneLocation(ctx, srcRoot, raw)
}

// enrichOneLocation 提取单个位置 (Location 或 LocationLink), 附加 signature 字段。
// 只对"符号级位置"增强: 文件级范围 (覆盖整个文件 = 包/模块跳转, 如 import 包名) 跳过,
// 避免对包内所有文件逐个误 hover (gopls 对包跳转返回全文件列表)。
func (l *LSP) enrichOneLocation(ctx context.Context, srcRoot string, item json.RawMessage) json.RawMessage {
	// 解析出 uri + range (Location 与 LocationLink 都含 targetUri/targetRange 或 uri/range)
	var loc struct {
		URI       string `json:"uri"`
		TargetURI string `json:"targetUri"`
		Range     *struct {
			Start struct {
				Line      int `json:"line"`
				Character int `json:"character"`
			} `json:"start"`
			End struct {
				Line      int `json:"line"`
				Character int `json:"character"`
			} `json:"end"`
		} `json:"range"`
		TargetRange *struct {
			Start struct {
				Line      int `json:"line"`
				Character int `json:"character"`
			} `json:"start"`
			End struct {
				Line      int `json:"line"`
				Character int `json:"character"`
			} `json:"end"`
		} `json:"targetRange"`
		TargetSelectionRange *struct {
			Start struct {
				Line      int `json:"line"`
				Character int `json:"character"`
			} `json:"start"`
			End struct {
				Line      int `json:"line"`
				Character int `json:"character"`
			} `json:"end"`
		} `json:"targetSelectionRange"`
	}
	if err := json.Unmarshal(item, &loc); err != nil {
		return item
	}
	uri := loc.URI
	rg := loc.Range
	if uri == "" {
		uri = loc.TargetURI
		rg = loc.TargetRange
	}
	if uri == "" || rg == nil {
		return item
	}
	// hover 位置优先级: targetSelectionRange.start (符号本体, 如 impl 的目标类型名)
	// → targetRange.start (整块声明, 可能是 impl/trait 关键字位置, hover 会命中关键字文档)
	// → range.start。location 显示也用同一位置。
	hoverPos := rg
	if loc.TargetSelectionRange != nil && uri == loc.TargetURI {
		hoverPos = loc.TargetSelectionRange
	} else if loc.TargetRange != nil && uri == loc.TargetURI {
		hoverPos = loc.TargetRange
	}
	// 文件级范围启发式: 单行且行内占满 (line==line 且 char 差距 > 100 列) 或 range 巨大 → 跳过
	if isFileLevelRange(hoverPos.Start.Line, hoverPos.Start.Character, hoverPos.End.Line, hoverPos.End.Character) {
		return item
	}
	path := uriToPath(uri)
	if path == "" {
		return item
	}
	// 提取签名 (lang 由文件扩展名检测, root 优先用落点自己项目的根)
	block := l.enrichSignature(ctx, projectRootFor(l, path, srcRoot), path, hoverPos.Start.Line, hoverPos.Start.Character)
	sigJSON, _ := json.Marshal(block)

	// 合并: 原 item + signature 字段
	var m map[string]any
	if err := json.Unmarshal(item, &m); err != nil {
		return item
	}
	m["signature"] = json.RawMessage(sigJSON)
	merged, err := json.Marshal(m)
	if err != nil {
		return item
	}
	return merged
}

// isFileLevelRange 判断是否文件级/包级范围 (非符号级):
// 起始在文件头 (line<=1) 且结束行远大于起始行 → 覆盖整个文件。
func isFileLevelRange(sl, sc, el, ec int) bool {
	if el-sl > 20 {
		return true // 跨 >20 行不可能是单个符号
	}
	if sl == 0 && sc == 0 && el >= 3 {
		return true // 从文件头开始的宽范围
	}
	return false
}

// projectRootFor 为落点路径找项目根 (优先已注册的, 其次 marker 反查, 兜底 srcRoot)。
// 同 ensureProject: 根约束在 srcRoot (激活项目) 边界内, 不漂到祖先目录; 新根顺手注册+监控
// (否则该连接没有 fsmonitor, 缓存永久 stale)。
func projectRootFor(l *LSP, path, srcRoot string) string {
	if root, ok := l.reg.ProjectForFile(path); ok && withinBoundary(root, srcRoot) {
		return root
	}
	lang := langs.Detect(l.getConfig(), path)
	if lang != "" {
		if root, _, ok := langs.FindProjectRootBounded(l.getConfig(), lang, path, srcRoot); ok {
			l.reg.RegisterProject(root)
			l.reg.RegisterProjectLangs(root, lang)
			l.startMonitor(root)
			return root
		}
	}
	return srcRoot
}

// uriToPath file:// URI 转本地路径。
func uriToPath(uri string) string {
	if !strings.HasPrefix(uri, "file://") {
		return ""
	}
	p := strings.TrimPrefix(uri, "file://")
	// 处理 file:///path → /path (去前导空 host)
	p = strings.TrimPrefix(p, "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// ---------- 签名增强管线 (纯编排: 解析全在 provider, 本层零语言语义) ----------

// SignatureBlock 符号签名结构化块 (统一喂给 LLM 的格式)。
// 核心字段由 Provider.Signature 返回的 SignatureDTO 填充 (契约标准化),
// Package/Location 等位置上下文由本层 (宿主) 附加。
type SignatureBlock struct {
	Symbol    string `json:"symbol"`             // 符号名 (provider 解析)
	Package   string `json:"package,omitempty"`  // 所属包/模块路径 (宿主填)
	Signature string `json:"signature"`          // 人类可读签名 (provider 解析, 可多行)
	Params    []any  `json:"params,omitempty"`   // 参数列表 (预留; 数据源并入时填)
	Returns   []any  `json:"returns,omitempty"`  // 返回类型列表 (预留)
	Doc       string `json:"doc,omitempty"`      // 简短文档 (provider 解析)
	Source    string `json:"source"`             // 来源: hover|go-doc|unavailable (provider 自报)
	Note      string `json:"note,omitempty"`     // 降级/能力说明 (provider 自报)
	Location  string `json:"location,omitempty"` // 文件:行:列 (宿主填)
	Err       string `json:"error,omitempty"`    // 提取失败说明 (不静默缺省)
}

// enrichSignature 对单个文件位置获取签名块 (纯编排)。
// 解析/降级策略全部由该语言 provider 决定; 本层只做: 检测语言 → 取 provider →
// 注入 hover 查询源 → 填宿主上下文。
func (l *LSP) enrichSignature(ctx context.Context, root, path string, line, char int) SignatureBlock {
	block := SignatureBlock{Source: "unavailable"}
	lang := langs.Detect(l.getConfig(), path)
	if lang == "" {
		block.Err = "无法识别语言: " + path
		return block
	}
	block.Location = filepath.ToSlash(path) + ":" + itoa(line+1) + ":" + itoa(char+1)
	prov, ok := lsp_capacity.Get(lsp_capacity.LanguageID(lang))
	if !ok {
		block.Err = "无该语言 provider: " + lang
		return block
	}
	dto, ok := prov.Signature(ctx, l.hoverSource(root, lang), path, line, char)
	logger.With("lang", lang, "path", path, "line", line, "char", char).Debug("provider Signature dto",
		"ok", ok, "symbol", dto.Symbol, "signature", truncate(dto.Signature, 200), "doc", truncate(dto.Doc, 80), "note", dto.Note)
	if !ok || dto.Signature == "" {
		if dto.Note != "" {
			block.Err = dto.Note
		} else {
			block.Err = "该位置无法取得签名 (hover/降级均无结果)"
		}
		return block
	}
	block.Symbol = dto.Symbol
	block.Signature = dto.Signature
	block.Doc = dto.Doc
	block.Source = dto.Source
	block.Note = dto.Note
	if root != "" {
		block.Package = shortPkg(path, root)
	}
	return block
}

// hoverSource 构造宿主 hover 查询源 (实现契约 SignatureSource, 无语言语义)。
func (l *LSP) hoverSource(root, lang string) lsp_capacity.SignatureSource {
	return &appHoverSource{app: l, root: root, lang: lang}
}

// appHoverSource 宿主实现: 经 registry 对指定语言连接发起 textDocument/hover。
type appHoverSource struct {
	app  *LSP
	root string
	lang string
}

func (s *appHoverSource) HoverMarkdown(ctx context.Context, path string, line, char int) (string, bool) {
	logger.With("root", s.root, "lang", s.lang, "path", path).Debug("enrich hover 请求", "line", line, "char", char)
	// 列单位: 上游给的是客户端单位 (响应已 Adapt), 这里换回服务器单位再查
	sline, sch := s.app.toServerPos(s.lang, path, line, char)
	res, err := s.app.reg.CallWithDoc(ctx, s.root, s.lang, "textDocument/hover", path,
		map[string]any{"position": map[string]any{"line": sline, "character": sch}},
		s.app.reqTimeout(s.lang))
	if err != nil {
		return "", false
	}
	logger.With("root", s.root, "lang", s.lang, "path", path, "line", line, "char", char).Debug("enrich hover 返回", "raw", truncate(string(res), 3000))
	var hov struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(res, &hov); err != nil || len(hov.Contents) == 0 {
		return "", false
	}
	value := hoverText(hov.Contents)
	if value == "" {
		return "", false
	}
	return value, true
}

// hoverText 从 hover contents (markup/字符串/多块) 提取纯文本。
// LSP 协议层结构 (contents 形状), 非语言语义 → 留在宿主。
func hoverText(contents json.RawMessage) string {
	var s string
	if err := json.Unmarshal(contents, &s); err == nil {
		return s
	}
	var obj struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(contents, &obj); err == nil && obj.Value != "" {
		return obj.Value
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(contents, &arr); err == nil {
		var parts []string
		for _, m := range arr {
			if p := hoverText(m); p != "" {
				parts = append(parts, p)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// shortPkg 返回 path 相对 root 的包短名 (宿主通用)。
func shortPkg(path, root string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(filepath.Dir(rel))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
