package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Service memory 子模块编排层: 组合 Store + md 解析 + 导出。
// 所有 mem_* MCP 工具都经这里 (唯一写入口)。
type Service struct {
	store    *Store
	exportDir string
	ext      string
}

// NewService 构建 Service (需已 Open 的 store)。
func NewService(store *Store, exportDir, ext string) *Service {
	if ext == "" {
		ext = "md"
	}
	return &Service{store: store, exportDir: exportDir, ext: ext}
}

// OpenService 便捷: 打开库并建 Service。
func OpenService(dbPath, exportDir, ext string) (*Service, error) {
	st, err := Open(dbPath)
	if err != nil {
		return nil, err
	}
	return NewService(st, exportDir, ext), nil
}

// Close 关闭。
func (s *Service) Close() error { return s.store.Close() }

// DocExists 文档是否存在。
func (s *Service) DocExists(key string) (bool, error) {
	d, err := s.store.GetDoc(key)
	return d != nil, err
}

// ---------- Update (唯一正文写路径) ----------

// Update 整篇文本更新:
//  1. 解析 frontmatter: 与 DB 不符 → warning+忽略结构化字段 (只应用正文)
//  2. 校验正文非空
//  3. diff vs 旧版 (行统计); 若出现疑似"意图连线"标记 → link hint
//  4. 入库 (revision++)
//  5. 导出映射 md (覆盖, 丢弃本地未提交改动——无 update 的一切手改均无效)
func (s *Service) Update(key, text string) (*UpdateResult, error) {
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("key 必填")
	}
	parsed := SplitFrontmatter(text)
	if err := ValidateBody(parsed.Body); err != nil {
		return nil, err
	}

	old, _ := s.store.GetDoc(key)
	res := &UpdateResult{Key: key}
	res.Warnings = append(res.Warnings, parsed.Warnings...)

	// title 字段 (来自 frontmatter 的 title, 若存在)
	title := parsed.Frontmatter["title"]

	// frontmatter 结构化字段 vs DB: 有差异 → warning + 忽略 (不覆盖 DB 字段)
	if old != nil {
		dbFields, _ := s.store.GetFields(key)
		for name, val := range parsed.Frontmatter {
			if name == "title" {
				// title 属于结构化: DB 有值且不同 → warning
				if old.Title != "" && old.Title != title {
					res.Warnings = append(res.Warnings, fmt.Sprintf("frontmatter title 与 DB 不符 (DB=%q, md=%q), 已忽略 (改结构化字段用 mem_field)", old.Title, title))
					title = old.Title // 保持 DB
				}
				continue
			}
			if dbVal, ok := dbFields[name]; ok && dbVal != val {
				res.Warnings = append(res.Warnings, fmt.Sprintf("frontmatter 字段 %q 与 DB 不符 (DB=%q, md=%q), 已忽略 (改结构化字段用 mem_field)", name, dbVal, val))
			} else if !ok {
				// DB 无此字段: 新字段出现在 md frontmatter → 同样忽略 (字段只能 mem_field)
				res.Warnings = append(res.Warnings, fmt.Sprintf("frontmatter 字段 %q 在 DB 不存在, 已忽略 (建字段用 mem_field)", name))
			}
		}
	}

	// 正文 diff
	var diff *TextDiff
	if old != nil {
		oldBody := old.Body
		added := countDiffLines(oldBody, parsed.Body)
		diff = &TextDiff{OldLen: len(oldBody), NewLen: len(parsed.Body), Added: added, Removed: len(strings.Split(oldBody, "\n")) - len(strings.Split(parsed.Body, "\n")) + added}
		if old.ContentHash == contentFingerprint(title, parsed.Body) {
			diff = nil // 无变化
		}
	} else {
		// 新文档: 无 old, diff 显示新增
		diff = &TextDiff{OldLen: 0, NewLen: len(parsed.Body), Added: len(strings.Split(parsed.Body, "\n")), Removed: 0}
	}

	// 入库
	doc := &Doc{Key: key, Title: title, Body: parsed.Body, Frontmatter: parsed.FrontmatterRaw}
	if old != nil {
		doc.CreatedAt = old.CreatedAt
		doc.Revision = old.Revision
	}
	if err := s.store.UpsertDoc(doc); err != nil {
		return nil, err
	}
	res.Applied = true
	res.Revision = doc.Revision
	res.Diff = diff

	// link hints: 正文出现 [[...]] 或类似图谱引用意图 → 提示 mem_link 手动连
	res.LinkHints = detectLinkHints(parsed.Body)

	// 导出映射视图 (覆盖)
	if werr := s.ExportDoc(key); werr != nil {
		res.Warnings = append(res.Warnings, "导出失败: "+werr.Error())
	}
	return res, nil
}

// contentFingerprint 简单指纹 (正文+title 长度与内容, 不用 hash 库保持轻)。
func contentFingerprint(title, body string) string {
	return fmt.Sprintf("%d:%d:%s", len(title), len(body), firstLine(body))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func countDiffLines(oldS, newS string) int {
	oldSet := map[string]int{}
	for _, l := range strings.Split(oldS, "\n") {
		oldSet[l]++
	}
	added := 0
	for _, l := range strings.Split(newS, "\n") {
		if oldSet[l] > 0 {
			oldSet[l]--
		} else {
			added++
		}
	}
	return added
}

// detectLinkHints 检测正文中疑似"意图连线"的标记:
//   - [[key]] 双链语法 (虽然 md 无链接, LLM 可能仍写) → 提示用 mem_link
//   - "(link: key)" / "连线: key" 指示
func detectLinkHints(body string) []string {
	var hints []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		// [[...]]
		for {
			start := strings.Index(trimmed, "[[")
			if start < 0 {
				break
			}
			end := strings.Index(trimmed[start+2:], "]]")
			if end < 0 {
				break
			}
			target := trimmed[start+2 : start+2+end]
			hints = append(hints, fmt.Sprintf("正文含 [[%s]] — 链接只存在于 DB, 若需建立请用 mem_link", target))
			trimmed = trimmed[start+2+end+2:]
		}
		// 指示语 (link/连线 开头行)
		low := strings.ToLower(trimmed)
		for _, marker := range []string{"(link:", "(连线:", "(connect:"} {
			if strings.HasPrefix(low, marker) && strings.Contains(trimmed, ")") {
				content := trimmed[len(marker) : strings.Index(trimmed, ")")]
				hints = append(hints, fmt.Sprintf("正文含连线指示 %q — 链接只存在于 DB, 请用 mem_link 手动建立", content))
				break
			}
		}
	}
	return hints
}

// ---------- Retrieve (读, 展开字段+链接) ----------

// Retrieve 读文档完整视图 (含字段/出链/反链, 来自 DB)。
func (s *Service) Retrieve(key string) (*DocView, error) {
	d, err := s.store.GetDoc(key)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("文档不存在: %s", key)
	}
	fields, _ := s.store.GetFields(key)
	out, _ := s.store.OutLinks(key)
	in, _ := s.store.InLinks(key)
	// frontmatter 视图 = fields 合并 title
	fm := map[string]string{}
	for k, v := range fields {
		fm[k] = v
	}
	if d.Title != "" {
		fm["title"] = d.Title
	}
	return &DocView{
		Key: d.Key, Title: d.Title, Body: d.Body,
		Frontmatter: fm, Fields: fields,
		OutLinks: out, InLinks: in,
		Revision: d.Revision, UpdatedAt: d.UpdatedAt,
	}, nil
}

// Find 全文检索。
func (s *Service) Find(query string, limit int) ([]SearchHit, error) {
	return s.store.Search(query, limit)
}

// List 全部文档键。
func (s *Service) List() ([]string, error) { return s.store.ListKeys() }

// Field 设置结构化字段 (唯一字段写路径)。
func (s *Service) Field(key, name, value string) error {
	if _, err := s.store.GetDoc(key); err != nil {
		return err
	}
	return s.store.SetField(key, name, value)
}

// FieldDelete 删字段。
func (s *Service) FieldDelete(key, name string) error {
	return s.store.DeleteField(key, name)
}

// Link 建链接 (唯一链接写路径)。
func (s *Service) Link(src, dst, typ, label string) error {
	if src == dst {
		return fmt.Errorf("自链接不允许 (src==dst==%s)", src)
	}
	if typ == "" {
		typ = "related"
	}
	return s.store.AddLink(src, dst, typ, label)
}

// LinkDelete 断链接。
func (s *Service) LinkDelete(src, dst, typ string) error {
	if typ == "" {
		typ = "related"
	}
	return s.store.DeleteLink(src, dst, typ)
}

// ExportAll 全部文档导出为 md 映射视图 (DB → 磁盘覆盖)。
func (s *Service) ExportAll() (int, error) {
	keys, err := s.store.ListKeys()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, k := range keys {
		if err := s.ExportDoc(k); err == nil {
			n++
		}
	}
	return n, nil
}

// ExportDoc 单文档导出 (覆盖写, 不跑 formatter)。
func (s *Service) ExportDoc(key string) error {
	d, err := s.store.GetDoc(key)
	if err != nil {
		return err
	}
	if d == nil {
		return fmt.Errorf("文档不存在: %s", key)
	}
	fields, _ := s.store.GetFields(key)
	md := RenderMD(key, fields, d.Title, d.Body)
	return s.writeExport(key, md)
}

// ExportPath 返回某 key 导出文件的绝对路径。
func (s *Service) ExportPath(key string) string {
	return filepath.Join(s.exportDir, key+"."+s.ext)
}

// writeExport 原子写导出文件 (temp+rename)。key 可含 category 路径 (a/b/name → <dir>/a/b/name.md)。
func (s *Service) writeExport(key, content string) error {
	if s.exportDir == "" {
		return nil // 未配置导出目录 → 跳过 (纯 DB 模式)
	}
	path := s.ExportPath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------- 默认路径 ----------

// DefaultPaths 返回默认库目录与导出目录 (~/.local/state/axis/memdb, .../memory)。
// 多项目隔离: 库目录下 global.db + <proj-slug>.db 各项目一库; 导出目录按项目分子目录。
func DefaultPaths() (dbDir, exportDir string) {
	base := ""
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		base = x
	} else if h, err := os.UserHomeDir(); err == nil {
		base = filepath.Join(h, ".local", "state")
	}
	base = filepath.Join(base, "axis")
	return filepath.Join(base, "memdb"), filepath.Join(base, "memory")
}
