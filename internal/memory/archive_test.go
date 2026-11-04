package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aiassistant/internal/store"
)

type capturedComplete struct {
	Model  string
	Effort string
	Prompt string
}

type fakeArchiveUpstream struct {
	*httptest.Server
	mu       sync.Mutex
	requests []capturedComplete
	invalid  bool
}

func newFakeArchiveUpstream(t *testing.T) *fakeArchiveUpstream {
	t.Helper()
	f := &fakeArchiveUpstream{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string `json:"model"`
			Effort string `json:"reasoning_effort"`
			Msgs   []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		prompt := ""
		if len(body.Msgs) > 0 {
			prompt = body.Msgs[0].Content
		}
		f.mu.Lock()
		f.requests = append(f.requests, capturedComplete{Model: body.Model, Effort: body.Effort, Prompt: prompt})
		invalid := f.invalid
		f.mu.Unlock()
		content := `{"detail":"详细归档：项目决定与待办","index":{"goal":"验证长期记忆","topics":["记忆","测试"],"entities":["Akatosh"],"decisions":["按 Session 归档"],"rejected":["跨会话合并"],"lookup":["删除与修复策略"],"aliases":["session memory"],"status":"已完成"}}`
		if invalid {
			content = "not json"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": content},
		}}})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeArchiveUpstream) captured() []capturedComplete {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedComplete(nil), f.requests...)
}

type memoryFixture struct {
	st  *store.Store
	mem *Manager
	up  *fakeArchiveUpstream
}

func newMemoryFixture(t *testing.T) *memoryFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	up := newFakeArchiveUpstream(t)
	mem, err := NewManager(st, filepath.Join(t.TempDir(), "misc", "memory"))
	if err != nil {
		t.Fatal(err)
	}
	f := &memoryFixture{st: st, mem: mem, up: up}
	t.Cleanup(func() {
		_ = mem.Close()
		_ = st.Close()
	})
	return f
}

func (f *memoryFixture) userWithConversation(t *testing.T, token, title string) (int64, int64) {
	t.Helper()
	user, err := f.st.CreateUser(token, store.HashToken(token), false)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := f.st.CreateProvider(user.ID, token, f.up.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpsertModels(provider.ID, []store.Model{
		{ModelID: "chat-model", Reasoning: true, Efforts: []string{"low", "medium", "high"}},
		{ModelID: "background-model", Reasoning: true, Efforts: []string{"low", "medium", "high"}},
	}); err != nil {
		t.Fatal(err)
	}
	conv, err := f.st.CreateConversation(user.ID, title, provider.ID, "chat-model", "high")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.mem.SetBackgroundSettings(user.ID, BackgroundSettings{ProviderID: provider.ID, Effort: "low"}); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := f.mem.BackgroundSettings(user.ID); cfg.ProviderID != provider.ID || cfg.ModelID != "" {
		t.Fatalf("分步选择供应商被错误清空: %+v", cfg)
	}
	if err := f.mem.SetBackgroundSettings(user.ID, BackgroundSettings{ProviderID: provider.ID, ModelID: "background-model", Effort: "medium"}); err != nil {
		t.Fatal(err)
	}
	return user.ID, conv.ID
}

func (f *memoryFixture) addExchange(t *testing.T, convID int64, question, answer string) {
	t.Helper()
	if _, err := f.st.CreateMessage(store.Message{ConvID: convID, Role: store.RoleUser, Status: store.StatusDone, Content: question}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.CreateMessage(store.Message{ConvID: convID, Role: store.RoleAssistant, Status: store.StatusDone, Content: answer}); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveLifecycleAndUserIsolation(t *testing.T) {
	f := newMemoryFixture(t)
	u1, c1 := f.userWithConversation(t, "user-one", "用户一的项目")
	u2, c2 := f.userWithConversation(t, "user-two", "用户二的项目")
	f.addExchange(t, c1, "用户一的问题", "用户一的回答")
	f.addExchange(t, c2, "用户二的问题", "用户二的回答")
	if _, err := f.mem.SetSessionEnabled(u1, c1, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mem.SetSessionEnabled(u2, c2, true); err != nil {
		t.Fatal(err)
	}

	for _, userID := range []int64{u1, u2} {
		report, err := f.mem.RunMaintenance(context.Background(), userID)
		if err != nil || report.Created != 1 || report.Failed != 0 {
			t.Fatalf("归档报告 %+v, err=%v", report, err)
		}
	}
	for _, req := range f.up.captured() {
		if req.Model != "background-model" || req.Effort != "medium" {
			t.Fatalf("后台模型参数错误: %+v", req)
		}
	}
	index1, _ := f.mem.ReadIndex(u1)
	index2, _ := f.mem.ReadIndex(u2)
	if !strings.Contains(index1, fmt.Sprintf("Session #%d", c1)) || strings.Contains(index1, fmt.Sprintf("Session #%d：", c2)) {
		t.Fatalf("用户一索引串数据:\n%s", index1)
	}
	if !strings.Contains(index2, fmt.Sprintf("Session #%d", c2)) || strings.Contains(index2, fmt.Sprintf("Session #%d：", c1)) {
		t.Fatalf("用户二索引串数据:\n%s", index2)
	}
	if f.mem.UserRoot(u1) == f.mem.UserRoot(u2) {
		t.Fatal("两个用户共用了持久化目录")
	}
	if _, _, _, err := f.mem.ReadArchive(u1, c2, 0, 100); err == nil {
		t.Fatal("跨用户读取归档没有被拒绝")
	}
	content, total, more, err := f.mem.ReadArchive(u1, c1, 0, 10)
	if err != nil || content == "" || total <= len([]rune(content)) || !more {
		t.Fatalf("分段读取异常 content=%q total=%d more=%v err=%v", content, total, more, err)
	}

	if err := f.st.DeleteConversation(c1); err != nil {
		t.Fatal(err)
	}
	report, err := f.mem.RunMaintenance(context.Background(), u1)
	if err != nil || report.Deleted != 1 {
		t.Fatalf("删除修复报告 %+v, err=%v", report, err)
	}
	index1, _ = f.mem.ReadIndex(u1)
	if strings.Contains(index1, fmt.Sprintf("Session #%d：", c1)) {
		t.Fatal("源 Session 删除后索引仍有残留")
	}
	matches, _ := filepath.Glob(filepath.Join(f.mem.UserRoot(u1), "*", fmt.Sprintf("session-%d.txt", c1)))
	if len(matches) != 0 {
		t.Fatalf("源 Session 删除后文件仍有残留: %v", matches)
	}
	monthIndexes, _ := filepath.Glob(filepath.Join(f.mem.UserRoot(u1), "*", "index.txt"))
	for _, path := range monthIndexes {
		content, _ := os.ReadFile(path)
		if strings.Contains(string(content), fmt.Sprintf("Session #%d：", c1)) {
			t.Fatalf("月份索引仍有残留: %s", path)
		}
	}
}

func TestArchiveIncrementEditDisableAndMissingFile(t *testing.T) {
	f := newMemoryFixture(t)
	userID, convID := f.userWithConversation(t, "increment", "增量归档")
	f.addExchange(t, convID, "第一问", "第一答")
	_, _ = f.mem.SetSessionEnabled(userID, convID, true)
	if _, err := f.mem.RunMaintenance(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	f.addExchange(t, convID, "第二问", "第二答")
	report, err := f.mem.RunMaintenance(context.Background(), userID)
	if err != nil || report.Updated != 1 {
		t.Fatalf("增量归档失败 %+v, %v", report, err)
	}
	requests := f.up.captured()
	if !strings.Contains(requests[len(requests)-1].Prompt, "已有归档") || !strings.Contains(requests[len(requests)-1].Prompt, "第二问") {
		t.Fatal("第二次没有走滚动增量归档")
	}

	msgs, _ := f.st.ListMessages(convID)
	if _, err := f.st.UpdateMessageContent(msgs[0].ID, "编辑后的第一问"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mem.RunMaintenance(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	requests = f.up.captured()
	if strings.Contains(requests[len(requests)-1].Prompt, "已有归档") || !strings.Contains(requests[len(requests)-1].Prompt, "编辑后的第一问") {
		t.Fatal("编辑历史后没有全量重建")
	}

	_, _ = f.mem.SetSessionEnabled(userID, convID, false)
	f.addExchange(t, convID, "关闭后的内容", "不应归档")
	before := len(f.up.captured())
	report, err = f.mem.RunMaintenance(context.Background(), userID)
	if err != nil || report.Skipped == 0 || len(f.up.captured()) != before {
		t.Fatalf("关闭后仍调用了归档: %+v", report)
	}
	status, _ := f.mem.SessionStatus(userID, convID)
	if status.ArchivedBefore == 0 {
		t.Fatal("关闭后已有归档被清掉")
	}

	u, _ := f.mem.user(userID)
	r, _ := u.session(convID)
	path, _ := u.resolveArchive(r.ArchivePath)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, _ = f.mem.SetSessionEnabled(userID, convID, true)
	if _, err := f.mem.RunMaintenance(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("缺失归档没有被修复: %v", err)
	}
}

func TestInvalidOutputPreservesOldArchive(t *testing.T) {
	f := newMemoryFixture(t)
	userID, convID := f.userWithConversation(t, "invalid", "无效输出")
	f.addExchange(t, convID, "旧内容", "旧回答")
	_, _ = f.mem.SetSessionEnabled(userID, convID, true)
	_, _ = f.mem.RunMaintenance(context.Background(), userID)
	u, _ := f.mem.user(userID)
	r, _ := u.session(convID)
	path, _ := u.resolveArchive(r.ArchivePath)
	before, _ := os.ReadFile(path)

	f.up.mu.Lock()
	f.up.invalid = true
	f.up.mu.Unlock()
	f.addExchange(t, convID, "新内容", "新回答")
	report, err := f.mem.RunMaintenance(context.Background(), userID)
	if err != nil || report.Failed != 1 {
		t.Fatalf("无效输出没有记为失败: %+v %v", report, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("无效模型输出覆盖了旧归档")
	}
	status, _ := f.mem.SessionStatus(userID, convID)
	if status.LastError == "" || !status.Pending {
		t.Fatalf("失败状态没有保留待重试信息: %+v", status)
	}
}

func TestMainDatabaseUntouchedAndSchedule(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	st, err := store.Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeVersion := schemaVersionForTest(t, mainPath)
	beforeTables := tableCountForTest(t, mainPath)
	mem, err := NewManager(st, filepath.Join(dir, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	user, _ := st.CreateUser("sidecar", store.HashToken("sidecar"), false)
	conv, _ := st.CreateConversation(user.ID, "sidecar", 0, "", "")
	if _, err := mem.SetSessionEnabled(user.ID, conv.ID, true); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "memory", "users", "999")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	mem.RunAll(context.Background(), 0)
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("已删除用户的记忆目录没有被全量维护清理")
	}
	_ = mem.Close()
	_ = st.Close()
	if got := schemaVersionForTest(t, mainPath); got != beforeVersion {
		t.Fatalf("主库 schema 版本改变: %d -> %d", beforeVersion, got)
	}
	if got := tableCountForTest(t, mainPath); got != beforeTables {
		t.Fatalf("主库表数量改变: %d -> %d", beforeTables, got)
	}

	loc := archiveLocation
	beforeFour := time.Date(2026, 11, 4, 3, 59, 0, 0, loc)
	afterFour := time.Date(2026, 11, 4, 4, 1, 0, 0, loc)
	if got := LatestArchiveTime(beforeFour); got.Day() != 3 || got.Hour() != 4 {
		t.Fatalf("04:00 前的计划时间错误: %v", got)
	}
	if got := LatestArchiveTime(afterFour); got.Day() != 4 || got.Hour() != 4 {
		t.Fatalf("04:00 后的计划时间错误: %v", got)
	}
}

func schemaVersionForTest(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func tableCountForTest(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
