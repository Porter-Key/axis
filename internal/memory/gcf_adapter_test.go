package memory

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestMemoryGCFAdapter(t *testing.T) {
	p := &Plugin{}
	if p.gcfEnabled() {
		t.Fatal("缺省应关闭")
	}
	// json 模式: 原样 JSON
	res := p.okJSON("mem_list", []string{"a", "b"})
	text := res.Content[0].(mcp.TextContent).Text
	if text != `["a","b"]` {
		t.Fatalf("json 模式应原样: %q", text)
	}
	// gcf 模式: generic 画像
	p.SetOutputFormat("gcf")
	res = p.okJSON("mem_list", []string{"a", "b"})
	text = res.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "profile=generic") {
		t.Fatalf("非 generic 形状: %q", text)
	}
}
