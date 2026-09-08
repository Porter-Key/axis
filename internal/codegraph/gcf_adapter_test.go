package codegraph

import (
	"encoding/json"
	"strings"
	"testing"

	gcflib "github.com/blackwell-systems/gcf-go"
)

func TestToGraphCallers(t *testing.T) {
	p := &Plugin{}
	p.SetOutputFormat("gcf")
	if !p.gcfEnabled() {
		t.Fatal("gcf 应启用")
	}
	res := json.RawMessage(`{"symbol":"Register","callers":[
		{"name":"init","kind":"function","filePath":"internal/lsp/capacity/go_capacity_provider.go","startLine":16},
		{"name":"Register","kind":"method","filePath":"internal/lsp/plugin.go","startLine":74}]}`)
	s, ok := p.toGCF("find_callers", "callers", "Register", res)
	if !ok {
		t.Fatal("toGCF 失败")
	}
	if !strings.Contains(s, "profile=graph") || !strings.Contains(s, " calls") {
		t.Fatalf("非 graph 形状:\n%s", s)
	}
	back, err := gcflib.Decode(s)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(back.Symbols) != 3 || len(back.Edges) != 2 {
		t.Fatalf("节点/边数量异常: %d/%d", len(back.Symbols), len(back.Edges))
	}
}

func TestToGenericQuery(t *testing.T) {
	p := &Plugin{}
	res := json.RawMessage(`[{"node":{"qualifiedName":"A::B","kind":"method"},"score":0.9}]`)
	s, ok := p.toGCF("query_symbols", "query", "", res)
	if !ok {
		t.Fatal("toGCF 失败")
	}
	if !strings.Contains(s, "profile=generic") || !strings.Contains(s, "A::B") {
		t.Fatalf("非 generic 形状:\n%s", s)
	}
}

func TestToGCFBadJSONFallback(t *testing.T) {
	p := &Plugin{}
	if _, ok := p.toGCF("find_callers", "callers", "X", json.RawMessage(`{bad`)); ok {
		t.Fatal("坏 JSON 应返回失败 (调用方回退原样)")
	}
	if _, ok := p.toGCF("query_symbols", "query", "", json.RawMessage(`{bad`)); ok {
		t.Fatal("坏 JSON 应返回失败 (调用方回退原样)")
	}
}

func TestGcfDisabledByDefault(t *testing.T) {
	p := &Plugin{}
	if p.gcfEnabled() {
		t.Fatal("缺省应关闭")
	}
}
