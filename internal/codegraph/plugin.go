// Package codegraph — axis 子插件: codegraph 代码图谱 (粗工具)。
//
// 自持 codegraph.Client, 注册 explore_code/query_symbols/find_callers/... 工具。
package codegraph

import (
	"context"
	"sync/atomic"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/Porter-Key/axis/internal/config"
	"github.com/Porter-Key/axis/internal/plugin"
)

// Plugin codegraph 插件实例。
type Plugin struct {
	// cg 热重载可换指针: atomic (handler 并发读, SetConfig 并发写)。
	cg   atomic.Pointer[Client]
	gate *plugin.Gate
}

// New 构建 codegraph 插件。
func NewPlugin(cfg config.CodegraphConfig) *Plugin {
	p := &Plugin{}
	p.cg.Store(New(cfg))
	return p
}

// Name 插件名。
func (p *Plugin) Name() string { return "codegraph" }

// SetConfig 热更新配置 (ctrlReload 路径): 原地重建 Client。
// handler 全部经 atomic Load 取当前 Client, 无需重注册;
// Client 无后台资源, 旧实例无需 Shutdown (在飞查询用旧实例跑完, 无中间状态)。
func (p *Plugin) SetConfig(cfg config.CodegraphConfig) {
	p.cg.Store(New(cfg))
}

// SetGate 注入会话门禁 (axis 壳调用)。
func (p *Plugin) SetGate(g *plugin.Gate) { p.gate = g }

// Register 注册 codegraph 工具。
func (p *Plugin) Register(ms *server.MCPServer) {
	ms.AddTool(mcp.NewTool("explore_code",
		mcp.WithDescription("陌生代码第一步（粗）：输入任务描述，一次返回相关符号源码+调用路径。精确落点再用 LSP 工具（get_definition/get_references）。"),
		mcp.WithString("root", mcp.Required(), mcp.Description("项目根 (绝对路径)")),
		mcp.WithString("query", mcp.Required(), mcp.Description("探索目标描述或符号名")),
	), p.handleExplore)

	ms.AddTool(mcp.NewTool("query_symbols",
		mcp.WithDescription("模糊搜符号（只记得名字片段/冷启动时用）。精确引用关系走 LSP get_references。"),
		mcp.WithString("root", mcp.Required()),
		mcp.WithString("query", mcp.Required()),
		mcp.WithString("kind", mcp.Description("过滤类型 function/class/...")),
	), p.handleJSON("query"))

	ms.AddTool(mcp.NewTool("find_callers",
		mcp.WithDescription("找调用方（粗召回：启发式图谱，局部变量/测试里的调用可能漏边）。动手改代码前用 LSP get_references 交叉验证。"),
		mcp.WithString("root", mcp.Required()),
		mcp.WithString("symbol", mcp.Required()),
	), p.handleJSON("callers"))

	ms.AddTool(mcp.NewTool("find_callees",
		mcp.WithDescription("找下游调用（粗召回，同 find_callers 的漏边说明）。精确校验走 LSP get_definition 逐点确认。"),
		mcp.WithString("root", mcp.Required()),
		mcp.WithString("symbol", mcp.Required()),
	), p.handleJSON("callees"))

	ms.AddTool(mcp.NewTool("analyze_impact",
		mcp.WithDescription("改动影响面粗筛（blast radius 初筛，可能含噪/漏边）。精确编辑点用 LSP get_references/get_rename 复核。"),
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
