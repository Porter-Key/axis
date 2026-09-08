// Package config 定义 axis 的配置结构与加载逻辑。
// 配置真相在 YAML (~/.config/mcp/axis/config.yaml): 语言服务器 command/args/文件模式
// 全部可覆盖, 热重载后无需重启。内置默认仅作无配置文件时的 fallback。
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// LangAdapter 描述一个语言 -> LSP server 的适配器。
type LangAdapter struct {
	LanguageID   string   `json:"language_id" yaml:"language_id"`                       // 如 "go"
	Command      string   `json:"command" yaml:"command"`                               // LSP server 可执行 (推荐配置绝对路径)
	Args         []string `json:"args,omitempty" yaml:"args,omitempty"`                 // 启动参数
	FilePatterns []string `json:"file_patterns" yaml:"file_patterns"`                   // glob, 如 **/*.go
	Markers      []string `json:"markers,omitempty" yaml:"markers,omitempty"`           // 项目 marker 文件名
	TimeoutSec   int      `json:"timeout_sec,omitempty" yaml:"timeout_sec,omitempty"`   // 请求超时
	InitOptions  any      `json:"init_options,omitempty" yaml:"init_options,omitempty"` // initializationOptions
}

// Session 配置: 会话心跳超时 (秒)。
type HeartbeatConfig struct {
	TimeoutSec  int `json:"timeout_sec,omitempty" yaml:"timeout_sec,omitempty"`
	IntervalSec int `json:"interval_sec,omitempty" yaml:"interval_sec,omitempty"`
}

// LSPPool 配置: 生命周期控制。
type PoolConfig struct {
	IdleTTLSec    int `json:"idle_ttl_sec,omitempty" yaml:"idle_ttl_sec,omitempty"`
	MaxServers    int `json:"max_servers,omitempty" yaml:"max_servers,omitempty"`
	MemoryLimitMB int `json:"memory_limit_mb,omitempty" yaml:"memory_limit_mb,omitempty"`
}

// Config 顶层配置。
type Config struct {
	Adapters  map[string]LangAdapter `json:"adapters" yaml:"adapters"` // key = language_id
	Heartbeat HeartbeatConfig        `json:"heartbeat" yaml:"heartbeat"`
	Pool      PoolConfig             `json:"pool" yaml:"pool"`
	Codegraph CodegraphConfig        `json:"codegraph" yaml:"codegraph"`
	Memory    MemoryConfig           `json:"memory,omitempty" yaml:"memory,omitempty"` // memory 子模块 (sqlite=SSOT 知识库)
	Ignore    []string               `json:"ignore,omitempty" yaml:"ignore,omitempty"` // 项目级忽略 (目录/文件名片段, fsmonitor 与 codegraph 通用)
	Log       LogConfig              `json:"log,omitempty" yaml:"log,omitempty"`       // 结构化日志 (JSONL)
	CtrlToken string                 `json:"ctrl_token,omitempty" yaml:"ctrl_token,omitempty"`
}

// MemoryConfig memory 子模块配置 (sqlite=SSOT + md 导出映射视图)。
type MemoryConfig struct {
	DBPath    string `json:"db_path,omitempty" yaml:"db_path,omitempty"`       // sqlite 库路径 (默认 ~/.local/state/axis/memory.db)
	ExportDir string `json:"export_dir,omitempty" yaml:"export_dir,omitempty"` // md 映射视图导出目录 (默认 ~/.local/state/axis/memory/)
	Extension string `json:"extension,omitempty" yaml:"extension,omitempty"`   // 导出文件扩展名 (默认 md)
}

// LogConfig 日志配置。
type LogConfig struct {
	Path  string `json:"path,omitempty" yaml:"path,omitempty"`
	Level string `json:"level,omitempty" yaml:"level,omitempty"`
	Also  bool   `json:"also,omitempty" yaml:"also,omitempty"`
}

// DefaultIgnore 默认忽略片段 (目录名/文件后缀精确匹配, 防事件风暴与噪声索引)。
var DefaultIgnore = []string{
	".git", ".hg", ".svn", "node_modules", "target", ".venv", "venv", ".codegraph",
	"dist", "build", "vendor", ".cache", "__pycache__", ".pytest_cache", ".mypy_cache",
	"bin", "obj", ".vs", "packages", ".idea", ".vscode", "*.min.js", "*.min.css",
	"*.map", ".DS_Store", "*.pyc",
}

type CodegraphConfig struct {
	Bin          string `json:"bin,omitempty" yaml:"bin,omitempty"`
	AutoInit     bool   `json:"auto_init,omitempty" yaml:"auto_init,omitempty"`
	SyncOnChange bool   `json:"sync_on_change,omitempty" yaml:"sync_on_change,omitempty"`
	TimeoutSec   int    `json:"timeout_sec,omitempty" yaml:"timeout_sec,omitempty"`
}

// Default 内置六语言默认适配器 (仅 fallback, 命令用相对名; 真实路径由 YAML 配置覆盖)。
func Default() *Config {
	cfg := &Config{
		Adapters: map[string]LangAdapter{
			"go": {
				LanguageID: "go", Command: "gopls",
				FilePatterns: []string{"**/*.go"}, Markers: []string{"go.mod", "go.work"},
			},
			"rust": {
				LanguageID: "rust", Command: "rust-analyzer",
				FilePatterns: []string{"**/*.rs"}, Markers: []string{"Cargo.toml"},
			},
			"csharp": {
				LanguageID: "csharp", Command: "csharp-ls",
				FilePatterns: []string{"**/*.cs"}, Markers: []string{"*.csproj", "*.sln"},
			},
			"python": {
				LanguageID: "python", Command: "pyright-langserver",
				Args:         []string{"--stdio"},
				FilePatterns: []string{"**/*.py"}, Markers: []string{"pyproject.toml", "requirements.txt", "setup.py"},
			},
			"typescript": {
				LanguageID: "typescript", Command: "typescript-language-server",
				Args:         []string{"--stdio"},
				FilePatterns: []string{"**/*.ts", "**/*.tsx", "**/*.mts", "**/*.cts"}, Markers: []string{"tsconfig.json"},
			},
			"javascript": {
				LanguageID: "javascript", Command: "typescript-language-server",
				Args:         []string{"--stdio"},
				FilePatterns: []string{"**/*.js", "**/*.jsx", "**/*.mjs", "**/*.cjs"}, Markers: []string{"package.json"},
			},
		},
		Heartbeat: HeartbeatConfig{TimeoutSec: 60, IntervalSec: 15},
		Pool:      PoolConfig{IdleTTLSec: 900, MaxServers: 6, MemoryLimitMB: 1024},
		Codegraph: CodegraphConfig{Bin: "codegraph", AutoInit: false, SyncOnChange: true, TimeoutSec: 120},
		Memory:    MemoryConfig{},
		Ignore:    DefaultIgnore,
	}
	return cfg
}

// Load 加载配置: 显式路径或按约定搜索 (~/.config/mcp/axis/config.yaml),
// 与默认合并 (文件中的字段覆盖默认)。
func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		path = searchDefault()
	}
	if path == "" {
		return cfg, nil // 无配置文件, 用默认
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil // 显式路径不存在 = 用默认
		}
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	var fileCfg Config
	if err := yaml.Unmarshal(data, &fileCfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s (YAML): %w", path, err)
	}
	// 合并: 文件中的 adapter 覆盖默认同语言, 新增语言追加
	for k, a := range fileCfg.Adapters {
		cfg.Adapters[k] = a
	}
	mergeHeartbeat(&cfg.Heartbeat, &fileCfg.Heartbeat)
	mergePool(&cfg.Pool, &fileCfg.Pool)
	if fileCfg.Codegraph.Bin != "" {
		cfg.Codegraph.Bin = fileCfg.Codegraph.Bin
	}
	cfg.Codegraph.AutoInit = fileCfg.Codegraph.AutoInit
	cfg.Codegraph.SyncOnChange = fileCfg.Codegraph.SyncOnChange
	if fileCfg.Codegraph.TimeoutSec != 0 {
		cfg.Codegraph.TimeoutSec = fileCfg.Codegraph.TimeoutSec
	}
	// memory 配置: 合并 (空字段保持默认, 默认路径由 memory 包决定)
	if fileCfg.Memory.DBPath != "" {
		cfg.Memory.DBPath = fileCfg.Memory.DBPath
	}
	if fileCfg.Memory.ExportDir != "" {
		cfg.Memory.ExportDir = fileCfg.Memory.ExportDir
	}
	if fileCfg.Memory.Extension != "" {
		cfg.Memory.Extension = fileCfg.Memory.Extension
	}
	if fileCfg.CtrlToken != "" {
		cfg.CtrlToken = fileCfg.CtrlToken
	}
	// 忽略规则: 文件配置非空则整体覆盖默认 (用户显式声明自己的忽略集)
	if len(fileCfg.Ignore) > 0 {
		cfg.Ignore = fileCfg.Ignore
	}
	// 日志配置: 合并
	if fileCfg.Log.Path != "" {
		cfg.Log.Path = fileCfg.Log.Path
	}
	if fileCfg.Log.Level != "" {
		cfg.Log.Level = fileCfg.Log.Level
	}
	cfg.Log.Also = fileCfg.Log.Also
	return cfg, nil
}

// searchDefault 按约定搜索配置: ~/.config/mcp/axis/config.yaml (XDG 优先)。
func searchDefault() string {
	cands := []string{
		"configs/axis.yaml",
		"axis.yaml",
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		cands = append(cands, filepath.Join(x, "mcp", "axis", "config.yaml"))
	} else if h, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(h, ".config", "mcp", "axis", "config.yaml"))
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func mergeHeartbeat(dst, src *HeartbeatConfig) {
	if src.TimeoutSec != 0 {
		dst.TimeoutSec = src.TimeoutSec
	}
	if src.IntervalSec != 0 {
		dst.IntervalSec = src.IntervalSec
	}
}

func mergePool(dst, src *PoolConfig) {
	if src.IdleTTLSec != 0 {
		dst.IdleTTLSec = src.IdleTTLSec
	}
	if src.MaxServers != 0 {
		dst.MaxServers = src.MaxServers
	}
	if src.MemoryLimitMB != 0 {
		dst.MemoryLimitMB = src.MemoryLimitMB
	}
}
