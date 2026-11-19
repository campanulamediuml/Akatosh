package session

import (
	"context"
	"strings"
	"testing"

	"aiassistant/internal/search"
	"aiassistant/internal/store"
)

type stubSearchExecutor struct {
	title string
	query string
}

func (s *stubSearchExecutor) Search(_ context.Context, query string) ([]search.Result, error) {
	s.query = query
	return []search.Result{{Title: s.title, URL: "https://example.com", Content: "摘要"}}, nil
}

func TestManagedSearchProvidersAreIndependentTools(t *testing.T) {
	up := newUpstream(t, 0)
	defer up.Close()
	mgr, st := newTestManager(t, up.URL)
	tavily := &stubSearchExecutor{title: "Tavily 结果"}
	google := &stubSearchExecutor{title: "Google 结果"}
	mgr.WithSearchProvider(search.Provider{
		ToolName: search.TavilyToolName, DisplayName: "Tavily", Description: "常规搜索", Executor: tavily,
	})
	mgr.WithSearchProvider(search.Provider{
		ToolName: search.GoogleToolName, DisplayName: "Google", Description: "Google 搜索", Executor: google,
	})

	conv, err := st.CreateConversation(1, "双搜索", 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	conv, err = st.UpdateConversation(conv.ID, store.ConversationPatch{WebSearch: boolPtr(true)})
	if err != nil {
		t.Fatal(err)
	}
	s := newSession(conv.ID, mgr, conv)
	defs := mgr.tools.Definitions(s)
	names := map[string]bool{}
	for _, def := range defs {
		names[def.Function.Name] = true
	}
	if !names[search.TavilyToolName] || !names[search.GoogleToolName] || names[search.ToolName] {
		t.Fatalf("应该只声明配置过的两个托管搜索工具: %+v", names)
	}

	entry := &toolExecEntry{}
	entry.call.Function.Name = search.GoogleToolName
	entry.call.Function.Arguments = `{"query":"长尾资料"}`
	s.execToolCall(&generation{ctx: context.Background()}, entry)
	if google.query != "长尾资料" || tavily.query != "" {
		t.Fatalf("工具路由到了错误后端: Google=%q Tavily=%q", google.query, tavily.query)
	}
	if !strings.Contains(entry.output, "Google 结果") {
		t.Fatalf("工具回执不对: %q", entry.output)
	}
	record, ok := entry.record.(search.Record)
	if !ok || record.Provider != "Google" {
		t.Fatalf("搜索记录没有标明 Google: %+v", entry.record)
	}
}
