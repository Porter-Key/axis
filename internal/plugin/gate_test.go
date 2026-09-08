package plugin

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain 门禁落盘隔离: 包内测试一律写临时 state, 不碰真实 ~/.local/state。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "axis-gate-test")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("XDG_STATE_HOME", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// TestGateExpiry 过期绑定必须被拒绝 (防 sessionID 长期累积)。
func TestGateExpiry(t *testing.T) {
	g := NewGate()
	g.SetTTL(50 * time.Millisecond)
	g.Activate("s1", "/tmp/proj")
	if p, ok := g.ActiveProject("s1"); !ok || p != "/tmp/proj" {
		t.Fatalf("activate 后应命中: %q %v", p, ok)
	}
	time.Sleep(150 * time.Millisecond)
	if p, ok := g.ActiveProject("s1"); ok {
		t.Fatalf("过期绑定应被拒绝, 实际命中 %q", p)
	}
}

// TestGateSlidingRefresh 活跃会话每次调用刷新窗口, 永不过期
// (目录级工具每次都经 ProjectFor, 不会误杀长连会话)。
func TestGateSlidingRefresh(t *testing.T) {
	g := NewGate()
	g.SetTTL(200 * time.Millisecond)
	g.Activate("s1", "/tmp/proj")
	for i := 0; i < 5; i++ {
		time.Sleep(80 * time.Millisecond)
		if _, ok := g.ActiveProject("s1"); !ok {
			t.Fatalf("第 %d 次调用过期了活跃会话", i)
		}
	}
}

// TestGateNoExpiryWhenTTLNonPositive TTL<=0 = 不过期 (仅测试语义)。
func TestGateNoExpiryWhenTTLNonPositive(t *testing.T) {
	g := NewGate()
	g.SetTTL(0)
	g.Activate("s1", "/tmp/proj")
	time.Sleep(20 * time.Millisecond)
	if _, ok := g.ActiveProject("s1"); !ok {
		t.Fatal("TTL<=0 时绑定不应过期")
	}
}

// TestGatePersistReload 落盘恢复: Activate 落盘 → 新 Gate 读回 (重启不丢“上次去哪”)。
func TestGatePersistReload(t *testing.T) {
	g := NewGate()
	g.Activate("s1", "/tmp/projA")
	g.Activate("s2", "/tmp/projB")
	g2 := NewGate()
	// 先断言提示 (ActiveProject 会刷新滑动窗口, 必须先查提示再碰条目)
	if h := g2.RecoveryHint(); h == "" {
		t.Fatal("恢复提示为空")
	} else if !strings.Contains(h, "/tmp/projB") {
		t.Fatalf("提示应指向上次活跃项目: %q", h)
	}
	if p, ok := g2.ActiveProject("s1"); !ok || p != "/tmp/projA" {
		t.Fatalf("s1 未恢复: %q %v", p, ok)
	}
	g2.Deactivate("s1")
	g2.Deactivate("s2")
	g3 := NewGate()
	if h := g3.RecoveryHint(); h != "" {
		t.Fatalf("全解绑后提示应空: %q", h)
	}
}
