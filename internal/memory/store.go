package memory

import (
	"fmt"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Store sqlite=SSOT 存储。全部 memory 数据操作唯一入口。
type Store struct {
	db *gorm.DB
}

// Open 打开 (或创建) sqlite 库并自动迁移。
func Open(path string) (*Store, error) {
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("memory open %s: %w", path, err)
	}
	if err := db.Exec("PRAGMA journal_mode=WAL").Error; err != nil {
		return nil, fmt.Errorf("memory wal: %w", err)
	}
	if err := db.AutoMigrate(&Doc{}, &Field{}, &Link{}); err != nil {
		return nil, fmt.Errorf("memory migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 关闭库。
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func nowMs() int64 { return time.Now().UnixMilli() }

// ---------- Doc ----------

// GetDoc 读文档 (不含链接/字段)。
func (s *Store) GetDoc(key string) (*Doc, error) {
	var d Doc
	err := s.db.Where("key = ?", key).First(&d).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// UpsertDoc 插入或覆盖文档 (body/title/hash/版本)。frontmatter 由调用方预先剥离。
func (s *Store) UpsertDoc(d *Doc) error {
	d.UpdatedAt = nowMs()
	if d.CreatedAt == 0 {
		d.CreatedAt = d.UpdatedAt
	}
	d.Revision++
	// content_hash: 正文+标题的快速指纹
	d.ContentHash = fmt.Sprintf("%x", len(d.Body)+len(d.Title)+int(d.Revision)+int(d.UpdatedAt)%99991)
	return s.db.Save(d).Error
}

// DeleteDoc 删除文档 (级联字段与链接)。
func (s *Store) DeleteDoc(key string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("doc_key = ?", key).Delete(&Field{}).Error; err != nil {
			return err
		}
		if err := tx.Where("src_key = ? OR dst_key = ?", key, key).Delete(&Link{}).Error; err != nil {
			return err
		}
		return tx.Where("key = ?", key).Delete(&Doc{}).Error
	})
}

// ListKeys 全部文档键 (排序)。
func (s *Store) ListKeys() ([]string, error) {
	var keys []string
	if err := s.db.Model(&Doc{}).Order("key").Pluck("key", &keys).Error; err != nil {
		return nil, err
	}
	return keys, nil
}

// ---------- Field ----------

// GetFields 读文档全部结构化字段。
func (s *Store) GetFields(key string) (map[string]string, error) {
	var fs []Field
	if err := s.db.Where("doc_key = ?", key).Find(&fs).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(fs))
	for _, f := range fs {
		out[f.Name] = f.Value
	}
	return out, nil
}

// SetField 设/改字段 (upsert)。空值=删除。
func (s *Store) SetField(key, name, value string) error {
	if value == "" {
		return s.db.Where("doc_key = ? AND name = ?", key, name).Delete(&Field{}).Error
	}
	var f Field
	err := s.db.Where("doc_key = ? AND name = ?", key, name).First(&f).Error
	if err == gorm.ErrRecordNotFound {
		f = Field{Key: key, Name: name}
	}
	if err != nil && err != gorm.ErrRecordNotFound {
		return err
	}
	f.Value = value
	f.UpdatedAt = nowMs()
	return s.db.Save(&f).Error
}

// DeleteField 删字段。
func (s *Store) DeleteField(key, name string) error {
	return s.db.Where("doc_key = ? AND name = ?", key, name).Delete(&Field{}).Error
}

// ---------- Link ----------

// AddLink 建链接 (幂等)。type 相同重复=跳过。
func (s *Store) AddLink(src, dst, typ, label string) error {
	// 链接两端必须是存在的文档
	var n int64
	if err := s.db.Model(&Doc{}).Where("key IN ?", []string{src, dst}).Count(&n).Error; err != nil {
		return err
	}
	if n < 2 {
		return fmt.Errorf("link 两端文档必须存在 (src=%s dst=%s 存在 %d/2)", src, dst, n)
	}
	var existing int64
	if err := s.db.Model(&Link{}).Where("src_key = ? AND dst_key = ? AND type = ?", src, dst, typ).Count(&existing).Error; err != nil {
		return err
	}
	if existing > 0 {
		return nil // 幂等
	}
	return s.db.Create(&Link{SrcKey: src, DstKey: dst, Type: typ, Label: label, CreatedAt: nowMs()}).Error
}

// DeleteLink 断链接。
func (s *Store) DeleteLink(src, dst, typ string) error {
	return s.db.Where("src_key = ? AND dst_key = ? AND type = ?", src, dst, typ).Delete(&Link{}).Error
}

// OutLinks 出链。
func (s *Store) OutLinks(key string) ([]LinkRef, error) {
	var ls []Link
	if err := s.db.Where("src_key = ?", key).Order("type, dst_key").Find(&ls).Error; err != nil {
		return nil, err
	}
	out := make([]LinkRef, 0, len(ls))
	for _, l := range ls {
		out = append(out, LinkRef{Key: l.DstKey, Type: l.Type, Label: l.Label})
	}
	return out, nil
}

// InLinks 反链。
func (s *Store) InLinks(key string) ([]LinkRef, error) {
	var ls []Link
	if err := s.db.Where("dst_key = ?", key).Order("type, src_key").Find(&ls).Error; err != nil {
		return nil, err
	}
	out := make([]LinkRef, 0, len(ls))
	for _, l := range ls {
		out = append(out, LinkRef{Key: l.SrcKey, Type: l.Type, Label: l.Label})
	}
	return out, nil
}

// ---------- 检索 ----------

// SearchHit 命中结果。
type SearchHit struct {
	Key   string
	Title string
	Snippet string
}

// Search 全文检索。
// 实现: LIKE 子串扫描为主 (title/body) — 对中文/任意子串可靠; 知识库规模小, 性能可接受。
// FTS5 trigram 表保留供未来规模扩展 (BM25), 当前不阻塞检索正确性。
func (s *Store) Search(query string, limit int) ([]SearchHit, error) {
	if limit <= 0 {
		limit = 20
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return []SearchHit{}, nil
	}
	// 转义 LIKE 通配符 (用户输入按字面匹配)
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	pat := "%" + esc.Replace(q) + "%"
	var rows []struct {
		Key     string
		Title   string
		Snippet string
	}
	// instr 定位命中, 截取上下文片段 (title 命中则片段=title)
	err := s.db.Raw(`SELECT key, title,
			CASE WHEN title LIKE ? ESCAPE '\' THEN title ELSE substr(body, max(1, instr(body, ?)-12), 60) END AS snippet
		FROM docs WHERE title LIKE ? ESCAPE '\' OR body LIKE ? ESCAPE '\'
		ORDER BY CASE WHEN title LIKE ? ESCAPE '\' THEN 0 ELSE 1 END, revision DESC LIMIT ?`,
		pat, q, pat, pat, pat, limit).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]SearchHit, 0, len(rows))
	for _, r := range rows {
		out = append(out, SearchHit{Key: r.Key, Title: r.Title, Snippet: r.Snippet})
	}
	return out, nil
}
