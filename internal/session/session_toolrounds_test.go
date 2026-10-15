package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"aiassistant/internal/sandbox"
	"aiassistant/internal/store"
)

// TestToolRoundsRecordedAndReplayed 验证 v11 的核心行为：
// 第一轮带工具的生成把完整工具轮落库（tool_rounds），
// 下一轮请求的历史里包含 role=tool 的**完整结果**（不是文本摘要）。
func TestToolRoundsRecordedAndReplayed(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools    []any `json:"tools"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		raw := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(raw)
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()

		hasToolMsg := false
		for _, m := range body.Messages {
			if m.Role == "tool" {
				hasToolMsg = true
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if len(body.Tools) > 0 && !hasToolMsg {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"先算一下\",\"tool_calls\":[{\"index\":0,\"id\":\"call_py\",\"type\":\"function\",\"function\":{\"name\":\"run_python\",\"arguments\":\"{\\\"code\\\": \\\"print(6*7)\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"结果有了\",\"content\":\"答案是 42。\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	up := httptest.NewServer(mux)
	t.Cleanup(up.Close)

	mgr, st := newTestManager(t, up.URL)
	pybox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"stdout":"42\n","stderr":"","exit_code":0,"timed_out":false,"duration_ms":31}`)
	}))
	t.Cleanup(pybox.Close)
	mgr.WithSandbox(sandbox.New(pybox.URL))

	conv, err := st.CreateConversation(1, "回放测试", 0, "", "")
	if err != nil {
		t.Fatal(err)
	}

	// 第一轮：触发工具调用
	if _, _, err := mgr.SendMessage(context.Background(), 1, conv.ID, "6 乘以 7？用代码算", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	state := waitIdle(t, mgr, conv.ID)
	last := state.Messages[len(state.Messages)-1]

	// 落库：tool_rounds 完整记录了 assistant(tool_calls) + tool 结果
	stored, err := st.GetMessage(last.ID)
	if err != nil || stored == nil {
		t.Fatal("读消息失败")
	}
	if stored.ToolRounds == "" {
		t.Fatal("tool_rounds 应已落库")
	}
	var p store.ToolRoundsPayload
	if err := json.Unmarshal([]byte(stored.ToolRounds), &p); err != nil {
		t.Fatalf("tool_rounds 不是合法 JSON: %v", err)
	}
	if len(p.Rounds) != 2 || len(p.Rounds[0].ToolCalls) != 1 || p.Rounds[1].Role != "tool" {
		t.Fatalf("tool_rounds 结构不对: %+v", p.Rounds)
	}
	if p.Rounds[0].Reasoning != "先算一下" {
		t.Fatalf("中间段的 reasoning 应按段记录: %q", p.Rounds[0].Reasoning)
	}
	s, _ := p.Rounds[1].Content.(string)
	if !strings.Contains(s, "42") {
		t.Fatalf("tool 结果应含完整输出: %q", s)
	}

	// 第二轮：历史里应出现**完整 tool 结果**（全量回放），而不是文本摘要
	if _, _, err := mgr.SendMessage(context.Background(), 1, conv.ID, "那 8 乘 8 呢？也用代码", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, mgr, conv.ID)

	mu.Lock()
	defer mu.Unlock()
	var second string
	for _, b := range bodies {
		if strings.Contains(b, "8 乘 8") {
			second = b
		}
	}
	if second == "" {
		t.Fatal("没找到第二轮的请求")
	}
	// 全量回放：role=tool 的完整结果在历史里（FormatToolResult 的 JSON 含 stdout）
	if !strings.Contains(second, `"role":"tool"`) || !strings.Contains(second, "stdout") || !strings.Contains(second, "exit_code") {
		t.Fatalf("第二轮历史应包含完整 tool 结果: %s", second[:1200])
	}
	// 有完整回放的老路标（文本摘要）不应再出现
	if strings.Contains(second, "[上一轮的工具调用") {
		t.Fatalf("有完整回放的消息不应再注入文本摘要")
	}
}
