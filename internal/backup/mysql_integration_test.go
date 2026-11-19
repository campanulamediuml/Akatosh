package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aiassistant/internal/memory"
	"aiassistant/internal/store"
)

func TestMySQLBackup(t *testing.T) {
	dsn := os.Getenv("AKATOSH_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 AKATOSH_MYSQL_TEST_DSN")
	}
	st, err := store.OpenMySQL(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	user, err := st.CreateUser("mysql-backup", store.HashToken("mysql-backup-"+time.Now().String()), false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.DeleteUser(user.ID)
	mem, err := memory.NewManager(st, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	defer mem.DeleteUser(user.ID)
	if _, err := mem.BackgroundSettings(user.ID); err != nil {
		t.Fatal(err)
	}

	manager, err := NewManager(st, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(path) != ".sql" {
		t.Fatalf("MySQL 备份应为 .sql: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, marker := range []string{"Akatosh MySQL backup", "CREATE TABLE `users`", "CREATE TABLE `memory_settings`", "Akatosh backup complete"} {
		if !strings.Contains(text, marker) {
			t.Fatalf("备份缺少 %q", marker)
		}
	}
}
