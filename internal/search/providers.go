package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"aiassistant/internal/llm"
)

const (
	TavilyToolName = "web_search_tavily"
	GoogleToolName = "web_search_google"
)

// Executor 是一个实际搜索后端。Provider 在它上面补充工具名与展示信息。
type Executor interface {
	Search(ctx context.Context, query string) ([]Result, error)
}

// Provider 把一个搜索后端注册成独立的 function calling 工具。
type Provider struct {
	ToolName    string
	DisplayName string
	Description string
	Executor    Executor
}

func (p Provider) Search(ctx context.Context, query string) ([]Result, error) {
	if p.Executor == nil {
		return nil, fmt.Errorf("搜索后端未配置")
	}
	return p.Executor.Search(ctx, query)
}

func (p Provider) ToolDef() llm.Tool {
	var t llm.Tool
	t.Type = "function"
	t.Function.Name = p.ToolName
	t.Function.Description = p.Description
	t.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "搜索关键词，简洁明确，可以用中文",
			},
		},
		"required": []string{"query"},
	}
	return t
}

// LegacyProvider 保留原 SearXNG 的单工具配置方式。
func LegacyProvider(c *Client) Provider {
	return Provider{
		ToolName:    ToolName,
		DisplayName: "SearXNG",
		Description: "搜索互联网获取实时信息。当问题涉及最新新闻、时事、天气、汇率、近期发布的内容，或你不确定的事实需要查证时调用。不要用于你确知的常识。",
		Executor:    c,
	}
}

// TavilyClient 调 Tavily Search API。
type TavilyClient struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client
}

func NewTavily(apiKey string) Provider {
	return Provider{
		ToolName:    TavilyToolName,
		DisplayName: "Tavily",
		Description: "默认的互联网搜索工具，适合常规资料查证、新闻、时事、天气和一般实时信息。需要联网搜索时优先使用；如果结果不足，可再使用 Google 搜索补充。",
		Executor: &TavilyClient{
			APIKey:  strings.TrimSpace(apiKey),
			BaseURL: "https://api.tavily.com",
			HTTP:    &http.Client{Timeout: 15 * time.Second},
		},
	}
}

func (c *TavilyClient) Search(ctx context.Context, query string) ([]Result, error) {
	body := map[string]any{
		"query":               query,
		"max_results":         maxResults,
		"search_depth":        "basic",
		"include_answer":      false,
		"include_raw_content": false,
	}
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	headers := map[string]string{"Authorization": "Bearer " + c.APIKey}
	if err := postSearch(ctx, c.HTTP, c.BaseURL+"/search", headers, body, &out); err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(out.Results))
	for _, r := range out.Results {
		results = append(results, Result{Title: r.Title, URL: r.URL, Content: r.Content})
	}
	return trimResults(results), nil
}

// SerperClient 通过 Serper 调托管的 Google 搜索。
type SerperClient struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client
}

func NewGoogle(apiKey string) Provider {
	return Provider{
		ToolName:    GoogleToolName,
		DisplayName: "Google",
		Description: "通过 Google 搜索互联网。用户明确要求使用 Google，或需要精确短语、站内搜索、冷门长尾资料时使用；Tavily 找不到或结果不足时也用它补充。",
		Executor: &SerperClient{
			APIKey:  strings.TrimSpace(apiKey),
			BaseURL: "https://google.serper.dev",
			HTTP:    &http.Client{Timeout: 15 * time.Second},
		},
	}
}

func (c *SerperClient) Search(ctx context.Context, query string) ([]Result, error) {
	body := map[string]any{"q": query, "num": maxResults}
	var out struct {
		Organic []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic"`
	}
	headers := map[string]string{"X-API-KEY": c.APIKey}
	if err := postSearch(ctx, c.HTTP, c.BaseURL+"/search", headers, body, &out); err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(out.Organic))
	for _, r := range out.Organic {
		results = append(results, Result{Title: r.Title, URL: r.Link, Content: r.Snippet})
	}
	return trimResults(results), nil
}

func postSearch(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, input, output any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(input); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("搜索服务返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(output); err != nil {
		return fmt.Errorf("解析搜索结果失败: %w", err)
	}
	return nil
}

func trimResults(input []Result) []Result {
	out := make([]Result, 0, maxResults)
	for _, r := range input {
		if len(out) >= maxResults {
			break
		}
		title := strings.TrimSpace(r.Title)
		link := strings.TrimSpace(r.URL)
		if title == "" || link == "" {
			continue
		}
		out = append(out, Result{
			Title:   title,
			URL:     link,
			Content: truncateRunes(strings.TrimSpace(r.Content), maxContentRunes),
		})
	}
	return out
}
