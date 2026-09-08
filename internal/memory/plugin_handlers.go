package memory

import (
	"context"
	"os"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Porter-Key/axis/internal/mcpkit"
)

// target 解析 handler 目标库: 显式 project 参数 (global 或绝对路径) 优先,
// 否则当前会话激活项目 (未激活 → ErrNotActivated)。
func (p *Plugin) target(ctx context.Context, args map[string]any) (*Service, error) {
	if proj := mcpkit.ArgStr(args, "project"); proj != "" {
		// 显式 project: 绝对路径或 global → 直接开该库 (不检查激活? 全局显式允许;
		// 非激活项目绝对路径也放行 — 语义: "全局需要显式", 绝对路径=显式指向)
		if proj == "global" {
			return p.svcFor("global")
		}
		return p.svcFor(proj)
	}
	// 缺省: 当前激活项目
	return p.svcForCtx(ctx)
}

func (p *Plugin) handleUpdate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	key := mcpkit.ArgStr(args, "key")
	svc, err := p.target(ctx, args)
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	// 无 text 参数 → 从导出文件读 (agent 先 opencode write/edit .axis/<key>.md)
	text := mcpkit.ArgStr(args, "text")
	if text == "" {
		path := svc.ExportPath(key)
		b, err := os.ReadFile(path)
		if err != nil {
			return mcpkit.ErrRes("读文件失败 (先 opencode write/edit .axis/" + key + ".md): " + err.Error()), nil
		}
		text = string(b)
	}
	res, err := svc.Update(key, text)
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	return p.okJSON("mem_update", res), nil
}

func (p *Plugin) handleRetrieve(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	key := mcpkit.ArgStr(args, "key")
	svc, err := p.target(ctx, args)
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	v, err := svc.Retrieve(key)
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	return p.okJSON("mem_retrieve", v), nil
}

func (p *Plugin) handleFind(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	q := mcpkit.ArgStr(args, "query")
	lim := mcpkit.ArgInt(args, "limit", 20)
	svc, err := p.target(ctx, args)
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	hits, err := svc.Find(q, lim)
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	return p.okJSON("mem_find", hits), nil
}

func (p *Plugin) handleList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	svc, err := p.target(ctx, req.GetArguments())
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	keys, err := svc.List()
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	return p.okJSON("mem_list", keys), nil
}

func (p *Plugin) handleField(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	key := mcpkit.ArgStr(args, "key")
	name := mcpkit.ArgStr(args, "name")
	value := mcpkit.ArgStr(args, "value")
	svc, err := p.target(ctx, args)
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	if err := svc.Field(key, name, value); err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	return p.okJSON("mem_field", map[string]any{"ok": true, "key": key, "field": name, "value": value}), nil
}

func (p *Plugin) handleLink(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	src := mcpkit.ArgStr(args, "src")
	dst := mcpkit.ArgStr(args, "dst")
	typ := mcpkit.ArgStr(args, "type")
	label := mcpkit.ArgStr(args, "label")
	svc, err := p.target(ctx, args)
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	if err := svc.Link(src, dst, typ, label); err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	return p.okJSON("mem_link", map[string]any{"ok": true, "src": src, "dst": dst, "type": typ}), nil
}

func (p *Plugin) handleExport(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	key := mcpkit.ArgStr(args, "key")
	svc, err := p.target(ctx, args)
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	if key == "" {
		n, err := svc.ExportAll()
		if err != nil {
			return mcpkit.ErrRes(err.Error()), nil
		}
		return p.okJSON("mem_export", map[string]any{"ok": true, "exported": n}), nil
	}
	if err := svc.ExportDoc(key); err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	return p.okJSON("mem_export", map[string]any{"ok": true, "exported": 1, "key": key, "path": svc.ExportPath(key)}), nil
}

func (p *Plugin) handleStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	key := mcpkit.ArgStr(args, "key")
	svc, err := p.target(ctx, args)
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	v, err := svc.Retrieve(key)
	if err != nil {
		return p.okJSON("mem_status", map[string]any{"key": key, "exists": false}), nil
	}
	return p.okJSON("mem_status", map[string]any{
		"key": key, "exists": true,
		"revision": v.Revision, "title": v.Title,
		"updated_at":  v.UpdatedAt,
		"export_path": svc.ExportPath(key),
		"fields":      v.Fields, "out_links": v.OutLinks, "in_links": v.InLinks,
	}), nil
}
