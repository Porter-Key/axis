// Package codegraph 封装 codegraph CLI 调用 (降级为 CLI, 即用即走, 不管理 daemon)。
// 提供粗探索工具后端: query/callers/callees/impact/context/explore。
// 索引新鲜度: 调用前若 fsmonitor 记录到变更则先 codegraph sync 再查询。
package codegraph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Porter-Key/axis/internal/config"
)

// Client codegraph CLI 客户端。
type Client struct {
	bin       string
	autoInit  bool
	syncOnChg bool
	timeout   time.Duration
}

// New 创建 client。
func New(cfg config.CodegraphConfig) *Client {
	c := &Client{bin: cfg.Bin, autoInit: cfg.AutoInit, syncOnChg: cfg.SyncOnChange}
	c.timeout = time.Duration(cfg.TimeoutSec) * time.Second
	if c.timeout <= 0 {
		c.timeout = 120 * time.Second
	}
	return c
}

// HasIndex 检查项目是否有 .codegraph 索引。
func (c *Client) HasIndex(projectRoot string) bool {
	st, err := os.Stat(projectRoot + "/.codegraph")
	return err == nil && st.IsDir()
}

// EnsureIndex 索引缺失时按配置 autoInit 或返回友好错误。
func (c *Client) EnsureIndex(projectRoot string) error {
	if c.HasIndex(projectRoot) {
		return nil
	}
	if !c.autoInit {
		return fmt.Errorf("项目 %s 无 codegraph 索引. 请先运行: codegraph init %s (或配置 codegraph.auto_init=true)", projectRoot, projectRoot)
	}
	_, err := c.run(context.Background(), projectRoot, nil, "init", projectRoot)
	return err
}

func ctxTODO() context.Context { return context.Background() }

// Query 符号搜索 (JSON 输出)。kind 可为 ""。
func (c *Client) Query(ctx context.Context, projectRoot, query, kind string, limit int) (json.RawMessage, error) {
	args := []string{"query", "-p", projectRoot, "-j"}
	if kind != "" {
		args = append(args, "-k", kind)
	}
	if limit > 0 {
		args = append(args, "-l", fmt.Sprint(limit))
	}
	args = append(args, query)
	return c.runJSON(ctx, projectRoot, args...)
}

// Callers / Callees / Impact 均 JSON。
func (c *Client) Callers(ctx context.Context, projectRoot, symbol string) (json.RawMessage, error) {
	return c.runJSON(ctx, projectRoot, "callers", "-p", projectRoot, "-j", symbol)
}
func (c *Client) Callees(ctx context.Context, projectRoot, symbol string) (json.RawMessage, error) {
	return c.runJSON(ctx, projectRoot, "callees", "-p", projectRoot, "-j", symbol)
}
func (c *Client) Impact(ctx context.Context, projectRoot, symbol string) (json.RawMessage, error) {
	return c.runJSON(ctx, projectRoot, "impact", "-p", projectRoot, "-j", symbol)
}

// Context 构建任务上下文 (markdown 或 json)。task 可能多词。
func (c *Client) Context(ctx context.Context, projectRoot, format string, task []string) (json.RawMessage, error) {
	args := []string{"context", "-p", projectRoot, "-f", format}
	args = append(args, task...)
	return c.runJSON(ctx, projectRoot, args...)
}

// Explore 粗探索 (文本输出, 直接透传)。
func (c *Client) Explore(ctx context.Context, projectRoot string, query []string) (string, error) {
	if err := c.syncGate(ctx, projectRoot); err != nil {
		return "", err
	}
	args := []string{"explore", "-p", projectRoot}
	args = append(args, query...)
	return c.runText(ctx, projectRoot, args...)
}

// Node 单符号源码 (文本)。
func (c *Client) Node(ctx context.Context, projectRoot, name string) (string, error) {
	if err := c.syncGate(ctx, projectRoot); err != nil {
		return "", err
	}
	return c.runText(ctx, projectRoot, "node", "-p", projectRoot, name)
}

// Status 索引状态 (JSON)。status 接受位置 path。
func (c *Client) Status(ctx context.Context, projectRoot string) (json.RawMessage, error) {
	return c.runJSON(ctx, projectRoot, "status", "-j", projectRoot)
}

// RunText 执行任意 codegraph 子命令 (如 files) 返回文本。
func (c *Client) RunText(ctx context.Context, projectRoot, cmd string) (string, error) {
	if err := c.syncGate(ctx, projectRoot); err != nil {
		return "", err
	}
	return c.runText(ctx, projectRoot, cmd, "-p", projectRoot)
}

// Sync 主动同步 (变更后调用)。sync 接受位置 path。
func (c *Client) Sync(ctx context.Context, projectRoot string) error {
	_, err := c.run(ctx, projectRoot, nil, "sync", projectRoot)
	return err
}

// LastChangeIsNewer 由外部 (fsmonitor) 提供是否需 sync; 内部记录上次 sync 时间。
func (c *Client) LastSyncAt(projectRoot string) time.Time {
	if t, ok := lastSync[projectRoot]; ok {
		return t
	}
	return time.Time{}
}
func (c *Client) MarkSynced(projectRoot string) {
	lastSync[projectRoot] = time.Now()
}

var lastSync = map[string]time.Time{}

// ConfigForTest 供测试构建 Client。
func ConfigForTest() config.CodegraphConfig {
	return config.CodegraphConfig{Bin: "codegraph", AutoInit: false, SyncOnChange: false, TimeoutSec: 60}
}

// syncGate: 若项目刚 sync 过 (本进程内) 则跳过; 否则每次查询前 sync (幂等, 增量快)。
func (c *Client) syncGate(ctx context.Context, projectRoot string) error {
	if !c.syncOnChg {
		return nil
	}
	if !c.HasIndex(projectRoot) {
		return c.EnsureIndex(projectRoot)
	}
	// codegraph CLI 的 sync 是增量的 (自身 watcher 也自动), 这里保守每次查询前跑一次
	// 代价: 增量 sync 毫秒级 (无变更时 fast path)。
	if err := c.Sync(ctx, projectRoot); err != nil {
		return fmt.Errorf("codegraph sync: %w", err)
	}
	c.MarkSynced(projectRoot)
	return nil
}

// runJSON 执行并解析 JSON 输出。
func (c *Client) runJSON(ctx context.Context, projectRoot string, args ...string) (json.RawMessage, error) {
	if err := c.syncGate(ctx, projectRoot); err != nil {
		return nil, err
	}
	out, err := c.run(ctx, projectRoot, nil, args...)
	if err != nil {
		return nil, err
	}
	// 容忍前置警告行: 找第一个 '{' 开始
	idx := strings.IndexByte(out, '{')
	if idx < 0 {
		return nil, fmt.Errorf("codegraph 输出非 JSON: %s", truncate(out, 200))
	}
	return json.RawMessage(out[idx:]), nil
}

func (c *Client) runText(ctx context.Context, projectRoot string, args ...string) (string, error) {
	return c.run(ctx, projectRoot, nil, args...)
}

func (c *Client) run(ctx context.Context, projectRoot string, _ map[string]string, args ...string) (string, error) {
	ctx2, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx2, c.bin, args...)
	cmd.Dir = projectRoot
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("codegraph %s: %w\n%s", args[0], err, truncate(buf.String(), 500))
	}
	return buf.String(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
