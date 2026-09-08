// Package memory — axis memory 子模块。
//
// 架构铁律 (用户确认):
//   - sqlite = SSOT。文档正文/结构化字段/链接全部只存在 DB。
//   - 磁盘 md 是同步视图: 项目笔记放 <项目根>/.axis/<category>/<name>.md (随项目走),
//     global 笔记放 state 目录。agent 可用 opencode write/edit 直接编辑 md,
//     然后调 mem_update(key) 从文件读入 DB (DB 覆盖导出回文件, 保持一致)。
//   - 链接是 N:N, 只存在于 DB (links 表); md 文本无链接语法 (写 [[...]] 仅得提示,
//     需用 mem_link 建连)。
//   - 唯一合法写路径 = MCP 工具 (mem_*) 改 DB。
//   - mem_update 整篇文本: 解析+校验, frontmatter 与 DB 不符 → warning+忽略结构化字段, 只应用正文。
//   - 结构化字段/链接只能用独立工具 (mem_field / mem_link) 改 DB。
package memory

// Doc 一篇笔记文档 (DB=SSOT)。
type Doc struct {
	Key         string `gorm:"column:key;primaryKey"`                    // 文档键 (语义名, 无扩展名, 全局唯一)
	Title       string `gorm:"column:title"`                             // frontmatter title (结构化字段, 由 mem_field/解析维护)
	Body        string `gorm:"column:body;type:text"`                    // md 正文 (不含 frontmatter)
	Frontmatter string `gorm:"column:frontmatter;type:text"`             // 原始 frontmatter yaml 块 (内部 yaml 字符串)
	Revision    int64  `gorm:"column:revision"`                          // 版本号 (每次 update 递增, 冲突/并发仲裁用)
	ContentHash string `gorm:"column:content_hash"`                      // 正文+标题 hash (快速 diff 检测)
	CreatedAt   int64  `gorm:"column:created_at"`                        // unix 毫秒
	UpdatedAt   int64  `gorm:"column:updated_at"`                        // unix 毫秒
}

func (Doc) TableName() string { return "docs" }

// Field 结构化字段 (独立于正文, 只能 mem_field 改)。
type Field struct {
	Key       string `gorm:"column:doc_key;primaryKey;index"` // 所属文档
	Name      string `gorm:"column:name;primaryKey"`          // 字段名 (如 tags/type/status)
	Value     string `gorm:"column:value;type:text"`          // 字段值 (yaml/json 标量或列表字符串)
	UpdatedAt int64  `gorm:"column:updated_at"`
}

func (Field) TableName() string { return "fields" }

// Link 链接关系 (只存在于 DB, md 文本无链接语法)。
type Link struct {
	SrcKey    string `gorm:"column:src_key;primaryKey;index"` // 源文档
	DstKey    string `gorm:"column:dst_key;primaryKey;index"` // 目标文档
	Type      string `gorm:"column:type;primaryKey"`          // 关系类型 (如 related/child/ref)
	Label     string `gorm:"column:label"`                    // 可选标签/说明
	CreatedAt int64  `gorm:"column:created_at"`
}

func (Link) TableName() string { return "links" }

// DocView 检索返回视图 (含字段与链接展开)。
type DocView struct {
	Key         string            `json:"key"`
	Title       string            `json:"title"`
	Body        string            `json:"body"`
	Frontmatter map[string]string `json:"frontmatter,omitempty"` // 结构化字段名→值 (非原始 yaml)
	Fields      map[string]string `json:"fields,omitempty"`      // 独立字段
	OutLinks    []LinkRef         `json:"out_links,omitempty"`   // 出链
	InLinks     []LinkRef         `json:"in_links,omitempty"`    // 反链 (查 DB)
	Revision    int64             `json:"revision"`
	UpdatedAt   int64             `json:"updated_at"`
}

// LinkRef 链接引用视图。
type LinkRef struct {
	Key     string `json:"key"`
	Type    string `json:"type"`
	Label   string `json:"label,omitempty"`
}

// UpdateResult update 结果。
type UpdateResult struct {
	Key       string   `json:"key"`
	Applied   bool     `json:"applied"`               // 正文是否入库
	Revision  int64    `json:"revision,omitempty"`    // 入库后的版本
	Warnings  []string `json:"warnings,omitempty"`    // frontmatter 忽略等警告
	Diff      *TextDiff `json:"diff,omitempty"`       // 与旧版正文 diff (无变化时为 nil)
	LinkHints []string `json:"link_hints,omitempty"`  // diff 中疑似"意图连线"提示 (需 mem_link 手动连)
}

// TextDiff 正文变化摘要 (行级增删统计, 不做完整 diff 输出)。
type TextDiff struct {
	OldLen  int `json:"old_len"`
	NewLen  int `json:"new_len"`
	Added   int `json:"added"`
	Removed int `json:"removed"`
}
