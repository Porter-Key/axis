// Package codegraph — axis 子插件: codegraph 代码图谱 (粗工具)。
//
// 自持 codegraph.Client, 注册 explore_code/query_symbols/find_callers/... 工具。
package codegraph

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/Porter-Key/axis/internal/config"
	"github.com/Porter-Key/axis/internal/plugin"
)

// Plugin codegraph 插件实例。
type Plugin struct {
	cfg config.CodegraphConfig
	cg  *Client
	gate *plugin.Gate
}

// New 构建 codegraph 插件。
func NewPlugin(cfg config.CodegraphConfig) *Plugin {
	return &Plugin{cfg: cfg, cg: New(cfg)}
}

// Name 插件名。
func (p *Plugin) Name() string { return "codegraph" }

// SetGate 注入会话门禁 (axis 壳调用)。
func (p *Plugin) SetGate(g *plugin.Gate) { p.gate = g }

// Register 注册 codegraph 工具。
func (p *Plugin) Register(ms *server.MCPServer) {
	ms.AddTool(mcp.NewTool("explore_code",
		mcp.WithDescription("codegraph 粗探索: 输入任务描述, 返回相关符号源码+调用路径 (一次一文件视角)。"),
		mcp.WithString("root", mcp.Required(), mcp.Description("项目根 (绝对路径)")),
		mcp.WithString("query", mcp.Required(), mcp.Description("探索目标描述或符号名")),
	), p.handleExplore)

	ms.AddTool(mcp.NewTool("query_symbols",
		mcp.WithDescription("codegraph 符号搜索 (模糊, JSON)。"),
		mcp.WithString("root", mcp.Required()),
		mcp.WithString("query", mcp.Required()),
		mcp.WithString("kind", mcp.Description("过滤类型 function/class/...")),
	), p.handleJSON("query"))

	ms.AddTool(mcp.NewTool("find_callers",
		mcp.WithDescription("找出所有调用某符号的调用方。"),
		mcp.WithString("root", mcp.Required()),
		mcp.WithString("symbol", mcp.Required()),
	), p.handleJSON("callers"))

	ms.AddTool(mcp.NewTool("find_callees",
		mcp.WithDescription("找出某符号调用的全部下游。"),
		mcp.WithString("root", mcp.Required()),
		mcp.WithString("symbol", mcp.Required()),
	), p.handleJSON("callees"))

	ms.AddTool(mcp.NewTool("analyze_impact",
		mcp.WithDescription("分析改动某符号会影响哪些代码 (blast radius)。"),
		mcp.WithString("root", mcp.Required()),
		mcp.WithString("symbol", mcp.Required()),
	), p.handleJSON("impact"))

	ms.AddTool(mcp.NewTool("index_status",
		mcp.WithDescription("查看项目 codegraph 索引状态。"),
		mcp.WithString("root", mcp.Required()),
	), p.handleJSON("status"))

	ms.AddTool(mcp.NewTool("list_files",
		mcp.WithDescription("列出项目索引内文件结构 (codegraph files)。"),
		mcp.WithString("root", mcp.Required()),
	), p.handleText("files"))
}

// Start 无后台资源。
func (p *Plugin) Start(ctx context.Context) error { return nil }

// Shutdown 无资源。
func (p *Plugin) Shutdown(ctx context.Context) error { return nil }
