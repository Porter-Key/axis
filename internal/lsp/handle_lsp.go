package lsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/Porter-Key/axis/internal/lsp/fsmonitor"
	"github.com/Porter-Key/axis/internal/lsp/langs"
	"github.com/Porter-Key/axis/internal/mcpkit"
)

// ---------- LSP 工具 handlers ----------

// handleDefinition get_definition: scope 支持 definition|implementation|typeDefinition。
// 语言服务器不支持指定 scope 时按能力降级 (说明降级, 不报错)。
func (l *LSP) handleDefinition(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	path, ok := mcpkit.AbsArg(args, "path")
	if !ok {
		return mcpkit.ErrRes("path 必须为绝对路径: " + mcpkit.ArgStr(args, "path")), nil
	}
	line, _ := args["line"].(float64)
	ch, _ := args["character"].(float64)
	scope, _ := args["scope"].(string)
	if scope == "" {
		scope = "definition"
	}
	if _, ok := l.requireProject(ctx, path); !ok {
		return mcpkit.ErrRes(l.notActivated()), nil
	}
	root, ok := l.ensureProject(ctx, path)
	if !ok {
		return mcpkit.ErrRes("无法定位项目根: " + path), nil
	}
	lang := langs.Detect(l.getConfig(), path)
	if lang == "" {
		return mcpkit.ErrRes("无法识别语言: " + path), nil
	}
	conn, err := l.reg.LSPConn(ctx, root, lang, true)
	if err != nil {
		return mcpkit.ErrRes("LSP 启动失败: " + err.Error()), nil
	}

	// scope → LSP 方法 + 能力探测路径 (协商降级)
	type scopeMap struct{ method, capKey string }
	scopeInfo, ok := map[string]scopeMap{
		"definition":     {"textDocument/definition", "textDocument.definitionProvider"},
		"implementation": {"textDocument/implementation", "textDocument.implementationProvider"},
		"typeDefinition": {"textDocument/typeDefinition", "textDocument.typeDefinitionProvider"},
	}[scope]
	if !ok {
		return mcpkit.ErrRes("scope 仅支持 definition|implementation|typeDefinition"), nil
	}
	// 能力降级: implementation/typeDefinition 非默认, 服务器不支持 → 明确说明并回退 definition
	if scope != "definition" && !conn.Supports(scopeInfo.capKey) {
		return mcpkit.ErrRes("语言服务器不支持 scope=" + scope + " (缺 " + scopeInfo.capKey + " capability), 请用默认 definition"), nil
	}
	extra := map[string]any{
		"position": map[string]any{"line": int(line), "character": int(ch)},
	}
	// 请求列换算: 客户端 UTF-8 → 服务器单位 (各 provider 自转码, 无 provider 则原样)
	{
		sline, sch := l.toServerPos(lang, path, int(line), int(ch))
		extra["position"] = map[string]any{"line": sline, "character": sch}
	}
	res, err := l.reg.CallWithDoc(ctx, root, lang, scopeInfo.method, path, extra, l.reqTimeout(lang))
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	// 响应列换算: 服务器单位 → 客户端 UTF-8 (再做签名增强; enrich 内 hover 会换回去)
	res = l.adaptPositions(lang, path, res)
	// 签名增强: 跳转落点自动附签名 (definition/implementation/typeDefinition 全覆盖)
	res = l.attachSignaturesToLocations(ctx, root, res)
	return l.okJSON("get_definition", res), nil
}

// handleLSPRequest 生成单个位置查询 handler (hover/references 共用)。
func (l *LSP) handleLSPRequest(lspMethod string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		path, ok := mcpkit.AbsArg(args, "path")
		if !ok {
			return mcpkit.ErrRes("path 必须为绝对路径: " + mcpkit.ArgStr(args, "path")), nil
		}
		line, _ := args["line"].(float64)
		ch, _ := args["character"].(float64)
		if _, ok := l.requireProject(ctx, path); !ok {
			return mcpkit.ErrRes(l.notActivated()), nil
		}
		root, ok := l.ensureProject(ctx, path)
		if !ok {
			return mcpkit.ErrRes("无法定位项目根: " + path), nil
		}
		lang := langs.Detect(l.getConfig(), path)
		if lang == "" {
			return mcpkit.ErrRes("无法识别语言: " + path), nil
		}
		method := map[string]string{
			"hover":      "textDocument/hover",
			"references": "textDocument/references",
		}[lspMethod]
		// 请求列换算: 客户端 UTF-8 → 服务器单位
		sline, sch := l.toServerPos(lang, path, int(line), int(ch))
		extra := map[string]any{
			"position": map[string]any{"line": sline, "character": sch},
		}
		res, err := l.reg.CallWithDoc(ctx, root, lang, method, path, extra, l.reqTimeout(lang))
		if err != nil {
			return mcpkit.ErrRes(err.Error()), nil
		}

		// ---- 签名增强: references 的返回位置是"跳转落点" ----
		// 落点可能在第三方包/官方包 (module cache / site-packages / node_modules),
		// LLM 看到位置后常断链失焦。这里对每个落点自动附带签名块 (用户决策: 签名是前提)。
		if lspMethod == "references" {
			// 响应列换算先行 (落点转为客户端单位; enrich 内 hover 会按需换回去)
			res = l.adaptPositions(lang, path, res)
			res = l.attachSignaturesToLocations(ctx, root, res)
		}
		return l.okJSON("get_"+lspMethod, res), nil
	}
}

func (l *LSP) handleDiagnostics(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	path, ok := mcpkit.AbsArg(args, "path")
	if !ok {
		return mcpkit.ErrRes("path 必须为绝对路径: " + mcpkit.ArgStr(args, "path")), nil
	}
	if _, ok := l.requireProject(ctx, path); !ok {
		return mcpkit.ErrRes(l.notActivated()), nil
	}
	root, ok := l.ensureProject(ctx, path)
	if !ok {
		return mcpkit.ErrRes("无法定位项目根: " + path), nil
	}
	lang := langs.Detect(l.getConfig(), path)
	if lang == "" {
		return mcpkit.ErrRes("无法识别语言: " + path), nil
	}
	// 推送语义: 先确保连接在跑, 再用一次轻查询把文档打开 (didOpen 是服务器开始推送的前提),
	// 然后等推送最多 2s。缓存命中立刻返回。
	conn, err := l.reg.LSPConn(ctx, root, lang, true)
	if err != nil {
		return mcpkit.ErrRes("LSP 启动失败: " + err.Error()), nil
	}
	_ = conn
	// 轻触发: hover(0,0) 只为 didOpen 文档 (结果丢弃; 0:0 在任何编码下都是 0:0, 无需换算)
	_, _ = l.reg.CallWithDoc(ctx, root, lang, "textDocument/hover", path,
		map[string]any{"position": map[string]any{"line": 0, "character": 0}}, 10*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if raw, ok := l.reg.DiagnosticsFor(root, lang, path); ok {
			return l.okJSON("get_diagnostics", l.shapeDiagnostics(path, raw, true)), nil
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return mcpkit.ErrRes(ctx.Err().Error()), nil
		case <-time.After(200 * time.Millisecond):
		}
	}
	return l.okJSON("get_diagnostics", l.shapeDiagnostics(path, nil, false)), nil
}

// shapeDiagnostics 包装诊断输出: 明确区分"服务器说没报错"与"尚无推送"(避免 agent 误判零报错)。
func (l *LSP) shapeDiagnostics(path string, raw json.RawMessage, cached bool) map[string]any {
	out := map[string]any{"path": path, "cached": cached, "diagnostics": []any{}, "count": 0}
	if !cached {
		out["note"] = "尚无该文件的推送诊断 (服务器未推送过, 不是零报错; 确保文件被查询过且服务器支持 publishDiagnostics)"
		return out
	}
	var p struct {
		Diagnostics []any `json:"diagnostics"`
	}
	if err := json.Unmarshal(raw, &p); err == nil && p.Diagnostics != nil {
		out["diagnostics"] = p.Diagnostics
		out["count"] = len(p.Diagnostics)
	}
	return out
}

func (l *LSP) handleRename(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	path, ok := mcpkit.AbsArg(args, "path")
	if !ok {
		return mcpkit.ErrRes("path 必须为绝对路径: " + mcpkit.ArgStr(args, "path")), nil
	}
	line, _ := args["line"].(float64)
	ch, _ := args["character"].(float64)
	newName, _ := args["newName"].(string)
	if _, ok := l.requireProject(ctx, path); !ok {
		return mcpkit.ErrRes(l.notActivated()), nil
	}
	root, ok := l.ensureProject(ctx, path)
	if !ok {
		return mcpkit.ErrRes("无法定位项目根: " + path), nil
	}
	lang := langs.Detect(l.getConfig(), path)
	// 请求列换算: 客户端 UTF-8 → 服务器单位
	sline, sch := l.toServerPos(lang, path, int(line), int(ch))
	res, err := l.reg.CallWithDoc(ctx, root, lang, "textDocument/rename", path, map[string]any{
		"position": map[string]any{"line": sline, "character": sch},
		"newName":  newName,
	}, l.reqTimeout(lang))
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	// 响应列换算: WorkspaceEdit 位置 → 客户端单位 (agent 按此落盘, 错位会改错地方)
	res = l.adaptPositions(lang, path, res)
	return l.okJSON("get_rename", res), nil
}

func (l *LSP) handleSymbols(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	path, ok := mcpkit.AbsArg(args, "path")
	if !ok {
		return mcpkit.ErrRes("path 必须为绝对路径: " + mcpkit.ArgStr(args, "path")), nil
	}
	query, _ := args["query"].(string)
	if _, ok := l.requireProject(ctx, path); !ok {
		return mcpkit.ErrRes(l.notActivated()), nil
	}
	// 目录 → workspace/symbol; 文件 → documentSymbol
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		proj, ok := l.requireProject(ctx, path)
		if !ok {
			return mcpkit.ErrRes(l.notActivated()), nil
		}
		// 按项目根 (而非子目录本身) 找已有连接: 连接 key 是项目根|lang,
		// 拿子目录查池永远 miss (旧 bug)。
		root := proj
		if rp, ok := l.reg.ProjectForFile(path); ok && withinBoundary(rp, proj) {
			root = rp
		}
		for lang := range l.getConfig().Adapters {
			conn, err := l.reg.LSPConn(ctx, root, lang, false)
			if err == nil && conn != nil {
				params := map[string]any{}
				if query != "" {
					params["query"] = query
				}
				res, err := conn.WorkspaceRequest(ctx, "workspace/symbol", params, 30*time.Second)
				if err != nil {
					return mcpkit.ErrRes(err.Error()), nil
				}
				// 响应列换算 (落点自带 uri, srcPath 仅兜底)
				res = l.adaptPositions(lang, path, res)
				return l.okJSON("get_symbols", res), nil
			}
		}
		return mcpkit.ErrRes("目录模式需已有 LSP 连接 (先用文件模式查询触发)"), nil
	}
	root, ok := l.ensureProject(ctx, path)
	if !ok {
		return mcpkit.ErrRes("无法定位项目根: " + path), nil
	}
	lang := langs.Detect(l.getConfig(), path)
	res, err := l.reg.CallWithDoc(ctx, root, lang, "textDocument/documentSymbol", path, nil, l.reqTimeout(lang))
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	// 响应列换算 (同文件符号, 归属查询源文件)
	res = l.adaptPositions(lang, path, res)
	return l.okJSON("get_symbols", res), nil
}

// ---------- 项目/监控辅助 ----------

// notActivated 未激活拒绝消息 (附 Gate 恢复提示: 上次项目一调即回)。
func (l *LSP) notActivated() string {
	return l.gate.RejectMsg("未激活项目或文件不在激活项目内: 请先 axis_activate(project)")
}

// requireProject 门禁: 从 ctx 取激活项目, 校验 path 属于激活项目内。
// 未激活 → err; path 在项目外 → err (严格绑定: 不能跨项目操作)。
func (l *LSP) requireProject(ctx context.Context, filePath string) (string, bool) {
	if l.gate == nil {
		return "", false
	}
	proj, ok := l.gate.ProjectFor(ctx)
	if !ok {
		return "", false
	}
	abs, _ := filepath.Abs(filePath)
	// path 必须在激活项目根下 (或等于根)
	rel, err := filepath.Rel(proj, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return proj, true
}

// ensureProject 确保项目已注册 + fsmonitor 在跑; 返回项目根。
// 根约束: 以当前会话激活项目为边界 (root ⊆ boundary), 已注册根与 marker 搜索一律不出界;
// 找不到 marker 时兜底用 boundary 本身, 根永不漂出激活目录 (如激活 /a/b 不会定根到 /a)。
// 无门禁上下文 (控制面/测试) 时退化为旧行为。
func (l *LSP) ensureProject(ctx context.Context, filePath string) (string, bool) {
	abs, _ := filepath.Abs(filePath)
	boundary, _ := l.requireProject(ctx, abs)
	if boundary == "" {
		boundary = filepath.Dir(abs)
	}
	if root, ok := l.reg.ProjectForFile(abs); ok && withinBoundary(root, boundary) {
		l.reg.TouchCall(root)
		return root, true
	}
	// 未注册或已注册根在边界外 (他会话的祖先根): 在边界内按 marker 定位并注册
	lang := langs.Detect(l.getConfig(), abs)
	if lang == "" {
		return "", false
	}
	root, _, ok := langs.FindProjectRootBounded(l.getConfig(), lang, abs, boundary)
	if !ok {
		root = boundary // 兜底用激活目录本身 (不漂移, 不建 per-dir 散根)
	}
	l.reg.RegisterProject(root)
	l.reg.RegisterProjectLangs(root, lang)
	l.startMonitor(root)
	return root, true
}

// withinBoundary root 是否在 boundary 内 (含等于)。
func withinBoundary(root, boundary string) bool {
	rel, err := filepath.Rel(boundary, root)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func (l *LSP) startMonitor(root string) {
	l.muMon.Lock()
	defer l.muMon.Unlock()
	if _, ok := l.monitors[root]; ok {
		return
	}
	m, err := newMonitor(root, l.getConfig().Ignore)
	if err != nil {
		return // 监听失败不致命 (索引新鲜度降级)
	}
	l.monitors[root] = m
}

// ---------- 工厂/超时 (LSP 自持) ----------

// newMonitor 创建 fsmonitor.Monitor (测试可替换)。ignore 为项目级忽略规则。
var newMonitor = func(root string, ignore []string) (*fsmonitor.Monitor, error) {
	return fsmonitor.New(root, 300*time.Millisecond, ignore)
}

// reqTimeout 单次 LSP 请求超时: 优先 adapter.TimeoutSec, 默认 30s。
// csharp-ls 冷启动慢 (60s 配置) → 首请求需宽限。
func (l *LSP) reqTimeout(lang string) time.Duration {
	if ad, ok := l.getConfig().Adapters[lang]; ok && ad.TimeoutSec > 0 {
		return time.Duration(ad.TimeoutSec) * time.Second
	}
	return 30 * time.Second
}
