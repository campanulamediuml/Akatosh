package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aiassistant/internal/store"
)

func TestSanitizeTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Go 并发模型速览", "Go 并发模型速览"},
		{"「Go 并发模型速览」", "Go 并发模型速览"},
		{"“Go 并发模型速览”", "Go 并发模型速览"},
		{"《Go 并发模型速览》", "Go 并发模型速览"},
		{"标题：Go 并发模型", "Go 并发模型"},
		{"标题: Go 并发模型", "Go 并发模型"},
		{"Title: Go Concurrency", "Go Concurrency"},
		{"# Go 并发模型", "Go 并发模型"},
		// 模型爱先来一句解释，取第一个句号前的部分
		{"Go 的并发模型入门。理由：用户问了 goroutine", "Go 的并发模型入门"},
		// 只取第一行
		{"Go 并发模型\n\n理由：因为用户问了……", "Go 并发模型"},
		{"  Go 并发模型  ", "Go 并发模型"},
		// 超长要截断
		{strings.Repeat("很", 40), strings.Repeat("很", 20)},
		// 空 / 只有引号 → 空串，调用方会保留本地标题
		{"", ""},
		{"「」", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := sanitizeTitle(c.in); got != c.want {
			t.Errorf("sanitizeTitle(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// titleUpstream 是一个假上游：流式请求返回正文，非流式请求返回一个标题。
func titleUpstream(t *testing.T, title string, titleDelay time.Duration) (*httptest.Server, *int32) {
	t.Helper()
	var titleCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = decodeBody(r, &body)

		if !body.Stream { // 起标题走这条
			atomic.AddInt32(&titleCalls, 1)
			if titleDelay > 0 {
				time.Sleep(titleDelay)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{
					"message": map[string]any{"role": "assistant", "content": title},
				}},
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"好的\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	return httptest.NewServer(mux), &titleCalls
}

func waitTitle(t *testing.T, st *store.Store, convID int64, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		c, err := st.GetConversation(convID)
		if err == nil && c != nil {
			last = c.Title
			if last == want {
				return last
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last
}

// TestTitleGeneratedByModel 验证：先秒出本地截断标题，随后被模型起的好名字覆盖。
func TestTitleGeneratedByModel(t *testing.T) {
	up, calls := titleUpstream(t, "「Go 并发模型速览」", 0)
	defer up.Close()
	mgr, st := newTestManager(t, up.URL)

	conv, err := st.CreateConversation("新对话", 0, "m-reason", "high")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := mgr.SendMessage(ctx, conv.ID, "请详细讲讲 Go 的并发模型，越详细越好", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	fin := waitIdle(t, mgr, conv.ID)

	// 本地截断标题应当已经出现（秒出，不依赖上游）
	if len(fin.Conv.Title) == 0 || fin.Conv.Title == "新对话" {
		t.Fatalf("本地截断标题没有生效: %q", fin.Conv.Title)
	}
	t.Logf("本地标题: %q", fin.Conv.Title)

	// 随后被模型标题覆盖（引号要被清掉）
	got := waitTitle(t, st, conv.ID, "Go 并发模型速览", 5*time.Second)
	if got != "Go 并发模型速览" {
		t.Fatalf("模型标题没有生效，当前标题 %q", got)
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("起标题请求应当只发一次，实际 %d 次", n)
	}

	// 关键：起标题是「隐式」的，绝不能往会话里写消息
	msgs, err := st.ListMessages(conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("起标题不该往对话里插消息，期望 2 条（一问一答），实际 %d 条", len(msgs))
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "起一个标题") || strings.Contains(m.Content, "标题：") {
			head := m.Content
			if len(head) > 60 {
				head = head[:60]
			}
			t.Fatalf("起标题的提示词泄漏进聊天记录了: %q", head)
		}
	}
}

// TestTitleNotOverwrittenAfterRename 验证用户手动改名后不会被覆盖。
func TestTitleNotOverwrittenAfterRename(t *testing.T) {
	up, calls := titleUpstream(t, "模型起的名字", 400*time.Millisecond)
	defer up.Close()
	mgr, st := newTestManager(t, up.URL)

	conv, _ := st.CreateConversation("新对话", 0, "m-reason", "high")
	ctx := context.Background()
	if _, _, err := mgr.SendMessage(ctx, conv.ID, "随便聊聊", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, mgr, conv.ID)

	// 请求还在飞的时候用户手动改名
	mine := "我自己起的名字"
	if _, err := mgr.PatchConversation(ctx, conv.ID, store.ConversationPatch{Title: &mine}); err != nil {
		t.Fatal(err)
	}

	// 等起标题请求确实发生过，再确认标题没被改掉
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(calls) == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(800 * time.Millisecond)

	c, _ := st.GetConversation(conv.ID)
	if c.Title != mine {
		t.Fatalf("用户改的名字被覆盖了: %q", c.Title)
	}
}

// TestTitleFallsBackWhenUpstreamFails 验证起标题失败时保留本地标题，不会变成空白。
func TestTitleFallsBackWhenUpstreamFails(t *testing.T) {
	var titleCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = decodeBody(r, &body)
		if !body.Stream {
			atomic.AddInt32(&titleCalls, 1)
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"boom"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"好的\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	up := httptest.NewServer(mux)
	defer up.Close()

	mgr, st := newTestManager(t, up.URL)
	conv, _ := st.CreateConversation("新对话", 0, "m-reason", "high")
	ctx := context.Background()
	if _, _, err := mgr.SendMessage(ctx, conv.ID, "帮我看看这段代码有没有并发问题", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	fin := waitIdle(t, mgr, conv.ID)
	localTitle := fin.Conv.Title

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&titleCalls) == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(600 * time.Millisecond)

	c, _ := st.GetConversation(conv.ID)
	if c.Title != localTitle {
		t.Fatalf("上游失败后标题被改动了: %q → %q", localTitle, c.Title)
	}
	if strings.TrimSpace(c.Title) == "" || c.Title == "新对话" {
		t.Fatalf("标题不该是空的或默认值: %q", c.Title)
	}
}

// TestTitleGeneratesOnlyOnce 验证后续轮次不会再发一次起标题请求。
func TestTitleGeneratesOnlyOnce(t *testing.T) {
	up, calls := titleUpstream(t, "第一次就定下来的名字", 0)
	defer up.Close()
	mgr, st := newTestManager(t, up.URL)

	conv, _ := st.CreateConversation("新对话", 0, "m-reason", "high")
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, _, err := mgr.SendMessage(ctx, conv.ID, fmt.Sprintf("第 %d 轮提问", i+1), 0, "", nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, mgr, conv.ID)
	}
	waitTitle(t, st, conv.ID, "第一次就定下来的名字", 5*time.Second)
	time.Sleep(500 * time.Millisecond)

	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("起标题请求应当只发一次，实际 %d 次", n)
	}
}
