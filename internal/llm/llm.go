package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ChatMessage 是发给上游的一条消息。
//
// Content 类型是 any：纯文本时是 string，带图片时是 []ContentPart（OpenAI 兼容的
// 多段 content）。注意这里刻意没有 Reasoning 字段 —— CoT 只存本地用于显示，
// 绝不能回传给上游（DeepSeek 等会因为 assistant 消息带 reasoning_content 直接报错）。
//
// ToolCalls 只在 assistant 消息上出现（模型要求调工具）；role=tool 的消息
// 用 ToolCallID 对应回那次调用。这条纪律同样适用：assistant 消息回传时
// 只带 content + tool_calls，不带任何思考内容。
//
// Reasoning 例外（实测 2026-10）：DeepSeek 在**思考模式下的 tool 循环**里
// 强制要求回传 reasoning_content（不带直接 400：
// "The `reasoning_content` in the thinking mode must be passed back to the API"）。
// 注意这只针对 tool 循环内的 assistant 消息；跨轮历史（新用户消息之后）
// 依然不带 reasoning（上游不强制，还能省 token）。
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Reasoning  string     `json:"reasoning_content,omitempty"`
}

// ToolCall 是模型发来的一次工具调用请求。
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // 固定 "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON 字符串（上游就是按字符串传的）
	} `json:"function"`
}

// Tool 是声明给模型的一个可用工具。
type Tool struct {
	Type     string `json:"type"` // 固定 "function"
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"` // JSON Schema
	} `json:"function"`
}

// ContentPart 是多段 content 里的一段。
type ContentPart struct {
	Type     string    `json:"type"` // text | image_url
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL 用 data URL 传图：本地服务没有公网地址，上游取不到我们的 URL。
type ImageURL struct {
	URL string `json:"url"`
}

// TextPart 造一段文本。
func TextPart(s string) ContentPart { return ContentPart{Type: "text", Text: s} }

// ImagePart 造一段图片（dataURL 形如 data:image/png;base64,xxxx）。
func ImagePart(dataURL string) ContentPart {
	return ContentPart{Type: "image_url", ImageURL: &ImageURL{URL: dataURL}}
}

// BuildContent 把若干段拼成 Content：只有文本时返回 string（兼容性最好，
// 有些老网关不认数组形式），有图才返回数组。
func BuildContent(parts []ContentPart) any {
	hasImage := false
	for _, p := range parts {
		if p.Type == "image_url" {
			hasImage = true
			break
		}
	}
	if !hasImage {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return parts
}

type Usage struct {
	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int
	TotalTokens      int
	// 缓存命中/未命中。
	// DeepSeek 直接给 prompt_cache_hit_tokens / prompt_cache_miss_tokens；
	// OpenAI 系只给 prompt_tokens_details.cached_tokens。两种都认。
	CacheHitTokens  int
	CacheMissTokens int
}

// 上游 usage 里我们关心的字段，OpenAI 兼容各家大同小异，这里做一次归一。
type rawUsage struct {
	PromptTokens          int `json:"prompt_tokens"`
	CompletionTokens      int `json:"completion_tokens"`
	TotalTokens           int `json:"total_tokens"`
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
	PromptTokensDetails   *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// toUsage 把上游的原始 usage 归一成我们的结构。
func (r *rawUsage) toUsage() Usage {
	if r == nil {
		return Usage{}
	}
	u := Usage{
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
		TotalTokens:      r.TotalTokens,
		CacheHitTokens:   r.PromptCacheHitTokens,
		CacheMissTokens:  r.PromptCacheMissTokens,
	}
	if u.CacheHitTokens == 0 && r.PromptTokensDetails != nil {
		u.CacheHitTokens = r.PromptTokensDetails.CachedTokens
	}
	// 有的上游只给命中数，未命中就按总数减出来
	if u.CacheMissTokens == 0 && r.PromptTokens > 0 {
		if miss := r.PromptTokens - u.CacheHitTokens; miss > 0 {
			u.CacheMissTokens = miss
		}
	}
	if r.CompletionTokensDetails != nil {
		u.ReasoningTokens = r.CompletionTokensDetails.ReasoningTokens
	}
	return u
}

type EventType int

const (
	EventReasoning EventType = iota
	EventContent
	EventUsage
	EventDone
	EventError
	// EventToolCalls：流结束且模型要求调用工具（finish_reason=tool_calls）。
	// 此时不会补发 EventDone —— 是否继续（执行工具后续传）由调用方决定。
	EventToolCalls
	// EventToolArgs：tool_calls 的 arguments 流式分片（JSON 增量文本）。
	// 前端据此实时显示「模型正在写什么代码/参数」。
	EventToolArgs
)

// Event 是上游流解析后产出的统一事件。
type Event struct {
	Type         EventType
	Delta        string
	Usage        Usage
	FinishReason string
	ToolCalls    []ToolCall // 仅 EventToolCalls 时非空（已从流式分片累积成完整调用）
	Err          error
}

type ChatRequest struct {
	Model    string
	Messages []ChatMessage
	Effort   string // "" 表示不发送 reasoning_effort
	Tools    []Tool // 空 = 不声明工具（上游行为与不带 tools 参数完全一致）
}

// Client 实现 OpenAI 兼容协议（/chat/completions 与 /models）。
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// APIError 是上游返回的非 2xx 响应。
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	msg := extractErrMessage(e.Body)
	if msg == "" {
		msg = strings.TrimSpace(e.Body)
	}
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	return fmt.Sprintf("%d %s", e.Status, msg)
}

// extractErrMessage 从上游错误体里挖出人类可读的那句话。
func extractErrMessage(body string) string {
	var v struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &v); err == nil {
		if v.Error.Message != "" {
			return v.Error.Message
		}
		if v.Message != "" {
			return v.Message
		}
	}
	return ""
}

func New(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: NormalizeBaseURL(baseURL),
		APIKey:  apiKey,
		HTTP: &http.Client{
			// 不设 Timeout / ResponseHeaderTimeout：长回答和非流式高思考请求都可能
			// 持续几分钟，超时由调用方的 context 控制（压缩 8 分钟、起标题 45 秒）。
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          16,
				MaxIdleConnsPerHost:   8,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
	}
}

// versionRe 匹配路径里已经存在的版本段，例如 /v1、/v4、/v1beta。
var versionRe = regexp.MustCompile(`(?i)/v[0-9]+[a-z]*(/|$)`)

// NormalizeBaseURL 把用户填的地址整理成可以直接拼 /chat/completions 的形式：
//   - 去掉首尾空白和结尾斜杠
//   - 用户要是把完整端点（.../chat/completions）粘进来了，帮他剥掉
//   - 路径里已经有版本段（/v1、/api/paas/v4、/compatible-mode/v1）就原样保留
//   - 否则补一个 /v1（DeepSeek、Ollama、one-api 这类都适用）
func NormalizeBaseURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = strings.TrimRight(s, "/")
	for _, suffix := range []string{"/chat/completions", "/completions", "/models"} {
		if strings.HasSuffix(strings.ToLower(s), suffix) {
			s = strings.TrimRight(s[:len(s)-len(suffix)], "/")
			break
		}
	}
	if s == "" {
		return ""
	}
	if versionRe.MatchString(s) {
		return s
	}
	return s + "/v1"
}

// Ping 发一个最小请求验证连通性（不流式）。
func (c *Client) Ping(ctx context.Context, model string) (time.Duration, error) {
	if model == "" {
		return 0, fmt.Errorf("未指定模型")
	}
	body := map[string]any{
		"model":      model,
		"messages":   []ChatMessage{{Role: "user", Content: "ping"}},
		"max_tokens": 1,
		"stream":     false,
	}
	start := time.Now()
	resp, err := c.do(ctx, "/chat/completions", body)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, &APIError{Status: resp.StatusCode, Body: readBody(resp)}
	}
	// 读掉响应体，确保连接可以复用
	readBody(resp)
	return time.Since(start), nil
}
