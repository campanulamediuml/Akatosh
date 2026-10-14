// Package search 是联网搜索工具的执行端：调 SearXNG 的 JSON API，
// 把结果修剪成适合喂给模型的形态，并给出 web_search 的工具声明。
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"aiassistant/internal/llm"
)

const (
	// maxResults 是喂给模型的结果条数上限。再多只会稀释上下文、
	// 拖垮前缀缓存命中率，对回答质量没有帮助。
	maxResults = 6
	// maxContentRunes 是单条摘要的长度上限（SearXNG 给的引擎摘要本身不长，
	// 但个别站点会塞整段正文）。
	maxContentRunes = 300
	// ToolName 是工具名，session 按它识别「这是搜索调用」。
	ToolName = "web_search"
)

// Result 是一条修剪后的搜索结果。
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

// Record 是「本轮搜过什么」的持久摘要，落库到消息的 search_log 给界面展示。
// 刻意不存 content：那只是引擎摘要，占地方且没有复看价值；标题 + 链接足够。
type Record struct {
	Type    string `json:"type"` // 固定 "search"，和 Python 执行记录（"python"）区分
	Query   string `json:"query"`
	Results []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"results"`
	// 搜索失败时记录原因（工具消息里也会如实告诉模型，让它自己圆场）
	Error string `json:"error,omitempty"`
}

// Client 调 SearXNG。
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		HTTP: &http.Client{
			Timeout: 10 * time.Second, // 搜索是生成的附属环节，慢了就宁可没有
		},
	}
}

// searxResponse 是 SearXNG format=json 的响应（只取用得上的字段）。
type searxResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

// Search 执行一次搜索，返回修剪后的结果。
func (c *Client) Search(ctx context.Context, query string) ([]Result, error) {
	u := c.BaseURL + "/search?q=" + url.QueryEscape(query) + "&format=json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("搜索服务返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out searxResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("解析搜索结果失败: %w", err)
	}

	results := make([]Result, 0, maxResults)
	for _, r := range out.Results {
		if len(results) >= maxResults {
			break
		}
		title := strings.TrimSpace(r.Title)
		if title == "" || r.URL == "" {
			continue
		}
		results = append(results, Result{
			Title:   title,
			URL:     r.URL,
			Content: truncateRunes(strings.TrimSpace(r.Content), maxContentRunes),
		})
	}
	return results, nil
}

// NewRecord 把一次搜索整理成可落库的摘要。err 非空表示搜索失败。
func NewRecord(query string, results []Result, err error) Record {
	rec := Record{Type: "search", Query: query}
	if err != nil {
		rec.Error = err.Error()
		return rec
	}
	for _, r := range results {
		rec.Results = append(rec.Results, struct {
			Title string `json:"title"`
			URL   string `json:"url"`
		}{Title: r.Title, URL: r.URL})
	}
	return rec
}

// FormatToolResult 把搜索结果拼成 role=tool 消息的内容（给模型看的）。
// 搜索失败也走这里：如实告诉模型，让它决定换个问法还是坦白说不知道。
func FormatToolResult(results []Result, searchErr error) string {
	if searchErr != nil {
		return fmt.Sprintf(`{"error": %q}`, "搜索失败："+searchErr.Error())
	}
	if len(results) == 0 {
		return `{"results": [], "note": "没有搜到相关结果"}`
	}
	buf, err := json.Marshal(map[string]any{"results": results})
	if err != nil {
		return `{"error": "搜索结果序列化失败"}`
	}
	return string(buf)
}

// ToolDef 是 web_search 的工具声明（OpenAI tools 格式）。
// description 用中文写清使用时机 —— 声明直接决定模型什么时候愿意调它。
func ToolDef() llm.Tool {
	var t llm.Tool
	t.Type = "function"
	t.Function.Name = ToolName
	t.Function.Description = "搜索互联网获取实时信息。当问题涉及最新新闻、时事、天气、汇率、近期发布的内容，或你不确定的事实需要查证时调用。不要用于你确知的常识。"
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

// ParseQuery 从 tool call 的 arguments（JSON 字符串）里挖出 query。
// 模型偶尔会给出畸形 JSON，解析失败时退而求其次把整串当关键词 ——
// 总比直接报错让这一轮断掉强。
func ParseQuery(arguments string) string {
	var v struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(arguments), &v); err == nil && strings.TrimSpace(v.Query) != "" {
		return strings.TrimSpace(v.Query)
	}
	s := strings.TrimSpace(arguments)
	if utf8.RuneCountInString(s) > 100 {
		s = truncateRunes(s, 100)
	}
	return s
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}
