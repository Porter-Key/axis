package lsp

import (
	"context"
	"encoding/json"
	"time"

	lsp_capacity "github.com/Porter-Key/axis/internal/lsp/capacity"
)

// ---------- 宿主 workflow 查询源 (通用编排原语, 无语言语义) ----------

// workflowSource 实现 lsp_capacity.WorkflowSource: 位置一律客户端单位进出
// (请求前 toServerPos, 响应后 adaptPositions); provider 只管语言语义。
type workflowSource struct {
	*appHoverSource
}

// workflowSrc 构造某项目/语言的 workflow 查询源。
func (l *LSP) workflowSrc(root, lang string) *workflowSource {
	return &workflowSource{appHoverSource: &appHoverSource{app: l, root: root, lang: lang}}
}

// Definition 定义查询 (客户端单位进出, 失败 ok=false)。
func (s *workflowSource) Definition(ctx context.Context, path string, line, char int) (json.RawMessage, bool) {
	sline, sch := s.app.toServerPos(s.lang, path, line, char)
	res, err := s.app.reg.CallWithDoc(ctx, s.root, s.lang, "textDocument/definition", path,
		map[string]any{"position": map[string]any{"line": sline, "character": sch}},
		s.app.reqTimeout(s.lang))
	if err != nil {
		return nil, false
	}
	return s.app.adaptPositions(s.lang, path, res), true
}

// References 引用查询 (与 get_references 同口径: 仅 position 参数)。
func (s *workflowSource) References(ctx context.Context, path string, line, char int) (json.RawMessage, bool) {
	sline, sch := s.app.toServerPos(s.lang, path, line, char)
	res, err := s.app.reg.CallWithDoc(ctx, s.root, s.lang, "textDocument/references", path,
		map[string]any{"position": map[string]any{"line": sline, "character": sch}},
		s.app.reqTimeout(s.lang))
	if err != nil {
		return nil, false
	}
	return s.app.adaptPositions(s.lang, path, res), true
}

// Hover 悬停查询 (返回原文经位置适配, provider 只取 markdown 解析签名)。
func (s *workflowSource) Hover(ctx context.Context, path string, line, char int) (json.RawMessage, bool) {
	sline, sch := s.app.toServerPos(s.lang, path, line, char)
	res, err := s.app.reg.CallWithDoc(ctx, s.root, s.lang, "textDocument/hover", path,
		map[string]any{"position": map[string]any{"line": sline, "character": sch}},
		s.app.reqTimeout(s.lang))
	if err != nil {
		return nil, false
	}
	return s.app.adaptPositions(s.lang, path, res), true
}

// Diagnostics 已整形诊断 (与 get_diagnostics 同口径: 轻触发 didOpen + 最多等 2s)。
func (s *workflowSource) Diagnostics(ctx context.Context, path string) (map[string]any, bool) {
	if _, err := s.app.reg.LSPConn(ctx, s.root, s.lang, true); err != nil {
		return nil, false
	}
	_, _ = s.app.reg.CallWithDoc(ctx, s.root, s.lang, "textDocument/hover", path,
		map[string]any{"position": map[string]any{"line": 0, "character": 0}}, 10*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if raw, ok := s.app.reg.DiagnosticsFor(s.root, s.lang, path); ok {
			return s.app.shapeDiagnostics(path, raw, true), true
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(200 * time.Millisecond):
		}
	}
	return s.app.shapeDiagnostics(path, nil, false), false
}

// Supports 服务器能力探测 (连接失败时乐观返回 true, 真实错误由各查询暴露)。
func (s *workflowSource) Supports(cap lsp_capacity.Capability) bool {
	conn, err := s.app.reg.LSPConn(context.Background(), s.root, s.lang, false)
	if err != nil || conn == nil {
		return true
	}
	key, ok := map[lsp_capacity.Capability]string{
		lsp_capacity.CapDefinition:     "textDocument.definitionProvider",
		lsp_capacity.CapImplementation: "textDocument.implementationProvider",
		lsp_capacity.CapTypeDefinition: "textDocument.typeDefinitionProvider",
	}[cap]
	if !ok {
		return true
	}
	return conn.Supports(key)
}

// PreviewText 合成 didChange (不落盘)。
func (s *workflowSource) PreviewText(ctx context.Context, path, text string) error {
	conn, err := s.app.reg.LSPConn(ctx, s.root, s.lang, true)
	if err != nil {
		return err
	}
	return conn.PreviewText(path, text)
}

// RestoreFile 磁盘内容恢复 (SyncFile 即 didChange 磁盘版 + 清预览标记)。
func (s *workflowSource) RestoreFile(ctx context.Context, path string) error {
	conn, err := s.app.reg.LSPConn(ctx, s.root, s.lang, true)
	if err != nil {
		return err
	}
	return conn.SyncFile(ctx, path)
}

// 编译期断言: 宿主源满足 workflow 契约。
var _ lsp_capacity.WorkflowSource = (*workflowSource)(nil)
