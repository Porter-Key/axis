// Package registry 管理项目/会话/LSP 连接池的生命周期:
//   - Project: 注册项目 (根+语言), 维护最近活跃度
//   - Session: opencode 等客户端会话 (token -> 项目), 心跳超时自动注销
//   - LSP Pool: 懒加载 + 空闲 TTL + LRU 上限 + 内存看门狗
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Porter-Key/axis/internal/config"
	"github.com/Porter-Key/axis/internal/logger"
	"github.com/Porter-Key/axis/internal/lsp/client"
)

// Project 一个注册的代码项目。
type Project struct {
	Root     string // 项目根 (marker 所在目录)
	Langs    map[string]bool
	lastFs   time.Time // 最近文件系统活动 (fsmonitor 喂)
	lastCall time.Time // 最近工具调用
	mu       sync.Mutex
}

// Session 一个连接的客户端会话。
type Session struct {
	Token     string
	Project   string // 关联项目根
	LastSeen  time.Time
	UserAgent string
}

// Registry 总控。
type Registry struct {
	cfg *config.Config

	mu       sync.Mutex
	projects map[string]*Project // root -> project
	sessions map[string]*Session // token -> session

	poolMu    sync.Mutex
	pool      map[string]*lspclient.Conn // key: root|lang
	poolOrder []string                   // LRU 顺序 (尾部最新)
	poolCtx   context.Context

	// 崩溃自愈: key(root|lang) -> 退避截止时间; 崩溃重启失败后指数退避重试。
	backoffMu     sync.Mutex
	crashBackoff  map[string]time.Time
	crashAttempts map[string]int // key -> 连续失败次数 (计算退避)
}

// New 创建 registry 并启动回收 goroutine。
func New(cfg *config.Config) *Registry {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Registry{
		cfg:           cfg,
		projects:      map[string]*Project{},
		sessions:      map[string]*Session{},
		pool:          map[string]*lspclient.Conn{},
		poolCtx:       ctx,
		crashBackoff:  map[string]time.Time{},
		crashAttempts: map[string]int{},
	}
	_ = cancel
	go r.reapLoop(ctx)
	return r
}

// nextBackoff 指数退避: 5s → 10s → 20s → 40s → cap 60s (按连续失败次数)。
func (r *Registry) nextBackoff(key string) time.Duration {
	r.backoffMu.Lock()
	defer r.backoffMu.Unlock()
	n := r.crashAttempts[key]
	if n < 4 {
		r.crashAttempts[key] = n + 1
	}
	d := 5 * time.Second
	for i := 0; i < n && i < 4; i++ {
		d *= 2
	}
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

// clearBackoff 崩溃重启成功后清退避状态。
func (r *Registry) clearBackoff(key string) {
	r.backoffMu.Lock()
	defer r.backoffMu.Unlock()
	delete(r.crashBackoff, key)
	delete(r.crashAttempts, key)
}

// Shutdown 停止所有回收并关闭全部 LSP。
func (r *Registry) Shutdown() {
	r.poolMu.Lock()
	defer r.poolMu.Unlock()
	for k, c := range r.pool {
		_ = c.Close()
		delete(r.pool, k)
	}
}

// RegisterProject 注册/更新项目根。
func (r *Registry) RegisterProject(root string) *Project {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.projects[root]; ok {
		return p
	}
	p := &Project{Root: root, Langs: map[string]bool{}, lastCall: time.Now()}
	r.projects[root] = p
	return p
}

// TouchFs 记录项目文件系统活动 (由 fsmonitor 调用)。
func (r *Registry) TouchFs(root string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.projects[root]; ok {
		p.mu.Lock()
		p.lastFs = time.Now()
		p.mu.Unlock()
	}
}

// RegisterSession 注册会话。
func (r *Registry) RegisterSession(token, project, ua string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.projects[project]; !ok {
		return fmt.Errorf("project %s 未注册 (先 RegisterProject)", project)
	}
	r.sessions[token] = &Session{Token: token, Project: project, LastSeen: time.Now(), UserAgent: ua}
	return nil
}

// Heartbeat 会话心跳。
func (r *Registry) Heartbeat(token string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[token]
	if !ok {
		return false
	}
	s.LastSeen = time.Now()
	return true
}

// UnregisterSession 注销会话 (opencode 退出时调用)。
func (r *Registry) UnregisterSession(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, token)
}

// TouchCall 记录一次工具调用 (活跃度)。
func (r *Registry) TouchCall(projectRoot string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.projects[projectRoot]; ok {
		p.mu.Lock()
		p.lastCall = time.Now()
		p.mu.Unlock()
	}
}

// LSPConn 获取 (root, lang) 的 LSP 连接 (长连接池, 懒加载 + 崩溃自愈)。
// lazySpawn=true 时若未运行则启动 (首查); 否则只返回已有 (预热查询用)。
func (r *Registry) LSPConn(ctx context.Context, projectRoot, lang string, lazySpawn bool) (*lspclient.Conn, error) {
	key := projectRoot + "|" + lang
	r.poolMu.Lock()
	if c, ok := r.pool[key]; ok {
		// 崩溃检测: 进程死了 → 移除, 走下方重启
		if !c.IsAlive() {
			_ = c.Close()
			delete(r.pool, key)
		} else {
			// 退避期内? (reap 刚失败过) → 仍可用旧连接? 不, 已删; 直接正常返回
			// 移到 LRU 尾部
			r.touchLRU(key)
			r.poolMu.Unlock()
			return c, nil
		}
	}
	if !lazySpawn {
		r.poolMu.Unlock()
		return nil, fmt.Errorf("LSP for %s 未运行", lang)
	}
	// 退避期内: 不立即重启, 返回明确错误 (崩溃自愈退避中)
	r.backoffMu.Lock()
	until, backing := r.crashBackoff[key]
	r.backoffMu.Unlock()
	if backing && time.Now().Before(until) {
		r.poolMu.Unlock()
		return nil, fmt.Errorf("LSP for %s 崩溃自愈退避中 (%.0fs 后重试)", lang, time.Until(until).Seconds())
	}
	// 检查 LRU 上限
	if len(r.pool) >= r.cfg.Pool.MaxServers {
		// 逐出最久未用 (头部)
		evict := r.poolOrder[0]
		if old, ok := r.pool[evict]; ok {
			go old.Close()
			delete(r.pool, evict)
		}
		r.poolOrder = r.poolOrder[1:]
	}
	ad, ok := r.cfg.Adapters[lang]
	if !ok {
		r.poolMu.Unlock()
		return nil, fmt.Errorf("无语言适配器: %s", lang)
	}
	r.poolMu.Unlock()

	timeout := time.Duration(ad.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	conn, err := lspclient.Spawn(ctx, ad, projectRoot, timeout)
	if err != nil {
		return nil, err
	}
	r.poolMu.Lock()
	// 双检: 可能并发已建
	if c, ok := r.pool[key]; ok {
		r.poolMu.Unlock()
		_ = conn.Close()
		r.touchLRU(key)
		return c, nil
	}
	r.pool[key] = conn
	r.poolOrder = append(r.poolOrder, key)
	r.poolMu.Unlock()
	r.clearBackoff(key) // 重启成功 → 清退避
	r.TouchCall(projectRoot)
	return conn, nil
}

func (r *Registry) touchLRU(key string) {
	for i, k := range r.poolOrder {
		if k == key {
			r.poolOrder = append(r.poolOrder[:i], r.poolOrder[i+1:]...)
			break
		}
	}
	r.poolOrder = append(r.poolOrder, key)
}

// CallWithDoc 崩溃自愈版文件请求: LSPConn → 调用; 失败且疑似连接死亡 (写/读错误)
// → 移除死连接 → 重启 → 重试一次。工具层对崩溃无感 (除极端二次失败)。
func (r *Registry) CallWithDoc(ctx context.Context, projectRoot, lang, method, filePath string, extra map[string]any, timeout time.Duration) (json.RawMessage, error) {
	key := projectRoot + "|" + lang
	conn, err := r.LSPConn(ctx, projectRoot, lang, true)
	if err != nil {
		return nil, err
	}
	res, err := conn.CallWithDoc(ctx, method, filePath, extra, timeout)
	if err == nil {
		r.clearBackoff(key)
		return res, nil
	}
	// 失败: 连接疑似死亡 (进程没了 / 写管道失败 / 读 EOF) → 自愈重启重试一次。
	// 僵尸进程可能让 Signal(0) 仍成功, 故用错误特征辅助判断。
	if !conn.IsAlive() || isDeadConnErr(err) {
		r.poolMu.Lock()
		if c, ok := r.pool[key]; ok && c == conn {
			delete(r.pool, key)
			go conn.Close() // 回收僵尸
		}
		r.poolMu.Unlock()
		// 退避检查
		r.backoffMu.Lock()
		_, backing := r.crashBackoff[key]
		r.backoffMu.Unlock()
		if backing {
			return nil, fmt.Errorf("LSP for %s 崩溃重启退避中: %v", lang, err)
		}
		newConn, err2 := r.LSPConn(ctx, projectRoot, lang, true)
		if err2 != nil {
			r.backoffMu.Lock()
			r.crashBackoff[key] = time.Now().Add(r.nextBackoff(key))
			r.backoffMu.Unlock()
			return nil, fmt.Errorf("LSP %s 崩溃自愈重启失败: %v (原错误: %v)", lang, err2, err)
		}
		res2, err3 := newConn.CallWithDoc(ctx, method, filePath, extra, timeout)
		if err3 != nil {
			return nil, fmt.Errorf("LSP %s 崩溃重启后重试仍失败: %v", lang, err3)
		}
		r.clearBackoff(key)
		return res2, nil
	}
	return nil, err
}

// isDeadConnErr 连接死亡类错误特征 (写入已死进程/读 EOF 等)。
func isDeadConnErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, frag := range []string{"broken pipe", "epipe", "connection reset", "eof", "closed pipe", "write", "input/output error", "io error"} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}

// InvalidateProject 项目文件变更 → 该 root 下所有常驻 LSP 连接:
//  1. 主动推送 workspace/didChangeWatchedFiles (服务器即时重载项目模型);
//  2. 本地脏标记 (下次文件请求 didChange 全量同步)。
//
// changedPaths 为空 = 项目级失效 (dirtyAll); 非空 = 精确文件失效。
// 由 fsmonitor 消费循环调用 (长连接重新索引/同步的入口)。
func (r *Registry) InvalidateProject(root string, changedPaths []string) {
	r.poolMu.Lock()
	defer r.poolMu.Unlock()
	prefix := root + "|"
	n := 0
	for k, c := range r.pool {
		if strings.HasPrefix(k, prefix) {
			n++
			if len(changedPaths) == 0 {
				// 项目级失效: 无法精确同步 → dirtyAll (下次任一文件请求 didChange 全量重读)
				c.Invalidate("")
				continue
			}
			// 精确变更: NotifyProjectChanged 内部对已打开文档发 didChange 全量,
			// 未打开文档发 didChangeWatchedFiles (服务器自行读盘)。内容已同步,
			// 无需再标脏。
			c.NotifyProjectChanged(changedPaths)
		}
	}
	logger.With("root", root, "conns", n).Info("invalidate project", "paths", len(changedPaths), "dirtyAll", len(changedPaths) == 0)
}

// Status 汇总状态 (供 /ctrl/status)。
func (r *Registry) Status() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]any{}
	projs := []map[string]any{}
	for root, p := range r.projects {
		p.mu.Lock()
		projs = append(projs, map[string]any{
			"root": root, "langs": keys(p.Langs),
			"last_fs":   p.lastFs.Format(time.RFC3339),
			"last_call": p.lastCall.Format(time.RFC3339),
		})
		p.mu.Unlock()
	}
	sort.Slice(projs, func(i, j int) bool { return projs[i]["root"].(string) < projs[j]["root"].(string) })
	out["projects"] = projs
	sess := []map[string]any{}
	for _, s := range r.sessions {
		sess = append(sess, map[string]any{
			"token": s.Token, "project": s.Project,
			"last_seen": s.LastSeen.Format(time.RFC3339),
		})
	}
	out["sessions"] = sess
	r.poolMu.Lock()
	defer r.poolMu.Unlock()
	pool := []map[string]any{}
	for k, c := range r.pool {
		pool = append(pool, map[string]any{
			"key": k, "lang": c.Lang(), "last_use": c.LastUse().Format(time.RFC3339),
			"rss_mb": procRSS(c) / (1024 * 1024),
		})
	}
	out["lsp_pool"] = pool
	return out
}

// RegisterProjectLangs 项目已知语言加入。
func (r *Registry) RegisterProjectLangs(root string, langs ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.projects[root]
	if !ok {
		return
	}
	for _, l := range langs {
		p.Langs[l] = true
	}
}

func (r *Registry) reapLoop(ctx context.Context) {
	hbTimeout := time.Duration(r.cfg.Heartbeat.TimeoutSec) * time.Second
	idleTTL := time.Duration(r.cfg.Pool.IdleTTLSec) * time.Second
	memLimit := int64(r.cfg.Pool.MemoryLimitMB) * 1024 * 1024

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			// 1. 会话心跳超时 → 注销
			r.mu.Lock()
			for tok, s := range r.sessions {
				if now.Sub(s.LastSeen) > hbTimeout {
					delete(r.sessions, tok)
				}
			}
			r.mu.Unlock()

			// 2. LSP 池: 崩溃检测 (IsAlive) + 空闲 TTL + 内存看门狗
			r.poolMu.Lock()
			var toClose []string
			var toRestart []string
			for k, c := range r.pool {
				if !c.IsAlive() {
					// 崩溃: 直接重启 (指数退避在 RestartConn 内管)
					toRestart = append(toRestart, k)
					continue
				}
				idle := now.Sub(c.LastUse())
				rss := procRSS(c)
				if idle > idleTTL || (memLimit > 0 && rss > memLimit) {
					toClose = append(toClose, k)
				}
			}
			// 先关空闲/超限
			for _, k := range toClose {
				if c, ok := r.pool[k]; ok {
					go c.Close()
					delete(r.pool, k)
				}
			}
			// 再重启崩溃的 (异步, 不阻塞 reap 循环)
			for _, k := range toRestart {
				if c, ok := r.pool[k]; ok {
					_ = c.Close() // 确保僵尸进程清掉
					delete(r.pool, k)
					parts := strings.SplitN(k, "|", 2)
					if len(parts) == 2 {
						key := k
						root, lang := parts[0], parts[1]
						go func() {
							ctx2, cancel := context.WithTimeout(context.Background(), 60*time.Second)
							defer cancel()
							if _, err := r.LSPConn(ctx2, root, lang, true); err != nil {
								// 重启失败: 记录并退避 (下次 reap 再试)
								r.backoffMu.Lock()
								r.crashBackoff[key] = time.Now().Add(r.nextBackoff(key))
								r.backoffMu.Unlock()
							}
						}()
					}
				}
			}
			// 同步 LRU 顺序
			if len(toClose) > 0 || len(toRestart) > 0 {
				newOrder := r.poolOrder[:0]
				for _, k := range r.poolOrder {
					if _, ok := r.pool[k]; ok {
						newOrder = append(newOrder, k)
					}
				}
				r.poolOrder = newOrder
			}
			r.poolMu.Unlock()
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// procRSS 读取子进程 RSS (bytes), 失败返回 0。
func procRSS(c *lspclient.Conn) int64 {
	pid := c.ProcessPID()
	if pid <= 0 {
		return 0
	}
	data, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "status"))
	if err != nil {
		return 0
	}
	var rss int64
	_, _ = fmt.Sscanf(string(data), "%*s") // noop 占位
	// 简单解析 VmRSS
	for _, line := range splitLines(string(data)) {
		var kb int64
		var name string
		if n, _ := fmt.Sscanf(line, "%s %d", &name, &kb); n == 2 && name == "VmRSS:" {
			rss = kb * 1024
			break
		}
	}
	return rss
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// ProjectForFile 通过文件路径找已注册项目 (最长前缀匹配)。
func (r *Registry) ProjectForFile(filePath string) (string, bool) {
	abs, _ := filepath.Abs(filePath)
	dir := filepath.Dir(abs)
	r.mu.Lock()
	defer r.mu.Unlock()
	best := ""
	for root := range r.projects {
		if (dir == root || isSub(dir, root)) && len(root) > len(best) {
			best = root
		}
	}
	return best, best != ""
}

func isSub(dir, root string) bool {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return false
	}
	return rel != ".." && !pathIsUp(rel)
}

func pathIsUp(rel string) bool {
	return len(rel) >= 2 && rel[:2] == ".."
}
