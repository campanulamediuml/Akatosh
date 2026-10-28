package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// streamChunk 是 OpenAI 兼容流式响应的一个分片。
// 字段做了冗余兼容：reasoning_content 是 DeepSeek 的写法，
// reasoning 是 OpenRouter / 部分网关的写法；message 用于兼容
// 「不支持 stream、直接返回完整 message」的网关。
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			// 流式 tool_calls 是分片的：id/name 通常只在首片，
			// arguments 是逐片增量的字符串，要靠 index 归并拼接。
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Message *struct {
			Content          string     `json:"content"`
			ReasoningContent string     `json:"reasoning_content"`
			Reasoning        string     `json:"reasoning"`
			ToolCalls        []ToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *rawUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

func buildChatBody(req ChatRequest, withEffort, withTools, withStreamOpts bool) map[string]any {
	msgs := req.Messages
	if msgs == nil {
		msgs = []ChatMessage{}
	}
	body := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   true,
	}
	if withStreamOpts {
		// Kimi/Moonshot 只有显式传了才会在流尾返回 usage（OpenAI 标准参数，
		// DeepSeek / OpenAI 也支持；不认识的网关走 400 兜底去掉重试）
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if withEffort && req.Effort != "" && req.Effort != "none" && req.Effort != "off" {
		body["reasoning_effort"] = req.Effort
	}
	if withTools && len(req.Tools) > 0 {
		body["tools"] = req.Tools
		body["tool_choice"] = "auto"
	}
	return body
}

// Stream 发起流式对话请求，返回事件通道。
// 上游返回非 2xx 时同步返回 error（这样调用方能立刻把鉴权失败告诉用户），
// 否则在后台 goroutine 里持续解析。
//
// toolsDropped 为 true 表示上游不认识 tools 参数（返回 400）、已自动去掉
// 工具声明重试成功 —— 调用方可以据此提示「这家上游不支持工具调用」。
//
// 兼容性兜底：上游因为不认识 reasoning_effort / tools / stream_options
// 返回 400 时，会自动去掉对应参数重试一次 —— 很多网关对未知字段是直接报错的。
func (c *Client) Stream(ctx context.Context, req ChatRequest) (<-chan Event, bool, error) {
	resp, err := c.do(ctx, "/chat/completions", buildChatBody(req, true, true, true))
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode == http.StatusBadRequest && req.Effort != "" {
		body := readBody(resp)
		resp.Body.Close()
		if strings.Contains(strings.ToLower(body), "reasoning_effort") {
			log.Printf("[llm] 上游不认识 reasoning_effort=%q（model=%s），已去掉该参数重试", req.Effort, req.Model)
			resp, err = c.do(ctx, "/chat/completions", buildChatBody(req, false, true, true))
			if err != nil {
				return nil, false, err
			}
		} else {
			return nil, false, &APIError{Status: http.StatusBadRequest, Body: body}
		}
	}
	toolsDropped := false
	if resp.StatusCode == http.StatusBadRequest && len(req.Tools) > 0 {
		body := readBody(resp)
		resp.Body.Close()
		if strings.Contains(strings.ToLower(body), "tools") || strings.Contains(strings.ToLower(body), "tool_choice") {
			log.Printf("[llm] 上游不认识 tools（model=%s），已去掉工具声明重试", req.Model)
			toolsDropped = true
			resp, err = c.do(ctx, "/chat/completions", buildChatBody(req, false, false, true))
			if err != nil {
				return nil, false, err
			}
		} else {
			return nil, false, &APIError{Status: http.StatusBadRequest, Body: body}
		}
	}
	if resp.StatusCode == http.StatusBadRequest {
		body := readBody(resp)
		resp.Body.Close()
		if strings.Contains(strings.ToLower(body), "stream_options") {
			log.Printf("[llm] 上游不认识 stream_options（model=%s），已去掉该参数重试（usage 统计将缺失）", req.Model)
			resp, err = c.do(ctx, "/chat/completions", buildChatBody(req, false, false, false))
			if err != nil {
				return nil, false, err
			}
		} else {
			return nil, toolsDropped, &APIError{Status: http.StatusBadRequest, Body: body}
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, toolsDropped, &APIError{Status: resp.StatusCode, Body: readBody(resp)}
	}

	ch := make(chan Event, 64)
	go parseStream(ctx, resp.Body, ch)
	return ch, toolsDropped, nil
}

// parseStream 解析上游 SSE。任何解析不了的行都跳过而不是中断整个流 ——
// 上游五花八门的 keep-alive / 注释行很常见。
func parseStream(ctx context.Context, body io.ReadCloser, out chan<- Event) {
	defer close(out)
	defer body.Close()

	send := func(ev Event) bool {
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // 单行最大 8MB，防止超长分片被截断

	usage := Usage{}
	gotUsage := false
	finishReason := ""
	emitted := false // 是否已经吐出过任何内容
	failed := false  // 上游报错，不再补发 done

	// 流式 tool_calls 的归并：index → 累积器。id/name 只在首片出现，
	// arguments 是逐片增量。有些上游干脆不给 index（恒 0），单调用时同样正确。
	type tcAccum struct {
		id, typ, name string
		args          strings.Builder
	}
	accums := map[int]*tcAccum{}
	accumOrder := []int{}     // map 遍历无序，tool_calls 的顺序要按首次出现的 index 保留
	fullCalls := []ToolCall{} // 「不支持 stream」网关经 message 全量返回的调用

	handle := func(payload string) (stop bool) {
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			return payload == "[DONE]"
		}
		var ck streamChunk
		if err := json.Unmarshal([]byte(payload), &ck); err != nil {
			return false // 不是 JSON，忽略
		}
		if ck.Error != nil && ck.Error.Message != "" {
			failed = true
			send(Event{Type: EventError, Err: fmt.Errorf("%s", ck.Error.Message)})
			return true
		}
		if ck.Usage != nil {
			usage = ck.Usage.toUsage()
			gotUsage = true
		}
		for _, ch := range ck.Choices {
			r := ch.Delta.ReasoningContent
			if r == "" {
				r = ch.Delta.Reasoning
			}
			// 兼容「不支持 stream」的网关：完整内容塞在 message 里
			if ch.Message != nil {
				if r == "" {
					r = ch.Message.ReasoningContent
					if r == "" {
						r = ch.Message.Reasoning
					}
				}
				if ch.Delta.Content == "" && ch.Message.Content != "" {
					ch.Delta.Content = ch.Message.Content
				}
				if len(ch.Message.ToolCalls) > 0 {
					fullCalls = append(fullCalls, ch.Message.ToolCalls...)
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				emitted = true
				a, ok := accums[tc.Index]
				if !ok {
					a = &tcAccum{}
					accums[tc.Index] = a
					accumOrder = append(accumOrder, tc.Index)
				}
				if tc.ID != "" {
					a.id = tc.ID
				}
				if tc.Type != "" {
					a.typ = tc.Type
				}
				if tc.Function.Name != "" {
					a.name = tc.Function.Name
				}
				a.args.WriteString(tc.Function.Arguments)
				// arguments 增量实时吐出去：前端能看到模型逐字写代码/参数
				if tc.Function.Arguments != "" {
					if !send(Event{Type: EventToolArgs, Delta: tc.Function.Arguments}) {
						return true
					}
				}
			}
			if r != "" {
				emitted = true
				if !send(Event{Type: EventReasoning, Delta: r}) {
					return true
				}
			}
			if ch.Delta.Content != "" {
				emitted = true
				if !send(Event{Type: EventContent, Delta: ch.Delta.Content}) {
					return true
				}
			}
			if ch.FinishReason != "" {
				finishReason = ch.FinishReason
			}
		}
		return false
	}

	streamEnded := false
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		trimmed := strings.TrimSpace(strings.TrimRight(sc.Text(), "\r"))
		switch {
		case trimmed == "":
			continue
		case strings.HasPrefix(trimmed, ":"): // SSE 注释 / 心跳
			continue
		case strings.HasPrefix(trimmed, "event:"):
			continue // OpenAI 兼容流不带 event 名，忽略
		case strings.HasPrefix(trimmed, "data:"):
			streamEnded = handle(strings.TrimPrefix(trimmed, "data:"))
		case strings.HasPrefix(trimmed, "{"):
			// 有的网关无视 stream:true，直接返回一整个 JSON
			streamEnded = handle(trimmed)
		default:
			continue
		}
		if streamEnded {
			break
		}
	}

	if !streamEnded && ctx.Err() == nil {
		if err := sc.Err(); err != nil {
			send(Event{Type: EventError, Err: fmt.Errorf("读取上游响应失败: %w", err)})
			return
		}
	}
	if failed {
		return
	}
	if gotUsage {
		send(Event{Type: EventUsage, Usage: usage})
	}
	// 模型要求调用工具：归并分片成完整调用，发 EventToolCalls 而不是 EventDone ——
	// 由调用方决定执行工具后续传，还是就此收尾。
	toolCalls := fullCalls
	if len(toolCalls) == 0 && len(accums) > 0 {
		for _, idx := range accumOrder {
			a := accums[idx]
			tc := ToolCall{ID: a.id, Type: a.typ}
			if tc.Type == "" {
				tc.Type = "function"
			}
			tc.Function.Name = a.name
			tc.Function.Arguments = a.args.String()
			toolCalls = append(toolCalls, tc)
		}
	}
	if len(toolCalls) > 0 && (finishReason == "tool_calls" || finishReason == "function_call") {
		send(Event{Type: EventToolCalls, ToolCalls: toolCalls, FinishReason: finishReason})
		return
	}
	if !emitted && finishReason == "" {
		send(Event{Type: EventError, Err: fmt.Errorf("上游没有返回任何内容（可能是模型名不对，或该网关不支持流式）")})
		return
	}
	send(Event{Type: EventDone, FinishReason: finishReason})
}

// ampleTokens 是「正文为空时」补枪用的额度：推理模型的思考要从这里扣，
// 给太小会被思考吃光，正文仍然是空的。
const ampleTokens = 2048

// Complete 发一次**非流式**请求，返回正文与用量，给起标题、上下文压缩这类任务用。
// effort 为空调用方不下发 reasoning_effort；起标题传 none，压缩传会话的思考强度。
//
// 这里兜住了两个实测出来的坑：
//
//  1. 推理模型在小 max_tokens 下会把额度**全花在 CoT 上、正文一个字都不返回**。
//     实测 DeepSeek deepseek-flash：max_tokens=512 时 finish_reason=length，
//     512 个 completion token 全是 reasoning，content 是空串。
//  2. 不是所有上游都认 reasoning_effort 这个参数。
//
// 上游不认这个参数就摘掉重试；万一正文还是空的，再放大额度补一枪。
func (c *Client) Complete(ctx context.Context, model string, msgs []ChatMessage, maxTokens int, effort string) (string, Usage, error) {
	out, usage, err := c.completeOnce(ctx, model, msgs, maxTokens, effort)
	if err != nil {
		var ae *APIError
		unsupported := errors.As(err, &ae) &&
			ae.Status == http.StatusBadRequest &&
			strings.Contains(strings.ToLower(ae.Body), "reasoning_effort")
		if !unsupported {
			return "", Usage{}, err
		}
		out, usage, err = c.completeOnce(ctx, model, msgs, maxTokens, "")
		if err != nil {
			return "", Usage{}, err
		}
	}
	if strings.TrimSpace(out) != "" {
		return out, usage, nil
	}
	// 正文是空的：多半是额度被思考吃光了，放大额度再来一次
	budget := maxTokens
	if budget < ampleTokens {
		budget = ampleTokens
	}
	if retry, retryUsage, err2 := c.completeOnce(ctx, model, msgs, budget, ""); err2 == nil && strings.TrimSpace(retry) != "" {
		return retry, retryUsage, nil
	}
	return out, usage, nil
}

// completeOnce 是一次具体的非流式请求。effort 为空表示不带该参数。
func (c *Client) completeOnce(ctx context.Context, model string, msgs []ChatMessage, maxTokens int, effort string) (string, Usage, error) {
	if msgs == nil {
		msgs = []ChatMessage{}
	}
	body := map[string]any{
		"model":    model,
		"messages": msgs,
		"stream":   false,
	}
	if maxTokens > 0 {
		body["max_tokens"] = maxTokens
	}
	if effort != "" {
		body["reasoning_effort"] = effort
	}
	resp, err := c.do(ctx, "/chat/completions", body)
	if err != nil {
		return "", Usage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", Usage{}, &APIError{Status: resp.StatusCode, Body: readBody(resp)}
	}

	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *rawUsage `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", Usage{}, fmt.Errorf("解析上游响应失败: %w", err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return "", Usage{}, fmt.Errorf("%s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", Usage{}, fmt.Errorf("上游没有返回任何选择项")
	}
	return out.Choices[0].Message.Content, out.Usage.toUsage(), nil
}

// do 发一个 JSON POST 请求。
func (c *Client) do(ctx context.Context, path string, body any) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("请求 %s 失败: %w", c.BaseURL+path, err)
	}
	return resp, nil
}

func readBody(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	return string(b)
}
