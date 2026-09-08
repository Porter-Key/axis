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
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/Porter-Key/axis/internal/logger"
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
//
// TTL: 绑定条目带滑动过期 (默认 defaultGateTTL; SetTTL 覆盖, <=0=不过期)。
// 每次 Activate/ProjectFor/ActiveProject 刷新 lastSeen, 过期条目惰性清扫
// (Activate 时顺带扫全表, 读路径只判自己)。不断掉长连 MCP 会话的误杀:
// 每次目录级工具调用都经 ProjectFor, 活跃会话永不过期。
type Gate struct {
	mu   sync.RWMutex
	proj map[string]gateEntry // sessionID → 条目 (项目根为绝对路径, Clean)
	ttl  time.Duration
}

// gateEntry 一条激活绑定。
type gateEntry struct {
	project  string
	lastSeen time.Time
}

// defaultGateTTL 默认绑定 TTL (App 会用 Heartbeat.TimeoutSec 覆盖, 与 registry 会话回收同口径)。
const defaultGateTTL = 24 * time.Hour

// NewGate 建空门禁 (启动时尝试从落盘恢复历史绑定, 过期与否由 lookup 惰性判定)。
func NewGate() *Gate {
	g := &Gate{proj: map[string]gateEntry{}, ttl: defaultGateTTL}
	g.load()
	return g
}

// SetTTL 覆盖绑定 TTL (<=0 = 不过期, 仅测试用; 线上必须由 App 传入心跳超时)。
func (g *Gate) SetTTL(d time.Duration) {
	g.mu.Lock()
	g.ttl = d
	g.mu.Unlock()
}

// Activate 绑定 sessionID → project (axis_activate 工具调用; 重复=换目录重激活)。
func (g *Gate) Activate(sessionID, project string) {
	abs, err := filepath.Abs(project)
	if err != nil {
		abs = project
	}
	now := time.Now()
	g.mu.Lock()
	g.proj[sessionID] = gateEntry{project: filepath.Clean(abs), lastSeen: now}
	// 惰性清扫: 激活是低频操作, 顺带清掉过期条目 (防 sessionID 长期累积)。
	if g.ttl > 0 {
		for sid, e := range g.proj {
			if now.Sub(e.lastSeen) > g.ttl {
				delete(g.proj, sid)
			}
		}
	}
	snap := g.snapshotLocked()
	g.mu.Unlock()
	g.persist(snap)
}

// Deactivate 解绑 (会话结束/显式释放)。
func (g *Gate) Deactivate(sessionID string) {
	g.mu.Lock()
	delete(g.proj, sessionID)
	snap := g.snapshotLocked()
	g.mu.Unlock()
	g.persist(snap)
}

// ProjectFor 取 ctx 会话的激活项目。从 mcp-go context 取 ClientSession.SessionID。
// 命中即刷新 lastSeen (滑动窗口); 过期条目视为未激活。
func (g *Gate) ProjectFor(ctx context.Context) (project string, ok bool) {
	sid := SessionIDFromContext(ctx)
	if sid == "" {
		return "", false
	}
	return g.lookup(sid)
}

// ActiveProject 显式给定 sessionID 查 (控制面/测试)。同样刷新滑动窗口。
func (g *Gate) ActiveProject(sessionID string) (string, bool) {
	return g.lookup(sessionID)
}

// lookup 查绑定 (读锁 fast path; 命中且需刷新时升级写锁)。
func (g *Gate) lookup(sessionID string) (string, bool) {
	g.mu.RLock()
	e, ok := g.proj[sessionID]
	ttl := g.ttl
	g.mu.RUnlock()
	if !ok {
		return "", false
	}
	if ttl > 0 && time.Since(e.lastSeen) > ttl {
		g.mu.Lock()
		// 复检 (并发下可能已被重激活刷新)
		if cur, still := g.proj[sessionID]; still && time.Since(cur.lastSeen) > ttl {
			delete(g.proj, sessionID)
		}
		g.mu.Unlock()
		return "", false
	}
	g.mu.Lock()
	if cur, still := g.proj[sessionID]; still && cur.lastSeen == e.lastSeen {
		cur.lastSeen = time.Now()
		g.proj[sessionID] = cur
	}
	g.mu.Unlock()
	return e.project, true
}

// SessionIDFromContext 从 MCP handler context 取当前客户端会话 ID。
// mcp-go 在 HTTP transport 下把 ClientSession (含 SessionID) 放入 ctx。
func SessionIDFromContext(ctx context.Context) string {
	if s := server.ClientSessionFromContext(ctx); s != nil {
		return s.SessionID()
	}
	return ""
}

// ---------- 会话落盘恢复 (重启不掉“上次去哪了”) ----------

// gateStatePath 落盘路径 (与 memory 同口径: XDG_STATE_HOME 或 ~/.local/state/axis/)。
func gateStatePath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		if h, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(h, ".local", "state")
		}
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, "axis", "gate.json")
}

// gateSnapshot 落盘形状 (lastSeen 用 unix 纳秒, 同秒多激活可比先后)。
type gateSnapshot struct {
	SavedAt  int64                    `json:"saved_at"`
	Sessions map[string]gateEntryJSON `json:"sessions"`
}

// gateEntryJSON 单条绑定的落盘形态 (具名防匿名结构标签漂移)。
type gateEntryJSON struct {
	Project  string `json:"project"`
	LastSeen int64  `json:"last_seen_nano"`
}

// snapshotLocked 当前全表快照 (调用方须持有至少读锁)。
func (g *Gate) snapshotLocked() gateSnapshot {
	snap := gateSnapshot{SavedAt: time.Now().Unix(), Sessions: map[string]gateEntryJSON{}}
	for sid, e := range g.proj {
		snap.Sessions[sid] = gateEntryJSON{Project: e.project, LastSeen: e.lastSeen.UnixNano()}
	}
	return snap
}

// persist 原子落盘 (tmp+rename; 失败只记日志, 不影响内存门禁)。
func (g *Gate) persist(snap gateSnapshot) {
	path := gateStatePath()
	if path == "" {
		return
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		logger.With().Warn("gate 落盘建目录失败", "error", err.Error())
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		logger.With().Warn("gate 落盘写失败", "error", err.Error())
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		logger.With().Warn("gate 落盘提交失败", "error", err.Error())
	}
}

// load 启动恢复 (解析失败/无文件=空门禁; 过期与否由 lookup 惰性判定, 这里全留)。
func (g *Gate) load() {
	path := gateStatePath()
	if path == "" {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var snap gateSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		logger.With().Warn("gate 恢复解析失败 (用空门禁)", "error", err.Error())
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for sid, e := range snap.Sessions {
		if sid == "" || e.Project == "" {
			continue
		}
		g.proj[sid] = gateEntry{project: e.Project, lastSeen: time.Unix(0, e.LastSeen)}
		n++
	}
	if n > 0 {
		logger.With().Debug("gate 恢复历史绑定", "count", n)
	}
}

// RecoveryHint 上次活跃项目提示 (落盘恢复的用途: sessionID 重启后会变,
// 严格绑定无法自动恢复, 但拒绝时给出精确恢复命令, agent 一调即回)。
// 无历史 → ""。nil 接收者安全。
func (g *Gate) RecoveryHint() string {
	if g == nil {
		return ""
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	var best string
	var bestSeen time.Time
	for _, e := range g.proj {
		if e.project == "" {
			continue
		}
		if best == "" || e.lastSeen.After(bestSeen) {
			best, bestSeen = e.project, e.lastSeen
		}
	}
	if best == "" {
		return ""
	}
	return "上次激活的项目是 " + best + "；调 axis_activate(project=" + best + ") 恢复绑定"
}

// RejectMsg 拒绝消息组装: detail + 恢复提示 (nil 接收者安全)。
func (g *Gate) RejectMsg(detail string) string {
	if h := g.RecoveryHint(); h != "" {
		return detail + "。" + h
	}
	return detail
}
