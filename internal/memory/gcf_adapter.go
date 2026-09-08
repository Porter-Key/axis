package memory

import (
	"encoding/json"

	gcflib "github.com/blackwell-systems/gcf-go"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Porter-Key/axis/internal/mcpkit"
)

// ---------- memory 插件 GCF 适配器 (本插件内部自实现) ----------

// outFormat 输出编码 (""/json/gcf, atomic.Value 存 string, 本插件 GCF 适配器开关)。
// 热重载: axis 壳经 SetOutputFormat 注入 (memory 库目录变更仍需重启, 开关即时生效)。
func (p *Plugin) SetOutputFormat(format string) { p.outFormat.Store(format) }

// gcfEnabled 本插件 GCF 开关 (只认 "gcf", 缺省 json)。
func (p *Plugin) gcfEnabled() bool {
	v, _ := p.outFormat.Load().(string)
	return v == "gcf"
}

// okJSON 成功响应统一出口; gcf 模式下经本插件适配器编为 generic 画像,
// 编码失败静默回退原 JSON。
func (p *Plugin) okJSON(tool string, v any) *mcp.CallToolResult {
	if p.gcfEnabled() {
		if s, ok := p.toGCF(tool, v); ok {
			return mcpkit.OkRes(s)
		}
	}
	return mcpkit.OkJSON(v)
}

// toGCF 本插件 generic 画像编码 (gcf-go 直调, 自包含在本文件)。
func (p *Plugin) toGCF(tool string, v any) (string, bool) {
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
