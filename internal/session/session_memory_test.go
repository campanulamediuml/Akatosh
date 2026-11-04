package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	memorypkg "aiassistant/internal/memory"
	"aiassistant/internal/store"
)

func TestMemoryIndexAndReadToolEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var streamRequests []string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(data, &body)
		if !body.Stream {
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
				"message": map[string]any{"role": "assistant", "content": `{"detail":"旧会话的端口号是 18083","index":{"goal":"记录部署端口","topics":["部署"],"entities":["18083"],"decisions":["NAT 使用 18083"],"lookup":["对外端口"],"aliases":["port"],"status":"完成"}}`},
			}}})
			return
		}
		hasToolResult := false
		for _, msg := range body.Messages {
			if msg.Role == "tool" {
				hasToolResult = true
			}
		}
		mu.Lock()
		streamRequests = append(streamRequests, string(data))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if !hasToolResult {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_memory\",\"type\":\"function\",\"function\":{\"name\":\"memory_read_session\",\"arguments\":\"{\\\"session_id\\\":1}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"旧会话使用 18083 端口\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	up := httptest.NewServer(mux)
	defer up.Close()
	mgr, st := newTestManager(t, up.URL)
	mem, err := memorypkg.NewManager(st, filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	mgr.WithMemory(mem)

	archived, _ := st.CreateConversation(1, "旧部署讨论", 0, "m-reason", "high")
	if archived.ID != 1 {
		t.Fatalf("测试工具参数要求旧会话 id=1，实际 %d", archived.ID)
	}
	_, _ = st.CreateMessage(store.Message{ConvID: archived.ID, Role: store.RoleUser, Status: store.StatusDone, Content: "对外端口是多少"})
	_, _ = st.CreateMessage(store.Message{ConvID: archived.ID, Role: store.RoleAssistant, Status: store.StatusDone, Content: "18083"})
	if _, err := mem.SetSessionEnabled(1, archived.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.RunMaintenance(context.Background(), 1); err != nil {
		t.Fatal(err)
	}

	current, _ := st.CreateConversation(1, "当前问题", 0, "m-reason", "high")
	if _, err := mem.SetSessionEnabled(1, current.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.SendMessage(context.Background(), 1, current.ID, "之前部署的端口是多少", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	fin := waitIdle(t, mgr, current.ID)
	if len(fin.Messages) != 2 || fin.Messages[1].Content != "旧会话使用 18083 端口" {
		t.Fatalf("记忆工具最终回答异常: %+v", fin.Messages)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(streamRequests) != 2 {
		t.Fatalf("工具循环请求次数=%d，期望 2", len(streamRequests))
	}
	if !strings.Contains(streamRequests[0], "memory_read_session") || !strings.Contains(streamRequests[0], "旧部署讨论") {
		t.Fatal("首轮请求没有包含记忆工具或用户索引")
	}
	if !strings.Contains(streamRequests[1], "旧会话的端口号是 18083") {
		t.Fatal("第二轮请求没有回传归档正文")
	}
}

func TestMemoryDisabledOmitsIndexAndTool(t *testing.T) {
	up := newUpstream(t, 0)
	defer up.Close()
	mgr, st := newTestManager(t, up.URL)
	mem, err := memorypkg.NewManager(st, filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	mgr.WithMemory(mem)
	conv, _ := st.CreateConversation(1, "无记忆", 0, "m-reason", "high")
	s := newSession(conv.ID, mgr, conv)
	if tools := s.toolDefs(); len(tools) != 0 {
		t.Fatalf("未开启记忆时不应声明工具: %+v", tools)
	}
	history := s.buildHistory(conv.ModelID)
	for _, msg := range history {
		if strings.Contains(fmt.Sprintf("%v", msg.Content), "长期记忆索引") {
			t.Fatal("未开启记忆时注入了索引")
		}
	}
}
