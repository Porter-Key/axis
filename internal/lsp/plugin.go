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
		mcp.WithDescription("返回文件某位置符号的定义位置。scope 可选: definition(默认)|implementation|typeDefinition, 按语言服务器能力降级。"),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件绝对路径")),
		mcp.WithNumber("line", mcp.Required(), mcp.Description("行号 (0-based)")),
		mcp.WithNumber("character", mcp.Required(), mcp.Description("列号 (0-based, UTF-8 字节偏移)")),
		mcp.WithString("scope", mcp.Description("查询范围: definition|implementation|typeDefinition (默认 definition)")),
	), l.handleDefinition)

	ms.AddTool(mcp.NewTool("get_hover",
		mcp.WithDescription("返回文件某位置的悬停信息 (类型/文档)。"),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件绝对路径")),
		mcp.WithNumber("line", mcp.Required(), mcp.Description("行号 (0-based)")),
		mcp.WithNumber("character", mcp.Required(), mcp.Description("列号 (0-based, UTF-8 字节偏移)")),
	), l.handleLSPRequest("hover"))

	ms.AddTool(mcp.NewTool("get_references",
		mcp.WithDescription("返回符号在项目内的全部引用位置 (落点附签名块)。"),
		mcp.WithString("path", mcp.Required()),
		mcp.WithNumber("line", mcp.Required(), mcp.Description("行号 (0-based)")),
		mcp.WithNumber("character", mcp.Required(), mcp.Description("列号 (0-based, UTF-8 字节偏移)")),
	), l.handleLSPRequest("references"))

	ms.AddTool(mcp.NewTool("get_diagnostics",
		mcp.WithDescription("返回文件当前全部诊断 (编译错误/警告)。"),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件绝对路径 (可目录)")),
	), l.handleDiagnostics)

	ms.AddTool(mcp.NewTool("get_rename",
		mcp.WithDescription("计算符号重命名的影响范围 (编辑点列表)。"),
		mcp.WithString("path", mcp.Required()),
		mcp.WithNumber("line", mcp.Required(), mcp.Description("行号 (0-based)")),
		mcp.WithNumber("character", mcp.Required(), mcp.Description("列号 (0-based, UTF-8 字节偏移)")),
		mcp.WithString("newName", mcp.Required()),
	), l.handleRename)

	ms.AddTool(mcp.NewTool("get_symbols",
		mcp.WithDescription("列出文件/工作区符号。path 为文件则文件内符号, 为目录则 workspace/symbol。"),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件或目录绝对路径")),
		mcp.WithString("query", mcp.Description("符号搜索词 (workspace 模式)")),
	), l.handleSymbols)
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
