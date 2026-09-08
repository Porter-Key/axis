// Package memory — axis 子插件: 知识库 (每项目独立 sqlite=SSOT + md 映射导出)。
//
// 项目隔离 (用户方案): 每个激活项目一个独立 .db; 全局数据固定 global 库,
// 显式 project=global 才可见。目录级工具必须经 axis_activate 绑定会话,
// 未激活 → 拒绝服务。本文件: 插件壳 + 项目库管理。
package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/Porter-Key/axis/internal/config"
	"github.com/Porter-Key/axis/internal/plugin"
)

// maxOpenServices 同时打开的项目库上限 (防不同项目无限累积): 超限逐出最久未用。
const maxOpenServices = 32

// svcEntry 缓存的开库条目 (带活跃度, 供逐出)。
type svcEntry struct {
	svc     *Service
	lastUse time.Time
}

// Plugin memory 插件实例: 按激活项目懒开库, 缓存 open 的 Service。
type Plugin struct {
	gate *plugin.Gate

	baseDir   string // 库根目录 (~/.local/state/axis/memory 或 config.Memory.DBPath 的目录)
	exportDir string
	ext       string

	// outFormat 输出编码 (""/json/gcf, atomic.Value 存 string, 本插件 GCF 适配器开关)。
	outFormat atomic.Value

	mu   sync.Mutex
	svcs map[string]*svcEntry // 库 key (project 根/global) → 条目
}

// NewPlugin 构建 memory 插件。baseDir 是放各项目 .db 的目录。
func NewPlugin(baseDir, exportDir, ext string) *Plugin {
	if ext == "" {
		ext = "md"
	}
	return &Plugin{
		baseDir:   baseDir,
		exportDir: exportDir,
		ext:       ext,
		svcs:      map[string]*svcEntry{},
	}
}

// Name 插件名。
func (p *Plugin) Name() string { return "memory" }

// SetGate 注入会话门禁 (axis 壳调用)。
func (p *Plugin) SetGate(g *plugin.Gate) { p.gate = g }

// Gate 暴露 (测试/控制面)。
func (p *Plugin) Gate() *plugin.Gate { return p.gate }

// ProjectKey project 根 → 库 key。global → "global"。
func ProjectKey(project string) string {
	if project == "" || project == "global" {
		return "global"
	}
	return filepath.Clean(project)
}

// dbPathFor project → db 文件路径。global → global.db, 项目 → <hash>.db。
func dbPathFor(baseDir, project string) string {
	if project == "global" {
		return filepath.Join(baseDir, "global.db")
	}
	abs := filepath.Clean(project)
	// 路径安全化: 替换分隔符/点 → 短哈希后缀避免碰撞与非法字符
	slug := pathSlug(abs)
	return filepath.Join(baseDir, slug+".db")
}

// exportDirFor project → 导出目录。
// 项目 → <项目根>/.axis (项目自包含: md 随项目走, agent 可直接 write/edit 再 mem_update);
// global → state 目录下 memory (全局笔记不在项目内)。
func (p *Plugin) exportDirFor(project string) string {
	if project == "global" {
		return filepath.Join(p.exportDir, "global")
	}
	return filepath.Join(filepath.Clean(project), ".axis")
}

// svcFor 取 (或懒开) 项目的 Service。project 为空=global。
// 开库是磁盘 IO (sqlite Open + 建表): 锁外做, 不阻塞其他项目的并发请求;
// 回填时双检, 并发先开者胜, 多余副本关闭。上限 maxOpenServices, 超限逐出最久未用。
func (p *Plugin) svcFor(project string) (*Service, error) {
	key := ProjectKey(project)
	p.mu.Lock()
	if e, ok := p.svcs[key]; ok {
		e.lastUse = time.Now()
		s := e.svc
		p.mu.Unlock()
		return s, nil
	}
	p.mu.Unlock()

	db := dbPathFor(p.baseDir, key)
	exp := p.exportDirFor(key)
	s, err := OpenService(db, exp, p.ext)
	if err != nil {
		return nil, fmt.Errorf("memory 开库 %s: %w", db, err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.svcs[key]; ok {
		e.lastUse = time.Now()
		_ = s.Close() // 并发下已被先开: 用已有的, 关多余副本
		return e.svc, nil
	}
	if len(p.svcs) >= maxOpenServices {
		var victim string
		var oldest time.Time
		first := true
		for k, e := range p.svcs {
			if first || e.lastUse.Before(oldest) {
				victim, oldest, first = k, e.lastUse, false
			}
		}
		if old, ok := p.svcs[victim]; ok {
			delete(p.svcs, victim)
			_ = old.svc.Close()
		}
	}
	p.svcs[key] = &svcEntry{svc: s, lastUse: time.Now()}
	return s, nil
}

// svcForCtx 从 ctx 会话取激活项目, 再开对应库。
// 目录级工具入口: 未激活 → (nil, ErrNotActivated)。
func (p *Plugin) svcForCtx(ctx context.Context) (*Service, error) {
	if p.gate == nil {
		return nil, fmt.Errorf("memory 门禁未注入")
	}
	proj, ok := p.gate.ProjectFor(ctx)
	if !ok {
		if h := p.gate.RecoveryHint(); h != "" {
			return nil, fmt.Errorf("%w。%s", ErrNotActivated, h)
		}
		return nil, ErrNotActivated
	}
	return p.svcFor(proj)
}

// ErrNotActivated 未激活错误。
var ErrNotActivated = fmt.Errorf("未激活项目: 请先调 axis_activate(project) 绑定目录级工具")

// ---------- 生命周期 ----------

func (p *Plugin) Start(ctx context.Context) error { return nil }

func (p *Plugin) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.svcs {
		_ = e.svc.Close()
	}
	p.svcs = map[string]*svcEntry{}
	return nil
}

// ---------- 工具注册 ----------

// Register 注册 memory 工具组。
// 输出契约: 配置 output.format=gcf 时输出 GCF generic 画像 (失败回退原 JSON)。
func (p *Plugin) Register(ms *server.MCPServer) {
	ms.AddTool(mcp.NewTool("mem_update",
		mcp.WithDescription("整篇正文入库 (唯一正文写路径)。作用于当前激活项目; 显式 project=global 写全局。解析+校验; frontmatter 与 DB 不符→warning+忽略结构化字段; 正文 diff 统计 + 链接意图提示 (不自动连线)。"),
		mcp.WithString("key", mcp.Required(), mcp.Description("文档键 (语义名, 无扩展名)")),
		mcp.WithString("text", mcp.Required(), mcp.Description("整篇 md (可含 frontmatter, 仅正文生效)")),
		mcp.WithString("project", mcp.Description("目标项目 (绝对路径) 或 global; 缺省=当前激活项目")),
	), p.handleUpdate)

	ms.AddTool(mcp.NewTool("mem_retrieve",
		mcp.WithDescription("读文档完整视图 (含字段/出链/反链)。作用于当前激活项目; 显式 project=global 读全局。"),
		mcp.WithString("key", mcp.Required()),
		mcp.WithString("project", mcp.Description("项目 (绝对路径) 或 global; 缺省=当前激活项目")),
	), p.handleRetrieve)

	ms.AddTool(mcp.NewTool("mem_find",
		mcp.WithDescription("全文检索。作用于当前激活项目; 显式 project=global 搜全局。"),
		mcp.WithString("query", mcp.Required()),
		mcp.WithNumber("limit", mcp.Description("返回条数 (默认 20)")),
		mcp.WithString("project", mcp.Description("项目 (绝对路径) 或 global; 缺省=当前激活项目")),
	), p.handleFind)

	ms.AddTool(mcp.NewTool("mem_list",
		mcp.WithDescription("列出文档键 (当前激活项目)。"),
		mcp.WithString("project", mcp.Description("项目 (绝对路径) 或 global; 缺省=当前激活项目")),
	), p.handleList)

	ms.AddTool(mcp.NewTool("mem_field",
		mcp.WithDescription("结构化字段写 (唯一字段写路径)。value 空=删除。作用于当前激活项目。"),
		mcp.WithString("key", mcp.Required()),
		mcp.WithString("name", mcp.Required(), mcp.Description("字段名 (如 tags/type/status)")),
		mcp.WithString("value", mcp.Description("字段值 (标量/序列化字符串); 空=删该字段")),
		mcp.WithString("project", mcp.Description("项目 (绝对路径) 或 global; 缺省=当前激活项目")),
	), p.handleField)

	ms.AddTool(mcp.NewTool("mem_link",
		mcp.WithDescription("建链接 (唯一链接写路径)。链接只存在于 DB, md 无链接语法。作用于当前激活项目。"),
		mcp.WithString("src", mcp.Required()),
		mcp.WithString("dst", mcp.Required()),
		mcp.WithString("type", mcp.Description("关系类型 (默认 related)")),
		mcp.WithString("label", mcp.Description("可选标签")),
		mcp.WithString("project", mcp.Description("项目 (绝对路径) 或 global; 缺省=当前激活项目")),
	), p.handleLink)

	ms.AddTool(mcp.NewTool("mem_export",
		mcp.WithDescription("DB → md 映射视图覆盖导出 (不跑 formatter)。key 空=全量。作用于当前激活项目。"),
		mcp.WithString("key", mcp.Description("单文档键 (空=全量导出)")),
		mcp.WithString("project", mcp.Description("项目 (绝对路径) 或 global; 缺省=当前激活项目")),
	), p.handleExport)

	ms.AddTool(mcp.NewTool("mem_status",
		mcp.WithDescription("文档状态 (存在/版本/导出路径)。"),
		mcp.WithString("key", mcp.Required()),
		mcp.WithString("project", mcp.Description("项目 (绝对路径) 或 global; 缺省=当前激活项目")),
	), p.handleStatus)
}

// pathSlug 绝对路径 → 文件系统安全短名 (哈希+尾段, 防碰撞与非法字符)。
func pathSlug(abs string) string {
	h := fnvHash(abs)
	base := filepath.Base(strings.TrimRight(abs, "/"))
	if base == "" || base == "." || base == "/" {
		base = "root"
	}
	return fmt.Sprintf("%s-%x", sanitize(base), h)
}

// sanitize 只留字母数字-_.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "proj"
	}
	return b.String()
}

// fnvHash FNV-1a 32 位短哈希。
func fnvHash(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

var _ = config.MemoryConfig{} // 文档引用
