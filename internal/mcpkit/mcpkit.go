// Package mcpkit — 各插件共享的 MCP 工具小工具 (响应构造/参数解析)。
package mcpkit

import (
	"encoding/json"
	"path/filepath"

	"github.com/mark3labs/mcp-go/mcp"
)

// OkRes 文本成功响应。
func OkRes(s string) *mcp.CallToolResult { return mcp.NewToolResultText(s) }

// OkJSON JSON 成功响应 (直接文本化, axis MCP 客户端按纯 JSON 解析)。
// v 为 json.RawMessage 时不二次校验 (codegraph CLI 输出可能含前置/多段内容,
// 旧行为直接透传, 客户端自行解析)。
func OkJSON(v any) *mcp.CallToolResult {
	if raw, ok := v.(json.RawMessage); ok {
		return mcp.NewToolResultText(string(raw))
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ErrRes("序列化失败: " + err.Error())
	}
	return mcp.NewToolResultText(string(b))
}

// ErrRes 错误响应 (ERROR: 前缀, 客户端据此识别失败)。
func ErrRes(s string) *mcp.CallToolResult {
	return mcp.NewToolResultText("ERROR: " + s)
}

// ArgStr 取字符串参数 (不存在返回 "")。
func ArgStr(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// AbsArg 从工具参数取路径并校验绝对路径 (axis 铁律: 路径必须绝对)。
// 返回 (清理后的绝对路径, 是否合法)。
func AbsArg(args map[string]any, key string) (string, bool) {
	s, _ := args[key].(string)
	if s == "" {
		return s, false
	}
	if !filepath.IsAbs(s) {
		return s, false
	}
	return filepath.Clean(s), true
}

// ArgInt 取整数参数 (float64/缺省 → int)。
func ArgInt(args map[string]any, key string, def int) int {
	if f, ok := args[key].(float64); ok {
		return int(f)
	}
	return def
}
