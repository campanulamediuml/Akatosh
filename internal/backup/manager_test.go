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

func TestBackupRollingAndPreserveOnFailure(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser("first", store.HashToken("first"), false); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, LegacyFilename)
	if err := os.WriteFile(legacy, []byte("old backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(st, dir)
	if err != nil {
		t.Fatal(err)
	}
	m.retain = 2

	first, err := m.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertBackup(t, first, 1)
	if _, err := st.CreateUser("second", store.HashToken("second"), false); err != nil {
		t.Fatal(err)
	}
	second, err := m.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("同秒内的两份备份互相覆盖")
	}
	assertBackup(t, first, 1)
	assertBackup(t, second, 2)
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("旧单文件备份没有被清理")
	}

	if _, err := st.CreateUser("third", store.HashToken("third"), false); err != nil {
		t.Fatal(err)
	}
	third, err := m.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertBackup(t, third, 3)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("滚存后文件数=%d，期望 2: %+v", len(entries), entries)
	}
	t.Logf("first=%s second=%s third=%s entries=%v", first, second, third, entries)
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatal("最旧备份没有被淘汰")
	}
	assertBackup(t, second, 2)
	assertBackup(t, third, 3)

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunOnce(context.Background()); err == nil {
		t.Fatal("源库关闭后备份仍然成功")
	}
	entries, err = os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("失败后既有备份发生变化: %+v, %v", entries, err)
	}
	assertBackup(t, third, 3)
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
