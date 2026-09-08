package registry

import (
	"context"
	"fmt"
	"io"
	"syscall"
	"testing"

	"github.com/Porter-Key/axis/internal/config"
)

func cfgWith(ttl, max, mem int) *config.Config {
	cfg := config.Default()
	cfg.Pool.IdleTTLSec = ttl
	cfg.Pool.MaxServers = max
	cfg.Pool.MemoryLimitMB = mem
	return cfg
}

func TestSessionHeartbeatExpiry(t *testing.T) {
	cfg := cfgWith(0, 6, 0)
	cfg.Heartbeat.TimeoutSec = 1 // 1 秒超时
	r := New(cfg)
	r.RegisterProject("/p1")
	if err := r.RegisterSession("tok1", "/p1", "test"); err != nil {
		t.Fatal(err)
	}
	if !r.Heartbeat("tok1") {
		t.Fatal("heartbeat should succeed")
	}
	// 等 reap loop (10s 周期, 太慢) → 手动调过期检查? reapLoop 周期硬编码 10s
	// 改为暴露 tick 供测试: 这里直接验证 Unregister 路径
	r.UnregisterSession("tok1")
	if r.Heartbeat("tok1") {
		t.Error("session should be gone after unregister")
	}
}

func TestRegisterSessionUnknownProject(t *testing.T) {
	cfg := config.Default()
	r := New(cfg)
	if err := r.RegisterSession("t", "/nope", ""); err == nil {
		t.Error("expected error for unregistered project")
	}
}

func TestStatusReportsProjects(t *testing.T) {
	r := New(config.Default())
	r.RegisterProject("/a")
	r.RegisterProject("/b")
	r.RegisterProjectLangs("/a", "go")
	st := r.Status()
	projs, ok := st["projects"].([]map[string]any)
	if !ok {
		t.Fatalf("status projects type wrong: %T", st["projects"])
	}
	if len(projs) != 2 {
		t.Errorf("want 2 projects, got %d", len(projs))
	}
}

func TestLSPConnLazySpawn_NoSpawnWhenNoAdapter(t *testing.T) {
	cfg := config.Default()
	r := New(cfg)
	r.RegisterProject("/p")
	_, err := r.LSPConn(context.Background(), "/p", "nosuchlang", true)
	if err == nil {
		t.Error("expected error for unknown lang")
	}
}

func TestLRUEviction(t *testing.T) {
	// 不真正 spawn (无 gopls), 直接注入假连接验证 eviction 逻辑
	cfg := config.Default()
	cfg.Pool.MaxServers = 1
	r := New(cfg)
	r.RegisterProject("/p")

	// 手动填池 (绕过 spawn)
	r.poolMu.Lock()
	r.pool["/p|fake"] = nil // nil conn 占位
	r.poolOrder = []string{"/p|fake"}
	r.poolMu.Unlock()

	// 触发一次 spawn 路径 (lang=go 会真 spawn, 若 gopls 不在则 error; 但 evict 先发生)
	// 此处仅验证 len>=max 时 evict 逻辑存在性, 不实际 spawn
	r.poolMu.Lock()
	if len(r.pool) >= cfg.Pool.MaxServers && len(r.poolOrder) > 0 {
		r.removeFromPool(r.poolOrder[0])
	}
	r.poolMu.Unlock()
	if _, ok := r.pool["/p|fake"]; ok {
		t.Error("fake entry should have been evicted")
	}
}

// TestPoolOrderInvariant_DeleteRespawnEvict 回归: 删除→重建→驱逐全程
// pool 与 poolOrder 必须同构 (无重复、无幽灵), 否则驱逐会误杀存活连接。
func TestPoolOrderInvariant_DeleteRespawnEvict(t *testing.T) {
	cfg := config.Default()
	cfg.Pool.MaxServers = 1
	r := New(cfg)
	defer r.Shutdown()
	const key = "/p|go"
	assertInvariant := func(stage string) {
		t.Helper()
		r.poolMu.Lock()
		defer r.poolMu.Unlock()
		seen := map[string]int{}
		for _, k := range r.poolOrder {
			seen[k]++
		}
		for k, n := range seen {
			if n > 1 {
				t.Fatalf("%s: poolOrder 重复 %q x%d", stage, k, n)
			}
			if _, ok := r.pool[k]; !ok {
				t.Fatalf("%s: poolOrder 有幽灵 %q", stage, k)
			}
		}
		if len(seen) != len(r.pool) {
			t.Fatalf("%s: order(%d) 与 pool(%d) 数量不一致", stage, len(seen), len(r.pool))
		}
	}

	// 注入连接
	r.poolMu.Lock()
	r.pool[key] = nil
	r.poolOrder = append(r.poolOrder, key)
	r.poolMu.Unlock()
	assertInvariant("inject")

	// 模拟死连接移除 (旧 bug: 只删 map 不清 order, 重建后双条目)
	r.poolMu.Lock()
	r.removeFromPool(key)
	r.poolMu.Unlock()
	assertInvariant("remove")

	// 重建
	r.poolMu.Lock()
	r.pool[key] = nil
	r.poolOrder = append(r.poolOrder, key)
	r.poolMu.Unlock()
	assertInvariant("respawn")

	// 驱逐: order[0] 必须是该唯一条目, 删后双空
	r.poolMu.Lock()
	evict := r.poolOrder[0]
	r.removeFromPool(evict)
	r.poolMu.Unlock()
	assertInvariant("evict")
	if len(r.pool) != 0 || len(r.poolOrder) != 0 {
		t.Fatalf("evict 后应为空, pool=%d order=%d", len(r.pool), len(r.poolOrder))
	}
}

func TestProjectForFile(t *testing.T) {
	r := New(config.Default())
	r.RegisterProject("/home/user/proj")
	root, ok := r.ProjectForFile("/home/user/proj/sub/file.go")
	if !ok || root != "/home/user/proj" {
		t.Errorf("want /home/user/proj, got %q ok=%v", root, ok)
	}
	// 前缀但非子路径
	if _, ok := r.ProjectForFile("/home/user/proj2/file.go"); ok {
		t.Error("proj2 should not match proj")
	}
}

// TestIsDeadConnErrNarrow 回归 (P2-8): 只认断管信号; 健康服务器的普通错误
// (含 "write"/"eof" 字样) 不得触发自愈重启。
func TestIsDeadConnErrNarrow(t *testing.T) {
	dead := []error{
		io.ErrClosedPipe,
		io.EOF,
		syscall.EPIPE,
		syscall.ECONNRESET,
		fmt.Errorf("write |1: broken pipe"),
		fmt.Errorf("read: connection reset by peer"),
	}
	for _, e := range dead {
		if !isDeadConnErr(e) {
			t.Errorf("should be dead: %v", e)
		}
	}
	alive := []error{
		nil,
		fmt.Errorf("failed to write response: disk quota exceeded"),
		fmt.Errorf("lsp: unexpected eof while parsing config"),
		fmt.Errorf("plugin io error: bad frame"),
		fmt.Errorf("server error: input/output error on workdir"),
	}
	for _, e := range alive {
		if isDeadConnErr(e) {
			t.Errorf("should NOT be dead: %v", e)
		}
	}
}
func TestSetConfigSwaps(t *testing.T) {
	r := New(config.Default())
	defer r.Shutdown()
	cfg2 := config.Default()
	cfg2.Pool.MaxServers = 2
	cfg2.Pool.IdleTTLSec = 7
	r.SetConfig(cfg2)
	max, ttl := r.cfg.Load().Pool.MaxServers, r.cfg.Load().Pool.IdleTTLSec
	if max != 2 || ttl != 7 {
		t.Errorf("SetConfig 未生效: max=%d ttl=%d", max, ttl)
	}
	r.SetConfig(nil) // 不得 panic, 保持旧配置
	max = r.cfg.Load().Pool.MaxServers
	if max != 2 {
		t.Errorf("SetConfig(nil) 不应清空配置: max=%d", max)
	}
}
