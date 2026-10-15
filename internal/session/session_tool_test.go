package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aiassistant/internal/search"
	"aiassistant/internal/store"
)

// toolUpstream 是一个会「要求搜索」的假上游：
//   - 请求带 tools 且历史里没有 role=tool 消息 → 返回 web_search 的流式分片（故意拆多片）
//   - 历史里已有 role=tool 消息（或请求不带 tools）→ 返回正常正文流
//
// calls 记录每次请求的关键信息，供断言。
type toolUpstream struct {
	srv *httptest.Server
	// 每次请求：有没有带 tools、messages 里有没有 role=tool
	withTools   *atomic.Int32
	withToolMsg *atomic.Int32
	total       *atomic.Int32
}

func newToolUpstream(t *testing.T) *toolUpstream {
	t.Helper()
	u := &toolUpstream{withTools: &atomic.Int32{}, withToolMsg: &atomic.Int32{}, total: &atomic.Int32{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools    []any `json:"tools"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := decodeBody(r, &body); err != nil {
			t.Errorf("解析请求体失败: %v", err)
		}
		u.total.Add(1)
		hasToolMsg := false
		for _, m := range body.Messages {
			if m.Role == "tool" {
				hasToolMsg = true
			}
		}
		if len(body.Tools) > 0 {
			u.withTools.Add(1)
		}
		if hasToolMsg {
			u.withToolMsg.Add(1)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		flush := func() {
			if f != nil {
				f.Flush()
			}
		}

		if len(body.Tools) > 0 && !hasToolMsg {
			// 第一段流：要求搜索。arguments 故意拆成 3 片，id/name 只在首片，
			// 后两片连 index 都带上（DeepSeek 就是这个形态）。
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_abc\",\"type\":\"function\",\"function\":{\"name\":\"web_search\",\"arguments\":\"{\\\"q\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"uery\\\":\\\"今\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"天新闻\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
			fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":10,\"total_tokens\":110}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			flush()
			return
		}
		// 第二段流（或本来就无 tools）：基于搜索结果写正文
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"根据搜索，今天的新闻是……\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":200,\"completion_tokens\":20,\"total_tokens\":220}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush()
	})
	u.srv = httptest.NewServer(mux)
	t.Cleanup(u.srv.Close)
	return u
}

// newSearx 起一个假 SearXNG，返回两条结果；queries 记录收到的 q。
func newSearx(t *testing.T, queries *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*queries = append(*queries, r.URL.Query().Get("q"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[
			{"title":"今日头条 A","url":"https://a.example.com","content":"A 的内容摘要"},
			{"title":"今日头条 B","url":"https://b.example.com","content":"B 的内容摘要"}
		]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func boolPtr(b bool) *bool { return &b }

// TestWebSearchToolLoop 验证完整的「模型要求搜索 → 后端执行 → 续传正文」循环。
func TestWebSearchToolLoop(t *testing.T) {
	up := newToolUpstream(t)
	mgr, st := newTestManager(t, up.srv.URL)
	queries := []string{}
	mgr.WithSearch(search.New(newSearx(t, &queries).URL))

	conv, err := st.CreateConversation(1, "联网测试", 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateConversation(conv.ID, store.ConversationPatch{WebSearch: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := mgr.SendMessage(context.Background(), 1, conv.ID, "今天有什么新闻", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	state := waitIdle(t, mgr, conv.ID)

	// 只搜了一次，query 是分片累积拼出来的
	if len(queries) != 1 || queries[0] != "今天新闻" {
		t.Fatalf("搜索次数/关键词不对: %v", queries)
	}
	// 上游收到两发请求：第一发带 tools 无 tool 消息，第二发带 tools 有 tool 消息
	if up.total.Load() != 2 || up.withTools.Load() != 2 || up.withToolMsg.Load() != 1 {
		t.Fatalf("上游请求形态不对: total=%d withTools=%d withToolMsg=%d",
			up.total.Load(), up.withTools.Load(), up.withToolMsg.Load())
	}

	last := state.Messages[len(state.Messages)-1]
	if last.Role != store.RoleAssistant || last.Status != store.StatusDone {
		t.Fatalf("最终消息状态不对: role=%s status=%s", last.Role, last.Status)
	}
	if !strings.Contains(last.Content, "根据搜索") {
		t.Fatalf("正文不对: %q", last.Content)
	}
	// usage 是两段流的累加（100+200 / 10+20 / 110+220）
	if last.Usage.PromptTokens != 300 || last.Usage.CompletionTokens != 30 || last.Usage.TotalTokens != 330 {
		t.Fatalf("usage 没有累加: %+v", last.Usage)
	}
	// search_log 落库：query + 两条来源
	var log []search.Record
	if err := json.Unmarshal([]byte(last.SearchLog), &log); err != nil || len(log) != 1 {
		t.Fatalf("search_log 不对: %q err=%v", last.SearchLog, err)
	}
	if log[0].Query != "今天新闻" || len(log[0].Results) != 2 || log[0].Results[0].URL != "https://a.example.com" {
		t.Fatalf("search_log 内容不对: %+v", log[0])
	}
}

// TestWebSearchOffByDefault 验证没开联网的会话请求里不带 tools（行为与旧版一致）。
func TestWebSearchOffByDefault(t *testing.T) {
	up := newToolUpstream(t)
	mgr, st := newTestManager(t, up.srv.URL)
	mgr.WithSearch(search.New(newSearx(t, &[]string{}).URL))

	conv, err := st.CreateConversation(1, "普通会话", 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.SendMessage(context.Background(), 1, conv.ID, "你好", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, mgr, conv.ID)

	if up.withTools.Load() != 0 {
		t.Fatalf("没开联网的会话不该带 tools，实际带了 %d 次", up.withTools.Load())
	}
}

// TestWebSearchUnavailable 验证没配搜索后端时，即使会话开了联网也不带 tools。
func TestWebSearchUnavailable(t *testing.T) {
	up := newToolUpstream(t)
	mgr, st := newTestManager(t, up.srv.URL) // 不注入 search

	conv, err := st.CreateConversation(1, "联网但无后端", 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateConversation(conv.ID, store.ConversationPatch{WebSearch: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.SendMessage(context.Background(), 1, conv.ID, "你好", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, mgr, conv.ID)

	if up.withTools.Load() != 0 {
		t.Fatalf("没配搜索后端不该带 tools，实际带了 %d 次", up.withTools.Load())
	}
}

// TestWebSearchToggleMidConversation 验证「先不开联网聊一轮，中途再打开开关」：
// 工具列表是每轮生成时现算的（beginGeneration 读会话当时的开关），所以中途
// 打开后下一轮请求就带 tools。走 mgr.PatchConversation（前端 PATCH 的同款路径），
// 连会话 actor 内存里的 s.conv 更新也一并覆盖。
func TestWebSearchToggleMidConversation(t *testing.T) {
	up := newToolUpstream(t)
	mgr, st := newTestManager(t, up.srv.URL)
	mgr.WithSearch(search.New(newSearx(t, &[]string{}).URL))

	conv, err := st.CreateConversation(1, "中途开联网", 0, "", "")
	if err != nil {
		t.Fatal(err)
	}

	// 第一轮：没开联网 → 上游请求不带 tools
	if _, _, err := mgr.SendMessage(context.Background(), 1, conv.ID, "第一轮", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, mgr, conv.ID)
	if up.withTools.Load() != 0 {
		t.Fatalf("第一轮不该带 tools，实际带了 %d 次", up.withTools.Load())
	}

	// 中途打开联网开关（前端点按钮走的就是这条 PATCH 路径）
	if _, err := mgr.PatchConversation(context.Background(), 1, conv.ID, store.ConversationPatch{WebSearch: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}

	// 第二轮：应带 tools，模型随即走「要求搜索 → 执行 → 续传」循环（两发都带 tools）
	if _, _, err := mgr.SendMessage(context.Background(), 1, conv.ID, "今天有什么新闻", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, mgr, conv.ID)
	if up.withTools.Load() != 2 {
		t.Fatalf("第二轮（搜索循环两发）应带 tools，实际 %d 次", up.withTools.Load())
	}
}

// alwaysSearchUpstream 永远要求搜索：用来验证熔断（最多 maxToolRounds 轮）。
// 不带 tools 的请求（上限后的续传）才返回正文。
// lastBody 非空时把每次请求的原文存进去（验证熔断后的收尾请求长什么样）。
func alwaysSearchUpstream(t *testing.T, searches *atomic.Int32, lastBody ...*atomic.Value) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools    []any `json:"tools"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		if len(lastBody) > 0 && lastBody[0] != nil {
			lastBody[0].Store(string(raw))
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
		if len(body.Tools) > 0 {
			// 只要还声明着工具，就继续要求搜（不管历史里有没有 tool 消息）
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_x\",\"type\":\"function\",\"function\":{\"name\":\"web_search\",\"arguments\":\"{\\\"query\\\":\\\"再来一次\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			if f != nil {
				f.Flush()
			}
			return
		}
		_ = hasToolMsg
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"搜了三轮，就这些了。\"}}]}\n\n")
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

// TestWebSearchRoundLimit 验证搜索轮数熔断：模型无休止要求搜索时，
// 最多执行 maxToolRounds 次，之后去 tools 强制收尾。
func TestWebSearchRoundLimit(t *testing.T) {
	var lastBody atomic.Value
	up := alwaysSearchUpstream(t, &atomic.Int32{}, &lastBody)
	mgr, st := newTestManager(t, up.URL)
	searxCount := 0
	searx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searxCount++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[{"title":"t","url":"https://x.example.com","content":"c"}]}`)
	}))
	t.Cleanup(searx.Close)
	mgr.WithSearch(search.New(searx.URL))

	conv, err := st.CreateConversation(1, "熔断测试", 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateConversation(conv.ID, store.ConversationPatch{WebSearch: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.SendMessage(context.Background(), 1, conv.ID, "搜个不停", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	state := waitIdle(t, mgr, conv.ID)

	if searxCount != maxToolRounds {
		t.Fatalf("搜索次数应是 %d（熔断），实际 %d", maxToolRounds, searxCount)
	}
	last := state.Messages[len(state.Messages)-1]
	if !strings.Contains(last.Content, "搜了三轮") {
		t.Fatalf("熔断后应拿到正文，实际: %q", last.Content)
	}
	var log []search.Record
	if err := json.Unmarshal([]byte(last.SearchLog), &log); err != nil || len(log) != maxToolRounds {
		t.Fatalf("search_log 应有 %d 条，实际: %q", maxToolRounds, last.SearchLog)
	}
	// 熔断后的收尾请求：不带 tools，且历史里有「工具用完了」的提示
	// （不说的话模型会把工具调用写成 DSML 文本标记吐进正文，实测抓到）
	final, _ := lastBody.Load().(string)
	if !strings.Contains(final, "工具调用次数已用完") {
		t.Fatalf("熔断后的请求应带「工具用完」提示: %.400s", final)
	}
	var finalBody struct {
		Tools []any `json:"tools"`
	}
	_ = json.Unmarshal([]byte(final), &finalBody)
	if len(finalBody.Tools) > 0 {
		t.Fatal("熔断后的请求不应再声明 tools")
	}
}

// TestStopDuringSearch 验证搜索进行中点「停止」能立刻收尾。
func TestStopDuringSearch(t *testing.T) {
	up := newToolUpstream(t)
	mgr, st := newTestManager(t, up.srv.URL)
	// 搜索后端故意慢：300ms 才返回
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[]}`)
	}))
	t.Cleanup(slow.Close)
	mgr.WithSearch(search.New(slow.URL))

	conv, err := st.CreateConversation(1, "停止测试", 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateConversation(conv.ID, store.ConversationPatch{WebSearch: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.SendMessage(context.Background(), 1, conv.ID, "今天有什么新闻", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	// 等第一段流（tool_calls）走完、进入搜索执行阶段
	time.Sleep(100 * time.Millisecond)
	stopped, err := mgr.Stop(context.Background(), 1, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	state := waitIdle(t, mgr, conv.ID)
	last := state.Messages[len(state.Messages)-1]
	// 停止即 aborted；慢搜索的迟到回执不该把它救活
	if !stopped || last.Status != store.StatusAborted {
		t.Fatalf("停止后状态不对: stopped=%v status=%s", stopped, last.Status)
	}
}
