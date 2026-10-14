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

	"aiassistant/internal/sandbox"
)

// pythonUpstream 是一个会「要求跑代码」的假上游：
// 带 tools 且无 tool 消息 → 返回 run_python 调用；有 tool 消息 → 返回正文。
func pythonUpstream(t *testing.T, gotTools *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools    []any `json:"tools"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		_ = decodeBody(r, &body)
		if len(body.Tools) > 0 {
			gotTools.Add(1)
		}
		hasToolMsg := false
		for _, m := range body.Messages {
			if m.Role == "tool" {
				hasToolMsg = true
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		if len(body.Tools) > 0 && !hasToolMsg {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_py\",\"type\":\"function\",\"function\":{\"name\":\"run_python\",\"arguments\":\"{\\\"code\\\": \\\"print(6*7)\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			if f != nil {
				f.Flush()
			}
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"答案是 42。\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f != nil {
			f.Flush()
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestPythonToolLoop 验证「模型要求跑代码 → 沙箱执行 → 结果回传 → 续写正文」循环。
func TestPythonToolLoop(t *testing.T) {
	gotTools := &atomic.Int32{}
	up := pythonUpstream(t, gotTools)
	mgr, st := newTestManager(t, up.URL)

	var gotCode []string
	pybox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Code string `json:"code"`
		}
		_ = decodeBody(r, &body)
		gotCode = append(gotCode, body.Code)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"stdout":"42\n","stderr":"","exit_code":0,"timed_out":false,"duration_ms":31}`)
	}))
	t.Cleanup(pybox.Close)
	mgr.WithSandbox(sandbox.New(pybox.URL))

	conv, err := st.CreateConversation("python 测试", 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Python 工具没有会话级开关：配了沙箱就全局可用，模型自主决定调不调

	if _, _, err := mgr.SendMessage(context.Background(), conv.ID, "6 乘以 7 是多少？用代码算", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	state := waitIdle(t, mgr, conv.ID)

	if len(gotCode) != 1 || gotCode[0] != "print(6*7)" {
		t.Fatalf("沙箱收到的代码不对: %v", gotCode)
	}
	if gotTools.Load() != 2 {
		t.Fatalf("上游应收两发带 tools 的请求，实际 %d", gotTools.Load())
	}

	last := state.Messages[len(state.Messages)-1]
	if !strings.Contains(last.Content, "答案是 42") {
		t.Fatalf("正文不对: %q", last.Content)
	}
	// search_log 里的 python 记录：type + code + stdout
	var log []map[string]any
	if err := json.Unmarshal([]byte(last.SearchLog), &log); err != nil || len(log) != 1 {
		t.Fatalf("search_log 不对: %q", last.SearchLog)
	}
	if log[0]["type"] != "python" || log[0]["code"] != "print(6*7)" || log[0]["stdout"] != "42\n" {
		t.Fatalf("python 记录内容不对: %+v", log[0])
	}
}

// TestPythonUnavailableWithoutSandbox 验证没配沙箱时不声明 run_python。
func TestPythonUnavailableWithoutSandbox(t *testing.T) {
	gotTools := &atomic.Int32{}
	up := pythonUpstream(t, gotTools)
	mgr, st := newTestManager(t, up.URL) // 不注入 sandbox

	conv, err := st.CreateConversation("普通会话", 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.SendMessage(context.Background(), conv.ID, "你好", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, mgr, conv.ID)

	if gotTools.Load() != 0 {
		t.Fatalf("没配沙箱不该带 tools，实际带了 %d 次", gotTools.Load())
	}
}
