// Package plugin — axis 插件契约与共享门禁。
//
// 架构 (用户确认): 一个服务 + 多个子插件。LSP / codegraph / memory 各自是
// 独立子模块 (ToolProvider), 互不 import; 由 axis 壳统一注册进同一 MCP server。
//
// 门禁 (用户方案): 项目自包含。agent 必须先调 axis_activate(project) 激活当前
// 会话, 之后所有目录级工具才可用; 未激活 → 拒绝。换目录必须重新激活。
package plugin

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/mark3labs/mcp-go/server"
)

// GateSetter 插件可选实现: 接收会话门禁 (目录级工具激活检查用)。
// axis 壳在 Register 前调用 SetGate。
type GateSetter interface {
	SetGate(g *Gate)
}

// ToolProvider 一个子模块插件: 注册自己的工具集, 可选生命周期钩子。
type ToolProvider interface {
	// Name 插件名 (日志/诊断用)。
	Name() string
	// Register 把自己的全部工具注册到 MCP server。
	Register(srv *server.MCPServer)
	// Start 可选: 启动后台资源 (monitor loop 等)。在 Register 后调用。
	Start(ctx context.Context) error
	// Shutdown 可选: 释放资源 (LSP 池、DB、watcher)。
	Shutdown(ctx context.Context) error
}

// Gate 会话→激活项目门禁。axis 壳持有, 传入各插件; 插件 handler 在目录级工具
// 入口调用 ProjectFor(ctx) 取当前会话激活的项目。未激活 → ok=false, 拒绝服务。
type Gate struct {
	mu   sync.RWMutex
	proj map[string]string // sessionID → 项目根 (绝对路径, Clean)
}

// NewGate 建空门禁。
func NewGate() *Gate {
	return &Gate{proj: map[string]string{}}
}

// Activate 绑定 sessionID → project (axis_activate 工具调用; 重复=换目录重激活)。
func (g *Gate) Activate(sessionID, project string) {
	abs, err := filepath.Abs(project)
	if err != nil {
		abs = project
	}
	g.mu.Lock()
	g.proj[sessionID] = filepath.Clean(abs)
	g.mu.Unlock()
}

// Deactivate 解绑 (会话结束/显式释放)。
func (g *Gate) Deactivate(sessionID string) {
	g.mu.Lock()
	delete(g.proj, sessionID)
	g.mu.Unlock()
}

// ProjectFor 取 ctx 会话的激活项目。从 mcp-go context 取 ClientSession.SessionID。
func (g *Gate) ProjectFor(ctx context.Context) (project string, ok bool) {
	sid := SessionIDFromContext(ctx)
	if sid == "" {
		return "", false
	}
	g.mu.RLock()
	p, ok := g.proj[sid]
	g.mu.RUnlock()
	return p, ok
}

// ActiveProject 显式给定 sessionID 查 (控制面/测试)。
func (g *Gate) ActiveProject(sessionID string) (string, bool) {
	g.mu.RLock()
	p, ok := g.proj[sessionID]
	g.mu.RUnlock()
	return p, ok
}

// SessionIDFromContext 从 MCP handler context 取当前客户端会话 ID。
// mcp-go 在 HTTP transport 下把 ClientSession (含 SessionID) 放入 ctx。
func SessionIDFromContext(ctx context.Context) string {
	if s := server.ClientSessionFromContext(ctx); s != nil {
		return s.SessionID()
	}
	return ""
}
