package lsp

import (
	"encoding/json"

	gcflib "github.com/blackwell-systems/gcf-go"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Porter-Key/axis/internal/mcpkit"
)

// ---------- LSP 插件 GCF 适配器 (本插件内部自实现) ----------

// gcfEnabled 本插件 GCF 开关: 读热重载配置 output.format (只认 "gcf")。
func (l *LSP) gcfEnabled() bool { return l.cfg.Load().Output.Format == "gcf" }

// okJSON JSON 成功响应; gcf 模式下经本插件 GCF 适配器编为 generic 画像,
// 编码失败静默回退原 JSON (工具调用永不因编码而挂)。
func (l *LSP) okJSON(tool string, v any) *mcp.CallToolResult {
	if l.gcfEnabled() {
		if s, ok := l.toGCF(tool, v); ok {
			return mcpkit.OkRes(s)
		}
	}
	return mcpkit.OkJSON(v)
}

// toGCF 本插件 generic 画像编码 (gcf-go 直调, 自包含在本文件)。
// json.RawMessage 先保序解析再编码; tool 仅声明产出方 (graph 画像在 codegraph 插件)。
func (l *LSP) toGCF(tool string, v any) (string, bool) {
	_ = tool
	data := v
	if raw, ok := v.(json.RawMessage); ok {
		parsed, err := gcflib.ParseJSONOrdered(raw)
		if err != nil {
			return "", false
		}
		data = parsed
	}
	s, err := gcflib.EncodeGenericChecked(data)
	if err != nil {
		return "", false
	}
	return s, true
}
