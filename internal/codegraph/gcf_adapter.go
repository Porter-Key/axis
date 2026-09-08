package codegraph

import (
	"encoding/json"
	"fmt"
	"strconv"

	gcflib "github.com/blackwell-systems/gcf-go"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Porter-Key/axis/internal/mcpkit"
)

// ---------- codegraph 插件 GCF 适配器 (本插件内部自实现) ----------

// gcfProvenance 本插件 graph 画像的 Provenance 标记。
// 上游 gcf-go v1.7.1 的 Decode 要求 symbol 行 ≥5 字段, 空 Provenance 的 Encode
// 输出只有 4 字段导致自家 round-trip 失败, 此处统一打标记规避。
const gcfProvenance = "codegraph_cli"

// gcfMaxSymbols graph 画像节点上限 (防 impact 大图爆炸; 超限截断, 保中心+前 N)。
const gcfMaxSymbols = 200

// SetOutputFormat 注入输出编码开关 (axis 壳在 New 与 ctrlReload 调用; 原子, 热生效)。
func (p *Plugin) SetOutputFormat(format string) { p.outFormat.Store(format) }

// gcfEnabled 本插件 GCF 开关 (只认 "gcf", 缺省 json)。
func (p *Plugin) gcfEnabled() bool {
	v, _ := p.outFormat.Load().(string)
	return v == "gcf"
}

// okJSON JSON 输出类工具的统一出口; gcf 模式下经本插件适配器编码
// (callers/callees/impact → graph 画像, query/status → generic 画像),
// 编码失败静默回退原 JSON。
func (p *Plugin) okJSON(tool, kind, symbol string, res json.RawMessage) *mcp.CallToolResult {
	if p.gcfEnabled() {
		if s, ok := p.toGCF(tool, kind, symbol, res); ok {
			return mcpkit.OkRes(s)
		}
	}
	return mcpkit.OkJSON(res)
}

// toGCF 本插件 GCF 编码 (gcf-go 直调, 自包含在本文件)。
func (p *Plugin) toGCF(tool, kind, symbol string, res json.RawMessage) (string, bool) {
	switch kind {
	case "callers", "callees", "impact":
		return p.toGraph(tool, kind, symbol, res)
	default:
		return p.toGeneric(res)
	}
}

// toGeneric 通用画像 (query/status: 保字段全保真)。
func (p *Plugin) toGeneric(res json.RawMessage) (string, bool) {
	data, err := gcflib.ParseJSONOrdered(res)
	if err != nil {
		return "", false
	}
	s, err := gcflib.EncodeGenericChecked(data)
	if err != nil {
		return "", false
	}
	return s, true
}

// cgItem codegraph CLI 关系条目 (callers/callees/impact 共用形状)。
type cgItem struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	FilePath  string `json:"filePath"`
	StartLine int    `json:"startLine"`
}

// toGraph 图画像 (callers/callees/impact: 中心符号 + 关系边)。
func (p *Plugin) toGraph(tool, kind, symbol string, res json.RawMessage) (string, bool) {
	var doc struct {
		Symbol   string   `json:"symbol"`
		Callers  []cgItem `json:"callers"`
		Callees  []cgItem `json:"callees"`
		Affected []cgItem `json:"affected"`
	}
	if err := json.Unmarshal(res, &doc); err != nil {
		return "", false
	}
	center := symbol
	if doc.Symbol != "" {
		center = doc.Symbol
	}
	items := doc.Affected
	edge := "impacts"
	if kind == "callers" {
		items = doc.Callers
		edge = "calls"
	} else if kind == "callees" {
		items = doc.Callees
		edge = "calls"
	}
	symbols := []gcflib.Symbol{{QualifiedName: center, Kind: "symbol", Score: 1, Distance: 0, Provenance: gcfProvenance}}
	var edges []gcflib.Edge
	for i, it := range items {
		if len(symbols) >= gcfMaxSymbols {
			break
		}
		// 同名符号多处出现: QualifiedName 必须唯一 (边引用之), 后缀文件:行。
		id := fmt.Sprintf("%s@%s:%d", it.Name, it.FilePath, it.StartLine)
		symbols = append(symbols, gcflib.Symbol{
			QualifiedName: id, Kind: it.Kind, Score: 1 - float64(i+1)/float64(len(items)+1),
			Distance: 1, Provenance: gcfProvenance,
			Signature: it.FilePath + ":" + strconv.Itoa(it.StartLine),
		})
		if kind == "callees" {
			edges = append(edges, gcflib.Edge{Source: center, Target: id, EdgeType: edge})
		} else {
			edges = append(edges, gcflib.Edge{Source: id, Target: center, EdgeType: edge})
		}
	}
	out := gcflib.Encode(&gcflib.Payload{Tool: tool, Symbols: symbols, Edges: edges})
	// 自校验 (防上游编码/解码漂移, 失败回退原 JSON)
	back, err := gcflib.Decode(out)
	if err != nil || len(back.Symbols) != len(symbols) || len(back.Edges) != len(edges) {
		return "", false
	}
	return out, true
}
