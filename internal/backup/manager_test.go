package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"aiassistant/internal/store"
	_ "modernc.org/sqlite"
)

func TestBackupOverwriteAndPreserveOnFailure(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser("first", store.HashToken("first"), false); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(st, filepath.Join(t.TempDir(), "backups"))
	if err != nil {
		t.Fatal(err)
	}
	path, err := m.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != Filename {
		t.Fatalf("备份文件名错误: %s", path)
	}
	assertBackup(t, path, 1)

	if _, err := st.CreateUser("second", store.HashToken("second"), false); err != nil {
		t.Fatal(err)
	}
	secondPath, err := m.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if secondPath != path {
		t.Fatalf("备份路径发生变化: %s -> %s", path, secondPath)
	}
	assertBackup(t, path, 2)
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != Filename {
		t.Fatalf("备份目录存在残留文件: %+v", entries)
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunOnce(context.Background()); err == nil {
		t.Fatal("源库关闭后备份仍然成功")
	}
	assertBackup(t, path, 2)
}

func assertBackup(t *testing.T, path string, wantUsers int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var quick string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&quick); err != nil || quick != "ok" {
		t.Fatalf("备份完整性异常: %s, %v", quick, err)
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 13 {
		t.Fatalf("备份 schema 版本异常: %d, %v", version, err)
	}
	var users int
	if err := db.QueryRow(`SELECT count(*) FROM users`).Scan(&users); err != nil || users != wantUsers {
		t.Fatalf("备份用户数=%d，期望 %d，err=%v", users, wantUsers, err)
	}
}
