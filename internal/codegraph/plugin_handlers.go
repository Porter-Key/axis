package codegraph

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/Porter-Key/axis/internal/mcpkit"
)

// requireRoot 门禁: root 必须等于当前会话激活项目 (严格绑定, 不能跨项目)。
func (p *Plugin) requireRoot(ctx context.Context, root string) bool {
	if p.gate == nil {
		return false
	}
	proj, ok := p.gate.ProjectFor(ctx)
	if !ok {
		return false
	}
	return filepath.Clean(proj) == filepath.Clean(root)
}

// handleExplore explore_code: 粗探索 (任务描述 → 相关符号+调用路径)。
func (p *Plugin) handleExplore(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	root, ok := mcpkit.AbsArg(args, "root")
	if !ok {
		return mcpkit.ErrRes("root 必须为绝对路径: " + mcpkit.ArgStr(args, "root")), nil
	}
	if !p.requireRoot(ctx, root) {
		return mcpkit.ErrRes("root 与激活项目不符或未激活: 请先 axis_activate(project)"), nil
	}
	q, _ := args["query"].(string)
	if q == "" {
		return mcpkit.ErrRes("query 必填"), nil
	}
	if err := p.cg.Load().EnsureIndex(root); err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	out, err := p.cg.Load().Explore(ctx, root, strings.Fields(q))
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	return mcpkit.OkRes(out), nil
}

// handleJSON JSON 输出类工具 (query/callers/callees/impact/status)。
func (p *Plugin) handleJSON(kind string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		root, ok := mcpkit.AbsArg(args, "root")
		if !ok {
			return mcpkit.ErrRes("root 必须为绝对路径: " + mcpkit.ArgStr(args, "root")), nil
		}
		if !p.requireRoot(ctx, root) {
			return mcpkit.ErrRes("root 与激活项目不符或未激活: 请先 axis_activate(project)"), nil
		}
		if err := p.cg.Load().EnsureIndex(root); err != nil {
			return mcpkit.ErrRes(err.Error()), nil
		}
		var res json.RawMessage
		var err error
		switch kind {
		case "query":
			q, _ := args["query"].(string)
			kindF, _ := args["kind"].(string)
			res, err = p.cg.Load().Query(ctx, root, q, kindF, 20)
		case "callers":
			s, _ := args["symbol"].(string)
			res, err = p.cg.Load().Callers(ctx, root, s)
		case "callees":
			s, _ := args["symbol"].(string)
			res, err = p.cg.Load().Callees(ctx, root, s)
		case "impact":
			s, _ := args["symbol"].(string)
			res, err = p.cg.Load().Impact(ctx, root, s)
		case "status":
			res, err = p.cg.Load().Status(ctx, root)
		}
		if err != nil {
			return mcpkit.ErrRes(err.Error()), nil
		}
		return mcpkit.OkJSON(res), nil
	}
}

// handleText 文本输出类工具 (files)。
func (p *Plugin) handleText(cmd string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		root, ok := mcpkit.AbsArg(args, "root")
		if !ok {
			return mcpkit.ErrRes("root 必须为绝对路径: " + mcpkit.ArgStr(args, "root")), nil
		}
		if !p.requireRoot(ctx, root) {
			return mcpkit.ErrRes("root 与激活项目不符或未激活: 请先 axis_activate(project)"), nil
		}
		if err := p.cg.Load().EnsureIndex(root); err != nil {
			return mcpkit.ErrRes(err.Error()), nil
		}
		out, err := p.cg.Load().RunText(ctx, root, cmd)
		if err != nil {
			return mcpkit.ErrRes(err.Error()), nil
		}
		return mcpkit.OkRes(out), nil
	}
}
