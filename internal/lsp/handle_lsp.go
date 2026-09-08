package lsp

import (
	"context"
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
		return mcpkit.ErrRes("未激活项目或文件不在激活项目内: 请先 axis_activate(project)"), nil
	}
	root, ok := l.ensureProject(ctx, path)
	if !ok {
		return mcpkit.ErrRes("无法定位项目根: " + path), nil
	}
	lang := langs.Detect(l.cfg, path)
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
	res, err := l.reg.CallWithDoc(ctx, root, lang, scopeInfo.method, path, extra, l.reqTimeout(lang))
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	// 签名增强: 跳转落点自动附签名 (definition/implementation/typeDefinition 全覆盖)
	res = l.attachSignaturesToLocations(ctx, root, res)
	return mcpkit.OkJSON(res), nil
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
			return mcpkit.ErrRes("未激活项目或文件不在激活项目内: 请先 axis_activate(project)"), nil
		}
		root, ok := l.ensureProject(ctx, path)
		if !ok {
			return mcpkit.ErrRes("无法定位项目根: " + path), nil
		}
		lang := langs.Detect(l.cfg, path)
		if lang == "" {
			return mcpkit.ErrRes("无法识别语言: " + path), nil
		}
		method := map[string]string{
			"hover":      "textDocument/hover",
			"references": "textDocument/references",
		}[lspMethod]
		extra := map[string]any{
			"position": map[string]any{"line": int(line), "character": int(ch)},
		}
		res, err := l.reg.CallWithDoc(ctx, root, lang, method, path, extra, l.reqTimeout(lang))
		if err != nil {
			return mcpkit.ErrRes(err.Error()), nil
		}

		// ---- 签名增强: references 的返回位置是"跳转落点" ----
		// 落点可能在第三方包/官方包 (module cache / site-packages / node_modules),
		// LLM 看到位置后常断链失焦。这里对每个落点自动附带签名块 (用户决策: 签名是前提)。
		if lspMethod == "references" {
			res = l.attachSignaturesToLocations(ctx, root, res)
		}
		return mcpkit.OkJSON(res), nil
	}
}

func (l *LSP) handleDiagnostics(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	path, ok := mcpkit.AbsArg(args, "path")
	if !ok {
		return mcpkit.ErrRes("path 必须为绝对路径: " + mcpkit.ArgStr(args, "path")), nil
	}
	if _, ok := l.requireProject(ctx, path); !ok {
		return mcpkit.ErrRes("未激活项目或文件不在激活项目内: 请先 axis_activate(project)"), nil
	}
	root, ok := l.ensureProject(ctx, path)
	if !ok {
		return mcpkit.ErrRes("无法定位项目根: " + path), nil
	}
	lang := langs.Detect(l.cfg, path)
	if lang == "" {
		return mcpkit.ErrRes("无法识别语言: " + path), nil
	}
	conn, err := l.reg.LSPConn(ctx, root, lang, true)
	if err != nil {
		return mcpkit.ErrRes("LSP 启动失败: " + err.Error()), nil
	}
	_ = conn
	return mcpkit.ErrRes("get_diagnostics 需 LSP 3.17 pull 支持, 阶段 C 实现"), nil
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
		return mcpkit.ErrRes("未激活项目或文件不在激活项目内: 请先 axis_activate(project)"), nil
	}
	root, ok := l.ensureProject(ctx, path)
	if !ok {
		return mcpkit.ErrRes("无法定位项目根: " + path), nil
	}
	lang := langs.Detect(l.cfg, path)
	res, err := l.reg.CallWithDoc(ctx, root, lang, "textDocument/rename", path, map[string]any{
		"position": map[string]any{"line": int(line), "character": int(ch)},
		"newName":  newName,
	}, l.reqTimeout(lang))
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	return mcpkit.OkJSON(res), nil
}

func (l *LSP) handleSymbols(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	path, ok := mcpkit.AbsArg(args, "path")
	if !ok {
		return mcpkit.ErrRes("path 必须为绝对路径: " + mcpkit.ArgStr(args, "path")), nil
	}
	query, _ := args["query"].(string)
	if _, ok := l.requireProject(ctx, path); !ok {
		return mcpkit.ErrRes("未激活项目或文件不在激活项目内: 请先 axis_activate(project)"), nil
	}
	// 目录 → workspace/symbol; 文件 → documentSymbol
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		root := path
		for lang := range l.cfg.Adapters {
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
				return mcpkit.OkJSON(res), nil
			}
		}
		return mcpkit.ErrRes("目录模式需已有 LSP 连接 (先用文件模式查询触发)"), nil
	}
	root, ok := l.ensureProject(ctx, path)
	if !ok {
		return mcpkit.ErrRes("无法定位项目根: " + path), nil
	}
	lang := langs.Detect(l.cfg, path)
	res, err := l.reg.CallWithDoc(ctx, root, lang, "textDocument/documentSymbol", path, nil, l.reqTimeout(lang))
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	return mcpkit.OkJSON(res), nil
}

// ---------- 项目/监控辅助 ----------

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
func (l *LSP) ensureProject(ctx context.Context, filePath string) (string, bool) {
	abs, _ := filepath.Abs(filePath)
	if root, ok := l.reg.ProjectForFile(abs); ok {
		l.reg.TouchCall(root)
		return root, true
	}
	// 未注册: 尝试按 marker 定位根并注册
	lang := langs.Detect(l.cfg, abs)
	if lang == "" {
		return "", false
	}
	root, _, ok := langs.FindProjectRoot(l.cfg, lang, abs)
	if !ok {
		root = filepath.Dir(abs) // 兜底用文件目录
	}
	l.reg.RegisterProject(root)
	l.reg.RegisterProjectLangs(root, lang)
	l.startMonitor(root)
	return root, true
}

func (l *LSP) startMonitor(root string) {
	l.muMon.Lock()
	defer l.muMon.Unlock()
	if _, ok := l.monitors[root]; ok {
		return
	}
	m, err := newMonitor(root, l.cfg.Ignore)
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
	if ad, ok := l.cfg.Adapters[lang]; ok && ad.TimeoutSec > 0 {
		return time.Duration(ad.TimeoutSec) * time.Second
	}
	return 30 * time.Second
}
