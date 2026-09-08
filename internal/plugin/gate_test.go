package plugin

import (
	"testing"
	"time"
)

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
