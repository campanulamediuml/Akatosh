package memory

import (
	"os"
	"testing"
	"time"

	"aiassistant/internal/store"
)

func TestMySQLMemoryStore(t *testing.T) {
	dsn := os.Getenv("AKATOSH_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 AKATOSH_MYSQL_TEST_DSN")
	}
	st, err := store.OpenMySQL(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	user, err := st.CreateUser("mysql-memory", store.HashToken("mysql-memory-"+time.Now().String()), false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.DeleteUser(user.ID)
	provider, err := st.CreateProvider(user.ID, "记忆测试", "https://example.com/v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModels(provider.ID, []store.Model{{ModelID: "background", Reasoning: true, Efforts: []string{"low"}}}); err != nil {
		t.Fatal(err)
	}
	conv, err := st.CreateConversation(user.ID, "记忆会话", provider.ID, "background", "low")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(st, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	want := BackgroundSettings{ProviderID: provider.ID, ModelID: "background", Effort: "low"}
	if err := manager.SetBackgroundSettings(user.ID, want); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetBackgroundSettings(user.ID, want); err != nil {
		t.Fatal(err)
	}
	got, err := manager.BackgroundSettings(user.ID)
	if err != nil || got != want {
		t.Fatalf("后台设置不对: %+v %v", got, err)
	}
	if _, err := manager.SetSessionEnabled(user.ID, conv.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetSessionEnabled(user.ID, conv.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetSessionEnabled(user.ID, conv.ID, true); err != nil {
		t.Fatal(err)
	}
	u, err := manager.user(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.setMaintenance("last_report", "ok"); err != nil {
		t.Fatal(err)
	}
	if status := u.maintenanceStatus(); status.LastReport != "ok" {
		t.Fatalf("维护设置不对: %+v", status)
	}
	record, err := u.session(conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.ArchivePath = "archive.md"
	record.ArchiveHash = "hash"
	record.SourceFingerprint = "source"
	record.IndexCard = "索引"
	record.ArchivedAt = time.Now().UnixMilli()
	if err := u.saveArchive(record); err != nil {
		t.Fatal(err)
	}
	loaded, err := u.session(conv.ID)
	if err != nil || loaded.ArchivePath != "archive.md" || loaded.IndexCard != "索引" || !loaded.Enabled {
		t.Fatalf("会话记忆读写失败: %+v %v", loaded, err)
	}
	if err := manager.DeleteUser(user.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"memory_session_states", "memory_maintenance", "memory_settings"} {
		var count int
		if err := st.SQLDB().QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE user_id=?`, user.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("删除用户后 %s 仍有 %d 行: %v", table, count, err)
		}
	}
}
