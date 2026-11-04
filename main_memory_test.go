package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aiassistant/internal/memory"
	"aiassistant/internal/store"
)

func TestMemoryMaintenanceStandaloneProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("独立进程集成测试")
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": `{"detail":"独立进程生成的归档","index":{"goal":"验证独立维护","topics":["进程"],"decisions":["无需启动 HTTP"],"status":"完成"}}`},
		}}})
	}))
	defer up.Close()

	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	miscDir := filepath.Join(dir, "misc")
	st, err := store.Open(filepath.Join(dataDir, "ai-assistant.db"))
	if err != nil {
		t.Fatal(err)
	}
	user, _ := st.CreateUser("standalone", store.HashToken("standalone"), false)
	provider, _ := st.CreateProvider(user.ID, "fake", up.URL, "")
	_, _ = st.UpsertModels(provider.ID, []store.Model{{ModelID: "archive-model", Reasoning: true, Efforts: []string{"low"}}})
	conv, _ := st.CreateConversation(user.ID, "独立维护", provider.ID, "archive-model", "low")
	_, _ = st.CreateMessage(store.Message{ConvID: conv.ID, Role: store.RoleUser, Status: store.StatusDone, Content: "测试独立进程"})
	_, _ = st.CreateMessage(store.Message{ConvID: conv.ID, Role: store.RoleAssistant, Status: store.StatusDone, Content: "后台归档"})
	mem, err := memory.NewManager(st, filepath.Join(miscDir, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.SetSessionEnabled(user.ID, conv.ID, true); err != nil {
		t.Fatal(err)
	}
	_ = mem.Close()
	_ = st.Close()

	binary := filepath.Join(dir, "ai-assistant-test")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建测试二进制失败: %v\n%s", err, output)
	}
	cmd := exec.Command(binary, "-data", dataDir, "-misc", miscDir, "-memory-maintenance-now", "-memory-user", "1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("独立维护进程失败: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "独立维护报告") {
		t.Fatalf("缺少维护报告:\n%s", output)
	}
	matches, _ := filepath.Glob(filepath.Join(miscDir, "memory", "users", "1", "*", "session-1.txt"))
	if len(matches) != 1 {
		t.Fatalf("独立进程没有生成归档: %v", matches)
	}
	content, _ := os.ReadFile(matches[0])
	if !strings.Contains(string(content), "独立进程生成的归档") {
		t.Fatalf("归档内容异常:\n%s", content)
	}

	check, err := memory.NewManager(mustOpenStore(t, filepath.Join(dataDir, "ai-assistant.db")), filepath.Join(miscDir, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	status, err := check.MaintenanceStatus(1)
	if err != nil || status.LastSuccessfulAt == 0 {
		t.Fatalf("维护状态未持久化: %+v, %v", status, err)
	}
}

func mustOpenStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
