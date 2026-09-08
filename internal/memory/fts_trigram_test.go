package memory

import "testing"

func TestTrigramMatch(t *testing.T) {
	s, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.db.Exec(`CREATE VIRTUAL TABLE t_fts USING fts5(a, tokenize='trigram')`).Error; err != nil {
		t.Skip("trigram 不可用:", err)
	}
	s.db.Exec(`INSERT INTO t_fts VALUES ('磁盘 md 只是映射视图 一个服务 加 多个 子插件')`)
	var n int64
	for _, q := range []string{`"映射视图"`, "映射视图", `"sqlite 是唯一"`, "sqlite 是唯一", `"插件"`} {
		err := s.db.Raw(`SELECT count(*) FROM t_fts WHERE t_fts MATCH ?`, q).Scan(&n).Error
		t.Logf("q=%q err=%v n=%d", q, err, n)
	}
}
