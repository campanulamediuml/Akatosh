// mockllm 是一个假的 OpenAI 兼容上游，只用于本地端到端验证。
//
//	go run ./tools/mockllm -addr 127.0.0.1:28899
//
// 它模拟真实上游的行为：/models 列表、SSE 流式输出 reasoning_content 与
// content、用量统计，以及对不支持的模型传 reasoning_effort 时返回 400。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type chunk struct {
	Choices []choice `json:"choices"`
	Usage   *usage   `json:"usage,omitempty"`
}

type choice struct {
	Delta        map[string]any `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

type usage struct {
	PromptTokens            int `json:"prompt_tokens"`
	CompletionTokens        int `json:"completion_tokens"`
	TotalTokens             int `json:"total_tokens"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

var models = []map[string]any{
	{"id": "mock-reasoner", "object": "model", "context_length": 65536},
	{"id": "mock-chat", "object": "model", "context_length": 32768},
	{"id": "gpt-5-mock", "object": "model"},
	{"id": "plain-model", "object": "model"},
}

func main() {
	addr := flag.String("addr", "127.0.0.1:28899", "监听地址")
	delay := flag.Duration("delay", 40*time.Millisecond, "每个分片之间的间隔")
	modelList := flag.String("models", "", "自定义模型列表，逗号分隔（用来模拟两个网关有同名模型）")
	reasoningList := flag.String("reasoning", "", "哪些模型会吐 CoT，逗号分隔；留空则按名字猜")
	visionList := flag.String("vision", "", "哪些模型能收图片，逗号分隔；没列到的收到图片会报 400（模拟真实上游）")
	flag.Parse()

	if strings.TrimSpace(*modelList) != "" {
		models = nil
		for _, id := range strings.Split(*modelList, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				models = append(models, map[string]any{"id": id, "object": "model"})
			}
		}
	}
	reasoningOverride := map[string]bool{}
	for _, id := range strings.Split(*reasoningList, ",") {
		if id = strings.TrimSpace(id); id != "" {
			reasoningOverride[id] = true
		}
	}
	visionSet := map[string]bool{}
	for _, id := range strings.Split(*visionList, ",") {
		if id = strings.TrimSpace(id); id != "" {
			visionSet[id] = true
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[mock] GET /v1/models  auth=%q", r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "Bearer bad-key" {
			http.Error(w, `{"error":{"message":"Incorrect API key provided","type":"authentication_error"}}`, 401)
			return
		}
		// 像真实网关那样把能力声明出来：-reasoning 列到的模型带 supported_parameters。
		// 这样「两个网关有同名模型但能力不同」才测得出串台。
		data := make([]map[string]any, 0, len(models))
		for _, m := range models {
			item := map[string]any{}
			for k, v := range m {
				item[k] = v
			}
			if id, _ := m["id"].(string); reasoningOverride[id] {
				item["supported_parameters"] = []string{"reasoning", "reasoning_effort"}
			}
			data = append(data, item)
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": data})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		handleChat(w, r, *delay, reasoningOverride, visionSet)
	})
	// 给测试用的自检接口：能看到最近一次请求收了几张图、文本是什么
	mux.HandleFunc("GET /__stats", func(w http.ResponseWriter, r *http.Request) {
		stats.mu.Lock()
		defer stats.mu.Unlock()
		writeJSON(w, 200, map[string]any{
			"chat_requests":  stats.chatRequests,
			"image_parts":    stats.imageParts,
			"last_model":     stats.lastModel,
			"last_user_text": stats.lastUserText,
			"last_image_kb":  stats.lastImageKB,
		})
	})
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"data": []any{}})
	})

	log.Printf("[mock] 假上游已启动: http://%s/v1 （每片间隔 %s，模型 %d 个）", *addr, *delay, len(models))
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

// mockStats 记录最近一次请求的关键信息，供测试断言「图片确实以 vision 形式到了上游」。
type mockStats struct {
	mu           sync.Mutex
	chatRequests int
	imageParts   int
	lastModel    string
	lastUserText string
	lastImageKB  int
}

var stats mockStats

// parseContent 解析一条消息的 content：可能是纯文本，也可能是多段（带图）。
// 返回文本、图片张数、图片总字节数。
func parseContent(raw json.RawMessage) (string, int, int) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", 0, 0
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s, 0, 0
		}
		return "", 0, 0
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", 0, 0
	}
	var sb strings.Builder
	imgs, bytes := 0, 0
	for _, p := range parts {
		switch p.Type {
		case "text":
			sb.WriteString(p.Text)
		case "image_url":
			imgs++
			if p.ImageURL != nil {
				bytes += len(p.ImageURL.URL)
			}
		}
	}
	return sb.String(), imgs, bytes
}

// willReason 决定这个模型这一轮吐不吐 CoT。
// 显式给了 -reasoning 就按它；否则按名字猜（和真实网关的启发式类似）。
func willReason(model, effort string, override map[string]bool) bool {
	if effort != "" {
		return true // 用户显式要了思考强度，就思考
	}
	if len(override) > 0 {
		return override[model]
	}
	return strings.Contains(model, "reasoner") ||
		strings.Contains(model, "gpt-5") ||
		strings.Contains(model, "think")
}

func handleChat(w http.ResponseWriter, r *http.Request, delay time.Duration, reasoningOverride, visionSet map[string]bool) {
	var body struct {
		Model           string `json:"model"`
		Stream          bool   `json:"stream"`
		ReasoningEffort string `json:"reasoning_effort"`
		MaxTokens       int    `json:"max_tokens"`
		// Content 可能是 string（纯文本），也可能是 []part（带图）。
		// 这里收成 RawMessage 自己判断，才能真的验证 vision 格式。
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":{"message":"bad json"}}`, 400)
		return
	}

	// 逐条消息解析出「文本」和「图片张数/字节数」
	lastUser := ""
	imageParts := 0
	imageBytes := 0
	for _, m := range body.Messages {
		text, imgs, bytes := parseContent(m.Content)
		if m.Role == "user" {
			lastUser = text
		}
		imageParts += imgs
		imageBytes += bytes
		if m.Role == "user" && imgs > 0 {
			lastUser = text
		}
	}

	stats.mu.Lock()
	if body.Stream {
		stats.chatRequests++
	}
	if imageParts > 0 {
		stats.imageParts = imageParts
		stats.lastImageKB = imageBytes / 1024
	}
	stats.lastModel = body.Model
	stats.lastUserText = lastUser
	stats.mu.Unlock()

	// 模拟真实上游：不支持视觉的模型收到图片会报 400
	if imageParts > 0 && len(visionSet) > 0 && !visionSet[body.Model] {
		log.Printf("[mock] 拒绝图片 model=%s（该模型不支持视觉）", body.Model)
		http.Error(w, `{"error":{"message":"This model does not support image input","type":"invalid_request_error"}}`, 400)
		return
	}

	// 模拟真实上游：不支持的模型收到 reasoning_effort 会报 400。
	// 客户端应当自动去掉该参数重试一次。
	if body.ReasoningEffort != "" && (body.Model == "plain-model" || body.Model == "mock-chat") {
		log.Printf("[mock] 拒绝 reasoning_effort model=%s", body.Model)
		http.Error(w, `{"error":{"message":"reasoning_effort is not supported by this model","type":"invalid_request_error"}}`, 400)
		return
	}
	// 模拟鉴权失败
	if auth := r.Header.Get("Authorization"); auth == "Bearer bad-key" {
		http.Error(w, `{"error":{"message":"Incorrect API key provided","type":"authentication_error"}}`, 401)
		return
	}

	isReasoner := willReason(body.Model, body.ReasoningEffort, reasoningOverride)
	log.Printf("[mock] POST chat model=%s stream=%v effort=%q reasoning=%v msgs=%d lastUser=%q",
		body.Model, body.Stream, body.ReasoningEffort, isReasoner, len(body.Messages), truncate(lastUser, 40))

	// 非流式（测试连接、以及「让模型起标题」走这条）
	if !body.Stream {
		content := "pong"
		if strings.Contains(lastUser, "起一个标题") || strings.Contains(lastUser, "标题") {
			// 故意带引号和前缀，用来验证后端会清洗干净
			content = "「Go 并发模型速览」"
		}
		writeJSON(w, 200, map[string]any{
			"id": "chatcmpl-mock", "object": "chat.completion", "model": body.Model,
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": content},
			}},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)

	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(delay)
	}

	reasoning := []string{
		"让我想想这个问题。", "用户问的是「", truncate(lastUser, 20), "」。",
		"\n\n第一步，先明确概念边界；", "第二步，考虑几种可能的方案：",
		"\n1. 方案甲——简单直接；", "\n2. 方案乙——更稳妥；",
		"\n\n这里有个坑需要注意：多字节字符不能被切断。",
		"最后综合给出结论。",
	}
	answer := []string{
		"结论如下：\n\n", "这是 **mock 上游** 返回的正文，", "用来验证流式链路。\n\n",
		"```go\n", "func main() {\n", "\tfmt.Println(\"你好，世界\")\n", "}\n", "```\n\n",
		"包含中文、emoji 🚀 和代码块，", "用来检查增量切片与 markdown 渲染。",
	}

	if isReasoner {
		for _, s := range reasoning {
			send(chunk{Choices: []choice{{Delta: map[string]any{"reasoning_content": s}}}})
		}
	}
	for _, s := range answer {
		send(chunk{Choices: []choice{{Delta: map[string]any{"content": s}}}})
	}

	stop := "stop"
	send(chunk{Choices: []choice{{Delta: map[string]any{}, FinishReason: &stop}}})

	u := usage{PromptTokens: 128, CompletionTokens: 256, TotalTokens: 384}
	u.CompletionTokensDetails = &struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	}{ReasoningTokens: 96}
	send(chunk{Usage: &u})

	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
