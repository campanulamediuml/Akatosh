package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// sseServer 按给定的 payload 行逐条写出 SSE 流（自动补 [DONE]）。
func sseServer(t *testing.T, lines []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, l := range lines {
			fmt.Fprintf(w, "data: %s\n\n", l)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// collect 把事件流读完。
func collect(ch <-chan Event) []Event {
	out := []Event{}
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// TestToolCallsFragmentAccumulation 验证流式 tool_calls 的分片归并：
// id/name 只在首片，arguments 逐片增量，最终要拼成完整的一次调用。
func TestToolCallsFragmentAccumulation(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"web_search","arguments":"{\"q"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"uery\":\"今天"}},{"index":0,"function":{"arguments":"天气\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
	})
	cli := New(srv.URL+"/v1", "k")
	ch, dropped, err := cli.Stream(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []ChatMessage{{Role: "user", Content: "x"}},
		Tools:    []Tool{toolForTest()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dropped {
		t.Fatal("不该标记 toolsDropped")
	}
	events := collect(ch)

	var tc *Event
	var sawDone, sawUsage bool
	for i := range events {
		switch events[i].Type {
		case EventToolCalls:
			tc = &events[i]
		case EventDone:
			sawDone = true
		case EventUsage:
			sawUsage = true
		}
	}
	if tc == nil {
		t.Fatalf("没收到 EventToolCalls: %+v", events)
	}
	if sawDone {
		t.Fatal("tool_calls 收尾时不该再发 EventDone")
	}
	if !sawUsage {
		t.Fatal("usage 事件丢了")
	}
	if len(tc.ToolCalls) != 1 {
		t.Fatalf("应归并成 1 次调用，实际 %d", len(tc.ToolCalls))
	}
	call := tc.ToolCalls[0]
	if call.ID != "call_1" || call.Function.Name != "web_search" {
		t.Fatalf("调用头不对: %+v", call)
	}
	if call.Function.Arguments != `{"query":"今天天气"}` {
		t.Fatalf("arguments 拼接不对: %q", call.Function.Arguments)
	}
	if tc.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason 不对: %q", tc.FinishReason)
	}
}

// TestToolCallsViaFullMessage 验证「不支持 stream」的网关把 tool_calls 塞在
// 完整 message 里返回时，也能归并出来。
func TestToolCallsViaFullMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"id":"call_9","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	t.Cleanup(srv.Close)
	cli := New(srv.URL+"/v1", "k")
	ch, _, err := cli.Stream(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []ChatMessage{{Role: "user", Content: "x"}},
		Tools:    []Tool{toolForTest()},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(ch)
	var tc *Event
	for i := range events {
		if events[i].Type == EventToolCalls {
			tc = &events[i]
		}
	}
	if tc == nil || len(tc.ToolCalls) != 1 || tc.ToolCalls[0].ID != "call_9" {
		t.Fatalf("message 全量分支没解析出 tool_calls: %+v", events)
	}
}

// TestToolsDroppedOn400 验证上游 400 且错误体含 tools 时：
// 自动去掉工具声明重试，并标记 toolsDropped。
func TestToolsDroppedOn400(t *testing.T) {
	var first, retried atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, hasTools := body["tools"]; hasTools {
			first.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"unknown field: tools"}}`)
			return
		}
		retried.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"好\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	cli := New(srv.URL+"/v1", "k")
	ch, dropped, err := cli.Stream(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []ChatMessage{{Role: "user", Content: "x"}},
		Tools:    []Tool{toolForTest()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !dropped {
		t.Fatal("应标记 toolsDropped")
	}
	if first.Load() != 1 || retried.Load() != 1 {
		t.Fatalf("重试次数不对: first=%d retried=%d", first.Load(), retried.Load())
	}
	events := collect(ch)
	last := events[len(events)-1]
	if last.Type != EventDone {
		t.Fatalf("重试后应正常完成: %+v", events)
	}
}

// TestChatBodyWithTools 验证请求体里的 tools / tool_choice 序列化。
func TestChatBodyWithTools(t *testing.T) {
	body := buildChatBody(ChatRequest{
		Model:    "m",
		Messages: []ChatMessage{{Role: "user", Content: "x"}},
		Tools:    []Tool{toolForTest()},
	}, true, true, true)
	if body["tool_choice"] != "auto" {
		t.Fatalf("tool_choice 不对: %v", body["tool_choice"])
	}
	tools, ok := body["tools"].([]Tool)
	if !ok || len(tools) != 1 || tools[0].Function.Name != "web_search" {
		t.Fatalf("tools 不对: %v", body["tools"])
	}
	// 不带 tools 时不该出现这两个键（老网关对未知字段会报错）
	body2 := buildChatBody(ChatRequest{Model: "m"}, true, true, true)
	if _, ok := body2["tools"]; ok {
		t.Fatal("Tools 为空时不该序列化 tools 键")
	}
	if _, ok := body2["tool_choice"]; ok {
		t.Fatal("Tools 为空时不该序列化 tool_choice 键")
	}
}

// toolForTest 造一个最小工具声明。
func toolForTest() Tool {
	var tool Tool
	tool.Type = "function"
	tool.Function.Name = "web_search"
	tool.Function.Description = "搜索"
	tool.Function.Parameters = map[string]any{"type": "object"}
	return tool
}

// TestNoToolCallsMeansDone 验证普通流（无 tool_calls）行为与旧版一致：
// finish_reason=stop 时发 EventDone。
func TestNoToolCallsMeansDone(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"content":"正文"},"finish_reason":"stop"}]}`,
	})
	cli := New(srv.URL+"/v1", "k")
	ch, _, err := cli.Stream(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []ChatMessage{{Role: "user", Content: "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(ch)
	var sawContent, sawDone, sawTC bool
	for _, e := range events {
		if e.Type == EventContent && strings.Contains(e.Delta, "正文") {
			sawContent = true
		}
		if e.Type == EventDone {
			sawDone = true
		}
		if e.Type == EventToolCalls {
			sawTC = true
		}
	}
	if !sawContent || !sawDone || sawTC {
		t.Fatalf("普通流事件不对: %+v", events)
	}
}

// TestInferContextLen 推断表的保守性：确定把握的系列给值，拿不准的返回 0。
func TestInferContextLen(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"deepseek-v4.1-flash", 1048576}, // 用户的主力：第三方网关不报，靠这里
		{"deepseek-v4-pro", 1048576},
		{"deepseek-flash", 1048576},
		{"deepseek-chat", 65536}, // V3 时代
		{"deepseek-reasoner", 65536},
		{"kimi-k3", 1048576},
		{"kimi-k2.7-code", 262144},
		{"gpt-5.6-sol", 1050000}, // 5.4 起 API 上限 1.05M
		{"gpt-5.5", 1050000},
		{"gpt-5", 400000},        // 初版 GPT-5
		{"gpt-6-astra", 1050000}, // GPT-6 家族延续 1.05M
		{"gpt-6.1-sol", 1050000},
		{"gpt-4.1", 1048576}, // 4.1 才是 1M
		{"gpt-4o", 128000},
		{"claude-sonnet-4.5", 200000},
		{"some-future-model", 0}, // 不认识的一律 0，不瞎猜
		{"", 0},
	}
	for _, c := range cases {
		if got := InferContextLen(c.model); got != c.want {
			t.Errorf("InferContextLen(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}
