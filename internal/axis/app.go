// Package axis — axis 壳: 组装各子插件 (LSP/codegraph/memory) 为统一 MCP server + 控制面。
//
// 架构 (用户确认): 一个服务 + 多个子插件。壳不持有业务, 只做:
//  1. 构造插件 (依赖 config/logger)
//  2. 统一注册进同一 MCP server
//  3. 控制面 HTTP (/ctrl: 注册/心跳/状态/重载) — 委托给相关插件能力
//  4. 会话激活门禁: axis_activate(project) 绑定 MCP 会话→项目, 未激活拒绝目录级工具
package axis

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/Porter-Key/axis/internal/codegraph"
	"github.com/Porter-Key/axis/internal/config"
	"github.com/Porter-Key/axis/internal/lsp"
	"github.com/Porter-Key/axis/internal/mcpkit"
	"github.com/Porter-Key/axis/internal/memory"
	"github.com/Porter-Key/axis/internal/plugin"
)

// App 壳。
type App struct {
	// cfg 热重载可换指针: atomic (ctrl 面 HTTP handler 并发读, ctrlReload 并发写)。
	cfg     atomic.Pointer[config.Config]
	cfgPath string // 启动时的 -config 传入值 (可为 "", 由 config.ResolvePath 解析); ctrlReload 复用
	plugins []plugin.ToolProvider
	// 具名引用 (控制面/热重载需要特定插件能力)
	lsp *lsp.LSP
	cg  *codegraph.Plugin
	mem *memory.Plugin
	MS  *server.MCPServer

	gate *plugin.Gate // 会话→激活项目门禁

	muReload sync.Mutex
}

// axisMCPVersion axis 自身 MCP server 版本。
// 注意这是另一套版本域, 与 codegraph 后端版本 (indexStatus.version, 当前 1.6.0)
// 无关: 一个是本服务发版, 一个是图谱索引格式/后端, 升级时不要误判漂移。
const axisMCPVersion = "0.3.1"

// New 组装全部插件。
func New(cfg *config.Config) (*App, error) {
	a := &App{gate: plugin.NewGate()}
	a.cfg.Store(cfg)
	// Gate 绑定 TTL 与 registry 会话心跳超时同口径 (滑动窗口, 活跃会话永不过期)。
	if hb := time.Duration(cfg.Heartbeat.TimeoutSec) * time.Second; hb > 0 {
		a.gate.SetTTL(hb)
	}

	// 子插件: LSP / codegraph / memory
	lspPlugin := lsp.New(cfg)
	cgPlugin := codegraph.NewPlugin(cfg.Codegraph)
	memPlugin, err := newMemoryPlugin(cfg)
	if err != nil {
		return nil, err
	}

	a.lsp = lspPlugin
	a.cg = cgPlugin
	a.mem = memPlugin
	a.plugins = []plugin.ToolProvider{lspPlugin, cgPlugin, memPlugin}

	ms := server.NewMCPServer("axis", axisMCPVersion,
		server.WithDescription("axis: 多语言 LSP 语义 + codegraph 图谱 + memory 知识库 MCP server。"+langsList(cfg)))
	a.MS = ms
	// 门禁注入各插件 (目录级工具激活检查)
	a.injectGate()
	// axis_activate 工具 (壳级, 绑定会话→项目)
	ms.AddTool(mcp.NewTool("axis_activate",
		mcp.WithDescription("激活当前 MCP 会话到项目 (目录级工具的前置门禁)。换目录必须重新激活; 未激活时目录级工具全部拒绝。项目自包含: memory 按项目隔离, 全局数据需显式 project=global 访问。"),
		mcp.WithString("project", mcp.Required(), mcp.Description("项目根目录 (绝对路径)")),
	), a.handleActivate)
	for _, p := range a.plugins {
		p.Register(ms)
	}
	return a, nil
}

// SetConfigPath 记录启动配置路径 (main 在 New 后调用, 供 ctrlReload 复用,
// 否则重载会丢 -config 读到另一个文件)。
func (a *App) SetConfigPath(p string) { a.cfgPath = p }

// handleActivate axis_activate: 绑定当前会话到项目。
func (a *App) handleActivate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sid := plugin.SessionIDFromContext(ctx)
	if sid == "" {
		return mcpkit.ErrRes("无法获取会话 ID (stdio 模式不支持会话激活, 请用 HTTP 模式)"), nil
	}
	args := req.GetArguments()
	proj := mcpkit.ArgStr(args, "project")
	if proj == "" {
		return mcpkit.ErrRes("project 必填 (绝对路径)"), nil
	}
	abs, err := filepath.Abs(proj)
	if err != nil {
		return mcpkit.ErrRes("project 解析失败: " + err.Error()), nil
	}
	a.gate.Activate(sid, abs)
	// 同步注册项目到 LSP registry + 启动 fsmonitor
	a.lsp.RegisterProject(abs)
	a.lsp.StartMonitor(abs)
	return mcpkit.OkJSON(map[string]any{
		"ok": true, "session": sid, "project": filepath.Clean(abs),
		"note": "换目录请重新 axis_activate",
	}), nil
}

// injectGate 把门禁传给需要激活检查的插件。
func (a *App) injectGate() {
	// 插件若实现 GateSetter 接口则注入 (在 Register 前调用)
	for _, p := range a.plugins {
		if gs, ok := p.(plugin.GateSetter); ok {
			gs.SetGate(a.gate)
		}
	}
}

// Start 启动全部插件后台资源。
func (a *App) Start(ctx context.Context) error {
	for _, p := range a.plugins {
		if err := p.Start(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Shutdown 释放全部插件。
func (a *App) Shutdown() error {
	for _, p := range a.plugins {
		_ = p.Shutdown(context.Background())
	}
	return nil
}

// MCPServer 暴露 MCP server (main 用)。
func (a *App) MCPServer() *server.MCPServer { return a.MS }

// ---------- 插件工厂 ----------

// newMemoryPlugin 构建 memory 插件 (多项目库目录模式)。
// baseDir: ~/.local/state/axis/memory (每项目一 .db, global.db 为全局库)。
// exportDir: 同下导出子目录 (每项目一子目录)。
func newMemoryPlugin(cfg *config.Config) (*memory.Plugin, error) {
	baseDir, exportDir := memory.DefaultPaths()
	if cfg.Memory.DBPath != "" {
		baseDir = filepath.Dir(cfg.Memory.DBPath) // DBPath 兼容: 取目录为库目录
	}
	if cfg.Memory.ExportDir != "" {
		exportDir = cfg.Memory.ExportDir
	}
	ext := cfg.Memory.Extension
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		return nil, err
	}
	return memory.NewPlugin(baseDir, exportDir, ext), nil
}

func langsList(cfg *config.Config) string {
	ls := make([]string, 0, len(cfg.Adapters))
	for k := range cfg.Adapters {
		ls = append(ls, k)
	}
	return "支持: " + strings.Join(ls, ", ")
}
