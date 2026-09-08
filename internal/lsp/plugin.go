// Package lsp — axis 子插件: 多语言 LSP 语义工具 (精) + 项目监控。
//
// 自持: registry (长连接池) + cfg + fsmonitor monitors (磁盘自动同步)。
// 工具: get_definition/get_hover/get_references/get_symbols/get_diagnostics/get_rename。
package lsp

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/Porter-Key/axis/internal/config"
	"github.com/Porter-Key/axis/internal/lsp/fsmonitor"
	"github.com/Porter-Key/axis/internal/lsp/registry"
	"github.com/Porter-Key/axis/internal/plugin"
)

// LSP 插件实例 (自治: 持自己的全部状态)。
type LSP struct {
	// cfg 热重载可换指针: atomic (handler 并发读, SetConfig 并发写)。
	cfg  atomic.Pointer[config.Config]
	reg  *registry.Registry
	gate *plugin.Gate

	monitors map[string]*fsmonitor.Monitor // root -> monitor
	muMon    sync.Mutex

	monCtx    context.Context
	monCancel context.CancelFunc
	monWg     sync.WaitGroup
}

// New 构建 LSP 插件。
func New(cfg *config.Config) *LSP {
	l := &LSP{
		reg:      registry.New(cfg),
		monitors: map[string]*fsmonitor.Monitor{},
	}
	l.cfg.Store(cfg)
	return l
}

// Name 插件名。
func (l *LSP) Name() string { return "lsp" }

// SetGate 注入会话门禁 (axis 壳调用)。
func (l *LSP) SetGate(g *plugin.Gate) { l.gate = g }

// Gate 暴露 (测试/控制面)。
func (l *LSP) Gate() *plugin.Gate { return l.gate }

// Registry 暴露 registry (内部互操作/测试)。
func (l *LSP) Registry() *registry.Registry { return l.reg }

// Config 暴露配置 (reload 用)。
func (l *LSP) Config() *config.Config { return l.cfg.Load() }

// getConfig 取当前配置指针 (handler 读路径统一入口, atomic Load)。
func (l *LSP) getConfig() *config.Config { return l.cfg.Load() }

// SetConfig 更新配置 (热重载)。
func (l *LSP) SetConfig(cfg *config.Config) {
	if cfg == nil {
		return
	}
	l.cfg.Store(cfg)
	l.reg.SetConfig(cfg) // 透传 registry, 否则 adapters/pool 改了不生效
}

// Register 注册 LSP 工具。
func (l *LSP) Register(ms *server.MCPServer) {
	ms.AddTool(mcp.NewTool("get_definition",
		mcp.WithDescription("精确查定义（LSP 类型级：校验 codegraph 结论的金标准）。scope 可选: definition(默认)|implementation|typeDefinition, 按语言服务器能力降级。"),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件绝对路径")),
		mcp.WithNumber("line", mcp.Required(), mcp.Description("行号 (0-based)")),
		mcp.WithNumber("character", mcp.Required(), mcp.Description("列号 (0-based, UTF-8 字节偏移)")),
		mcp.WithString("scope", mcp.Description("查询范围: definition|implementation|typeDefinition (默认 definition)")),
	), l.handleDefinition)

	ms.AddTool(mcp.NewTool("get_hover",
		mcp.WithDescription("精确查悬停签名/文档（LSP 类型级：读不懂的符号先看这里，再决定跟 definition）。"),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件绝对路径")),
		mcp.WithNumber("line", mcp.Required(), mcp.Description("行号 (0-based)")),
		mcp.WithNumber("character", mcp.Required(), mcp.Description("列号 (0-based, UTF-8 字节偏移)")),
	), l.handleLSPRequest("hover"))

	ms.AddTool(mcp.NewTool("get_references",
		mcp.WithDescription("精确查全部引用（LSP 类型级）。codegraph find_callers 可能漏边，动手改代码以本工具为准。"),
		mcp.WithString("path", mcp.Required()),
		mcp.WithNumber("line", mcp.Required(), mcp.Description("行号 (0-based)")),
		mcp.WithNumber("character", mcp.Required(), mcp.Description("列号 (0-based, UTF-8 字节偏移)")),
	), l.handleLSPRequest("references"))

	ms.AddTool(mcp.NewTool("get_diagnostics",
		mcp.WithDescription("文件诊断（LSP 推送语义：返回服务器已推送的诊断，需文件被查询打开过；cached=false 表示尚无推送≠零报错）。"),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件绝对路径 (可目录)")),
	), l.handleDiagnostics)

	ms.AddTool(mcp.NewTool("get_rename",
		mcp.WithDescription("精确算重命名编辑点（LSP，只计算不应用；改名前先用它拿全量清单）。"),
		mcp.WithString("path", mcp.Required()),
		mcp.WithNumber("line", mcp.Required(), mcp.Description("行号 (0-based)")),
		mcp.WithNumber("character", mcp.Required(), mcp.Description("列号 (0-based, UTF-8 字节偏移)")),
		mcp.WithString("newName", mcp.Required()),
	), l.handleRename)

	ms.AddTool(mcp.NewTool("get_symbols",
		mcp.WithDescription("精确列符号（LSP）：文件内符号一览，或目录 workspace 符号（需项目已建索引/服务器已启动）。定位前先用它拿新鲜行号。"),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件或目录绝对路径")),
		mcp.WithString("query", mcp.Description("符号搜索词 (workspace 模式)")),
	), l.handleSymbols)

	// 智能 workflow (batch: 一次调用完成多步编排; 各语言适配器在自己文件内自实现;
	// 配置 output.format=gcf 时输出 GCF generic 画像)。
	ms.AddTool(mcp.NewTool("blast_radius",
		mcp.WithDescription("智能 workflow·影响面（批）：定义(+签名)+全部引用（测试/非测试分区）+诊断摘要，一次返回。替代 20+ 次零散 LSP 调用；动手改代码以 get_references 复核。"),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件绝对路径")),
		mcp.WithNumber("line", mcp.Required(), mcp.Description("行号 (0-based)")),
		mcp.WithNumber("character", mcp.Required(), mcp.Description("列号 (0-based, UTF-8 字节偏移)")),
	), l.handleBlastRadius)

	ms.AddTool(mcp.NewTool("explore_symbol",
		mcp.WithDescription("智能 workflow·符号理解（批）：hover 签名+定义落点+引用计数/前 N 条，一把梭。先看它再决定跟 definition。"),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件绝对路径")),
		mcp.WithNumber("line", mcp.Required(), mcp.Description("行号 (0-based)")),
		mcp.WithNumber("character", mcp.Required(), mcp.Description("列号 (0-based, UTF-8 字节偏移)")),
	), l.handleExploreSymbol)

	ms.AddTool(mcp.NewTool("verify_chain",
		mcp.WithDescription("智能 workflow·修改后验证（批）：文件诊断+构建/测试提示（build/test 由你跑）。每次落盘改代码后调一次。"),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件绝对路径")),
	), l.handleVerifyChain)

	ms.AddTool(mcp.NewTool("simulate_edit",
		mcp.WithDescription("智能 workflow·安全编辑预览（批）：edits 作用于内存合成内容（不落盘）→诊断 diff→自动恢复。只预览不应用；应用由你落盘。"),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件绝对路径")),
		mcp.WithArray("edits", mcp.Required(), mcp.Description("编辑数组，每项 {startLine,startChar,endLine,endChar,newText} (0-based 行，列为 UTF-8 字节偏移，相对磁盘内容)")),
	), l.handleSimulateEdit)
}

// Start 启动 monitor 消费循环。
func (l *LSP) Start(ctx context.Context) error {
	l.monCtx, l.monCancel = context.WithCancel(ctx)
	l.monWg.Add(1)
	go l.monitorLoop(l.monCtx)
	return nil
}

// Shutdown 释放: cancel monitor + 关 registry 池。
func (l *LSP) Shutdown(ctx context.Context) error {
	if l.monCancel != nil {
		l.monCancel()
		l.monWg.Wait()
	}
	l.reg.Shutdown()
	l.muMon.Lock()
	for _, m := range l.monitors {
		m.Close()
	}
	l.muMon.Unlock()
	return nil
}

// monitorLoop 统一轮询所有项目 monitor: 变更路径 → reg.InvalidateProject (LSP 重索引)。
func (l *LSP) monitorLoop(ctx context.Context) {
	defer l.monWg.Done()
	ticker := newTicker()
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.muMon.Lock()
			type mr struct {
				root string
				m    *fsmonitor.Monitor
			}
			list := make([]mr, 0, len(l.monitors))
			for root, m := range l.monitors {
				list = append(list, mr{root, m})
			}
			l.muMon.Unlock()
			for _, item := range list {
				if paths := item.m.ConsumeChangedPaths(); len(paths) > 0 {
					l.reg.InvalidateProject(item.root, paths)
				}
			}
		}
	}
}
