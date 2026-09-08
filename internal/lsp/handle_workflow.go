package lsp

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	lsp_capacity "github.com/Porter-Key/axis/internal/lsp/capacity"
	"github.com/Porter-Key/axis/internal/lsp/langs"
	"github.com/Porter-Key/axis/internal/mcpkit"
)

// ---------- 智能 workflow 工具 handlers (契约层: 拼装各语言适配器实现) ----------

// workflowPrep 公共前置: 门禁 → 项目根 → 语言 → provider → 查询源。
func (l *LSP) workflowPrep(ctx context.Context, args map[string]any) (lang string, prov lsp_capacity.Provider, src *workflowSource, fail *mcp.CallToolResult) {
	path, ok := mcpkit.AbsArg(args, "path")
	if !ok {
		return "", nil, nil, mcpkit.ErrRes("path 必须为绝对路径: " + mcpkit.ArgStr(args, "path"))
	}
	if _, ok := l.requireProject(ctx, path); !ok {
		return "", nil, nil, mcpkit.ErrRes("未激活项目或文件不在激活项目内: 请先 axis_activate(project)")
	}
	root, ok := l.ensureProject(ctx, path)
	if !ok {
		return "", nil, nil, mcpkit.ErrRes("无法定位项目根: " + path)
	}
	lang = langs.Detect(l.getConfig(), path)
	if lang == "" {
		return "", nil, nil, mcpkit.ErrRes("无法识别语言: " + path)
	}
	prov, ok = lsp_capacity.Get(lsp_capacity.LanguageID(lang))
	if !ok {
		return "", nil, nil, mcpkit.ErrRes("无该语言 provider: " + lang)
	}
	return lang, prov, l.workflowSrc(root, lang), nil
}

// posArg 取行列参数 (客户端单位)。
func posArg(args map[string]any) (int, int) {
	line, _ := args["line"].(float64)
	ch, _ := args["character"].(float64)
	return int(line), int(ch)
}

// handleBlastRadius blast_radius: 影响面 batch (定义+引用分区+诊断)。
func (l *LSP) handleBlastRadius(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	path, _ := mcpkit.AbsArg(args, "path")
	_, prov, src, fail := l.workflowPrep(ctx, args)
	if fail != nil {
		return fail, nil
	}
	line, ch := posArg(args)
	out, err := prov.BlastRadius(ctx, src, path, line, ch)
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	return l.okJSON("blast_radius", out), nil
}

// handleExploreSymbol explore_symbol: 符号理解 batch。
func (l *LSP) handleExploreSymbol(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	path, _ := mcpkit.AbsArg(args, "path")
	_, prov, src, fail := l.workflowPrep(ctx, args)
	if fail != nil {
		return fail, nil
	}
	line, ch := posArg(args)
	out, err := prov.ExploreSymbol(ctx, src, path, line, ch)
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	return l.okJSON("explore_symbol", out), nil
}

// handleVerifyChain verify_chain: 修改后验证 batch。
func (l *LSP) handleVerifyChain(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	path, _ := mcpkit.AbsArg(args, "path")
	_, prov, src, fail := l.workflowPrep(ctx, args)
	if fail != nil {
		return fail, nil
	}
	out, err := prov.VerifyChain(ctx, src, path)
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	return l.okJSON("verify_chain", out), nil
}

// handleSimulateEdit simulate_edit: 安全编辑预览 batch (不落盘, 应用由 agent 落地)。
func (l *LSP) handleSimulateEdit(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	path, _ := mcpkit.AbsArg(args, "path")
	_, prov, src, fail := l.workflowPrep(ctx, args)
	if fail != nil {
		return fail, nil
	}
	edits, err := parseTextEdits(args)
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	out, err := prov.SimulateEdit(ctx, src, path, edits)
	if err != nil {
		return mcpkit.ErrRes(err.Error()), nil
	}
	return l.okJSON("simulate_edit", out), nil
}

// parseTextEdits 解析 edits 参数 (LSP TextEdit 数组, 客户端列单位)。
func parseTextEdits(args map[string]any) ([]lsp_capacity.TextEdit, error) {
	raw, ok := args["edits"].([]any)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("edits 必填 (非空数组, 每项 {startLine,startChar,endLine,endChar,newText})")
	}
	edits := make([]lsp_capacity.TextEdit, 0, len(raw))
	num := func(m map[string]any, k string) (int, bool) {
		f, ok := m[k].(float64)
		return int(f), ok
	}
	for i, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("edits[%d] 非对象", i)
		}
		sL, ok1 := num(m, "startLine")
		sC, ok2 := num(m, "startChar")
		eL, ok3 := num(m, "endLine")
		eC, ok4 := num(m, "endChar")
		nt, _ := m["newText"].(string)
		if !ok1 || !ok2 || !ok3 || !ok4 {
			return nil, fmt.Errorf("edits[%d] 缺行列数字字段", i)
		}
		edits = append(edits, lsp_capacity.TextEdit{StartLine: sL, StartChar: sC, EndLine: eL, EndChar: eC, NewText: nt})
	}
	return edits, nil
}
