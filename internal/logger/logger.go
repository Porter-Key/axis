// Package logger 提供进程统一结构化日志 (slog JSON 落 JSONL 文件)。
// 用法: logger.L().Info("msg", "key", val) 或 logger.With("conn", key).Debug(...)。
// 排查问题直接看 JSONL, 不再猜。
package logger

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

var (
	mu     sync.RWMutex
	global *slog.Logger
)

// Config 日志配置。
type Config struct {
	Path  string `json:"path,omitempty" yaml:"path,omitempty"`    // JSONL 文件路径; 空 = stderr
	Level string `json:"level,omitempty" yaml:"level,omitempty"`  // debug|info|warn|error (默认 info)
	Also  bool   `json:"also,omitempty" yaml:"also,omitempty"`    // true = 同时输出 stderr
}

// DefaultPath 默认 JSONL 路径 (用户级, systemd User=porter 可写)。
func DefaultPath() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "axis", "server.jsonl")
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		return filepath.Join(home, ".local", "state", "axis", "server.jsonl")
	}
	return "/tmp/axis-server.jsonl"
}

// Init 初始化全局 logger。可多次调用 (热重载); 关闭旧文件句柄。
func Init(cfg Config) error {
	var w io.Writer = os.Stderr
	if cfg.Path != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(cfg.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		w = f
		if cfg.Also {
			w = io.MultiWriter(f, os.Stderr)
		}
	}
	lvl := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	mu.Lock()
	global = slog.New(h)
	mu.Unlock()
	return nil
}

// L 全局 logger。
func L() *slog.Logger {
	mu.RLock()
	defer mu.RUnlock()
	if global == nil {
		return slog.Default()
	}
	return global
}

// With 带字段的派生 logger。
func With(args ...any) *slog.Logger { return L().With(args...) }
