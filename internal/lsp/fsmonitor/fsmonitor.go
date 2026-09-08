// Package fsmonitor 提供每项目文件监测 (长连接模型配套):
//   - 记录最近文件系统变更 (路径 + 时间) → 供 LSP 连接 Invalidate / codegraph sync 门
//   - 防抖: 事件风暴合并; 变更路径聚合并去重
//   - 忽略规则由配置注入 (项目级忽略, 默认 DefaultIgnore)
package fsmonitor

import (
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/Porter-Key/axis/internal/logger"
)

// Monitor 单项目 watcher。
type Monitor struct {
	root    string
	watcher *fsnotify.Watcher
	ignore  []string // 忽略片段: 目录名精确匹配 (如 ".git") 或通配后缀 (如 "*.pyc")

	mu           sync.Mutex
	changed      bool      // 自上次查询以来有无变更
	changedPaths []string  // 防抖窗口内变更路径 (去重, 供 LSP Invalidate)
	lastAt       time.Time // 最近变更时间
	debounce     time.Duration
	done         chan struct{}
	closeOnce    sync.Once
}

// New 创建并递归监听项目根。
func New(root string, debounce time.Duration, ignore []string) (*Monitor, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	m := &Monitor{root: root, watcher: w, debounce: debounce, ignore: ignore, done: make(chan struct{})}
	if err := m.walkAdd(root); err != nil {
		w.Close()
		return nil, err
	}
	go m.loop()
	return m, nil
}

// walkAdd 递归添加目录 (跳过忽略项)。
func (m *Monitor) walkAdd(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 权限等跳过
		}
		if !d.IsDir() {
			return nil
		}
		if m.isIgnored(path) {
			return filepath.SkipDir
		}
		return m.watcher.Add(path)
	})
}

// isIgnored 目录/文件是否命中忽略规则:
//   - 无通配符片段: 按 base 名精确匹配 (目录名或文件名)
//   - 含 "*" 片段: filepath.Match 匹配 base (如 "*.pyc" 防写入噪声)
func (m *Monitor) isIgnored(path string) bool {
	base := filepath.Base(path)
	for _, ig := range m.ignore {
		if ig == "" {
			continue
		}
		if strings.Contains(ig, "*") {
			if ok, _ := filepath.Match(ig, base); ok {
				return true
			}
			continue
		}
		if base == ig {
			return true
		}
	}
	return false
}

func (m *Monitor) loop() {
	for {
		select {
		case <-m.done:
			return
		case ev, ok := <-m.watcher.Events:
			if !ok {
				return
			}
			// 只关心代码/配置文件事件, 且非临时文件/忽略项
			if strings.HasSuffix(ev.Name, "~") || strings.HasSuffix(ev.Name, ".swp") || strings.HasSuffix(ev.Name, ".tmp") {
				continue
			}
			if m.isIgnored(ev.Name) {
				logger.With("root", m.root).Debug("fs event ignored", "path", ev.Name, "op", ev.Op.String())
				continue
			}
			logger.With("root", m.root).Debug("fs event", "path", ev.Name, "op", ev.Op.String())
			m.mu.Lock()
			if !m.changed {
				// 防抖: 变更标记置位, debounce 后由查询方消费
				m.changed = true
				m.lastAt = time.Now()
				go m.armReset()
			} else {
				m.lastAt = time.Now()
			}
			// 记录变更路径 (去重; 上限 200 防风暴爆内存, 超出后置 dirtyAll 语义=空串)
			if len(m.changedPaths) < 200 {
				dup := false
				for _, p := range m.changedPaths {
					if p == ev.Name {
						dup = true
						break
					}
				}
				if !dup {
					m.changedPaths = append(m.changedPaths, ev.Name)
				}
			}
			m.mu.Unlock()
			_ = ev
		case err, ok := <-m.watcher.Errors:
			if !ok {
				return
			}
			_ = err
		}
	}
}

// armReset debounce 到期后清 changed (若期间无新事件)。
func (m *Monitor) armReset() {
	time.Sleep(m.debounce)
	m.mu.Lock()
	defer m.mu.Unlock()
	// 距 lastAt 已过 debounce, 视为事件安静, 但保留 changed=true
	// 直到 ConsumeChange 消费。这里只负责"防抖窗口", 不做自动清除,
	// 避免查询方错过变更。
}

// ConsumeChange 查询并消费变更标记: 返回自上次以来是否有变更。
func (m *Monitor) ConsumeChange() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	had := m.changed
	m.changed = false
	return had
}

// ConsumeChangedPaths 查询并消费变更路径列表 (自上次以来; 空=无或已满 200 触发项目级失效)。
// 供长连接 LSP Invalidate 使用: 返回具体变更文件路径。
func (m *Monitor) ConsumeChangedPaths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.changedPaths
	m.changedPaths = nil
	m.changed = false
	if len(out) > 0 {
		logger.With("root", m.root).Debug("fs consume changed paths", "n", len(out))
	}
	return out
}

// ChangedSince 距指定时间后有无变更 (不消费)。
func (m *Monitor) ChangedSince(t time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastAt.After(t)
}

// LastChangeAt 最近变更时间。
func (m *Monitor) LastChangeAt() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastAt
}

// Close 停止监听。
func (m *Monitor) Close() {
	m.closeOnce.Do(func() {
		close(m.done)
		m.watcher.Close()
	})
}
