package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTavilySearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/search" {
			t.Errorf("请求路径不对: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization 不对: %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["query"] != "最新新闻" || body["max_results"] != float64(maxResults) {
			t.Errorf("请求体不对: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[{"title":" Tavily 标题 ","url":" https://tavily.example.com ","content":" Tavily 摘要 "}]}`)
	}))
	t.Cleanup(srv.Close)

	provider := NewTavily("test-key")
	client := provider.Executor.(*TavilyClient)
	client.BaseURL = srv.URL
	client.HTTP = srv.Client()
	results, err := provider.Search(context.Background(), "最新新闻")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Title != "Tavily 标题" || results[0].URL != "https://tavily.example.com" || results[0].Content != "Tavily 摘要" {
		t.Fatalf("结果映射不对: %+v", results)
	}
}

func TestGoogleSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-API-KEY"); got != "google-key" {
			t.Errorf("X-API-KEY 不对: %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["q"] != "精确搜索" || body["num"] != float64(maxResults) {
			t.Errorf("请求体不对: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"organic":[{"title":"Google 标题","link":"https://google.example.com","snippet":"Google 摘要"}]}`)
	}))
	t.Cleanup(srv.Close)

	provider := NewGoogle("google-key")
	client := provider.Executor.(*SerperClient)
	client.BaseURL = srv.URL
	client.HTTP = srv.Client()
	results, err := provider.Search(context.Background(), "精确搜索")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Title != "Google 标题" || results[0].URL != "https://google.example.com" || results[0].Content != "Google 摘要" {
		t.Fatalf("结果映射不对: %+v", results)
	}
}

func TestManagedProviderDefinitionsAndErrors(t *testing.T) {
	tavily := NewTavily("t")
	google := NewGoogle("g")
	if tavily.ToolDef().Function.Name != TavilyToolName || google.ToolDef().Function.Name != GoogleToolName {
		t.Fatalf("工具名不对: %s %s", tavily.ToolDef().Function.Name, google.ToolDef().Function.Name)
	}
	if tavily.ToolDef().Function.Description == "" || google.ToolDef().Function.Description == "" {
		t.Fatal("两个工具都要给模型明确的选择说明")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, "rate limited")
	}))
	t.Cleanup(srv.Close)
	client := tavily.Executor.(*TavilyClient)
	client.BaseURL = srv.URL
	client.HTTP = srv.Client()
	if _, err := tavily.Search(context.Background(), "q"); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("上游错误要如实返回: %v", err)
	}
}
