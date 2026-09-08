package lsp

import (
	"time"

	"github.com/Porter-Key/axis/internal/lsp/fsmonitor"
	"github.com/Porter-Key/axis/internal/lsp/registry"
)

// ---------- 供 axis 壳 / 控制面调用的能力 ----------

// RegisterProject 注册项目 (控制面 /activate 用)。
func (l *LSP) RegisterProject(root string) {
	l.reg.RegisterProject(root)
}

// RegisterSession 注册会话。
func (l *LSP) RegisterSession(token, root, ua string) error {
	return l.reg.RegisterSession(token, root, ua)
}

// UnregisterSession 注销会话。
func (l *LSP) UnregisterSession(token string) {
	l.reg.UnregisterSession(token)
}

// Heartbeat 心跳。
func (l *LSP) Heartbeat(token string) bool { return l.reg.Heartbeat(token) }

// Status 状态。
func (l *LSP) Status() any { return l.reg.Status() }

// StartMonitor 为 root 启动 fsmonitor (幂等)。
func (l *LSP) StartMonitor(root string) {
	l.startMonitor(root)
}

// newTicker 供 monitorLoop (测试可替换)。
var newTicker = func() *time.Ticker { return time.NewTicker(500 * time.Millisecond) }

var _ = registry.Registry{}
var _ = fsmonitor.Monitor{}
