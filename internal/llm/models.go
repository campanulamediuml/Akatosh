package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// RemoteModel 是从上游 /models 拉到的一个模型。
type RemoteModel struct {
	ID         string   `json:"id"`
	ContextLen int      `json:"context_len"`
	Reasoning  bool     `json:"reasoning"`
	Efforts    []string `json:"efforts"`
}

// ListModels 拉取上游模型列表（OpenAI 兼容的 GET /models）。
func (c *Client) ListModels(ctx context.Context) ([]RemoteModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("请求 %s 失败: %w", c.BaseURL+"/models", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &APIError{Status: resp.StatusCode, Body: readBody(resp)}
	}

	var payload struct {
		Data []struct {
			ID                  string          `json:"id"`
			Name                string          `json:"name"`
			ContextLength       int             `json:"context_length"`
			ContextWindow       int             `json:"context_window"`
			MaxModelLen         int             `json:"max_model_len"`
			Reasoning           json.RawMessage `json:"reasoning"`
			SupportedParameters []string        `json:"supported_parameters"`
			Capabilities        map[string]any  `json:"capabilities"`
			TopProvider         *struct {
				ContextLength int `json:"context_length"`
			} `json:"top_provider"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("解析模型列表失败（返回的可能不是 OpenAI 兼容的 /models 响应）: %w", err)
	}

	out := make([]RemoteModel, 0, len(payload.Data))
	for _, m := range payload.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			id = strings.TrimSpace(m.Name)
		}
		if id == "" {
			continue
		}
		rm := RemoteModel{ID: id}
		switch {
		case m.ContextLength > 0:
			rm.ContextLen = m.ContextLength
		case m.MaxModelLen > 0:
			rm.ContextLen = m.MaxModelLen
		case m.ContextWindow > 0:
			rm.ContextLen = m.ContextWindow
		case m.TopProvider != nil:
			rm.ContextLen = m.TopProvider.ContextLength
		}
		if rm.ContextLen == 0 {
			// 上游没给上下文长度：按模型名推断（拿不准的系列返回 0 = 不显示）
			rm.ContextLen = InferContextLen(id)
		}

		// 思考能力：优先用上游显式声明的，其次按模型名推断。
		rm.Reasoning, rm.Efforts = capabilitiesOf(id, m.Reasoning, m.SupportedParameters, m.Capabilities)
		out = append(out, rm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// capabilitiesOf 判断模型是否支持思考强度，以及支持哪些档位。
func capabilitiesOf(id string, reasoningRaw json.RawMessage, supportedParams []string, caps map[string]any) (bool, []string) {
	// 1) OpenRouter 风格：reasoning: {"supported_efforts": ["low","medium","high"]}
	//    以及 Kimi coding 风格（2026 新增）：reasoning: {"type": "only",
	//    "effort": {"valid": ["low","high","max"], "default": "high"}}
	//    —— 两轴分开：type 声明「能否思考」，effort.valid 声明「有无思考档位」。
	if len(reasoningRaw) > 0 {
		var obj struct {
			SupportedEfforts []string `json:"supported_efforts"`
			Mandatory        bool     `json:"mandatory"`
			// Kimi coding 的形状
			Type   string `json:"type"` // "only" = 始终思考；"optional" = 可开关
			Effort *struct {
				Valid   []string `json:"valid"`
				Default string   `json:"default"`
			} `json:"effort"`
		}
		if err := json.Unmarshal(reasoningRaw, &obj); err == nil {
			// 档位优先：effort.valid（Kimi）→ supported_efforts（OpenRouter）
			if obj.Effort != nil && len(obj.Effort.Valid) > 0 {
				return true, normalizeEfforts(obj.Effort.Valid)
			}
			if len(obj.SupportedEfforts) > 0 {
				return true, normalizeEfforts(obj.SupportedEfforts)
			}
			// 能思考但没档位：type 声明了思考能力（always/optional），effort 没有清单
			if obj.Type != "" {
				return true, []string{}
			}
		}
		var b bool
		if err := json.Unmarshal(reasoningRaw, &b); err == nil && b {
			return true, DefaultEfforts()
		}
	}
	// 2) supported_parameters 里出现 reasoning / reasoning_effort
	for _, p := range supportedParams {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "reasoning", "reasoning_effort", "thinking":
			return true, DefaultEfforts()
		}
	}
	// 3) capabilities: {"reasoning": true}
	for k, v := range caps {
		lk := strings.ToLower(k)
		if lk == "reasoning" || lk == "thinking" {
			if b, ok := v.(bool); ok && b {
				return true, DefaultEfforts()
			}
		}
	}
	// 4) MiMo v2 文本模型（v2.5 / v2.6 全系）：官方文档明确默认开启思考。
	// 官方只写了 thinking.type 开关、没提强度档位，但按「猜支持的代价很轻」
	// 的原则给出通用三档 —— reasoning_effort 不被认识时会自动去掉重试，
	// 最糟情况退回上游默认（思考开）。
	// 见 https://mimo.mi.com/docs/zh-CN/quick-start/usage-guide/text-generation/deep-thinking
	{
		lower := strings.ToLower(id)
		if strings.Contains(lower, "mimo-v2") {
			// asr/tts 是音频型号，不思考
			if strings.Contains(lower, "asr") || strings.Contains(lower, "tts") {
				return false, []string{}
			}
			return true, DefaultEfforts()
		}
	}
	// 5) 兜底：按模型名推断
	if InferReasoning(id) {
		return true, DefaultEfforts()
	}
	return false, []string{}
}

// DefaultEfforts 是默认提供的思考强度档位。
// 不同厂商支持的档位不完全一样，这里给一个通用的三档，
// 上游显式声明了档位时以声明为准。
func DefaultEfforts() []string { return []string{"low", "medium", "high"} }

// InferContextLen 按模型名猜上下文长度（上游 /models 没给时才用它兜底）。
//
// 原则与 InferReasoning 不同：这里必须**保守**。思考强度猜错了代价是多个没用的
// 参数（上游不认会被自动去掉）；上下文猜错了会让「剩余上下文」显示一个自信的错误值，
// 用户据此判断还能聊多少，会被坑。所以只写有把握的系列，拿不准就返回 0
// （界面不显示，比显示错数强）。
func InferContextLen(modelID string) int {
	s := strings.ToLower(modelID)
	switch {
	// DeepSeek V4 全系 1M（官方 API 实测返回 1048576）；老型号（V3/R1 时代）64K
	case strings.Contains(s, "deepseek-v4"), strings.Contains(s, "deepseek-flash"):
		return 1048576
	case strings.Contains(s, "deepseek"):
		return 65536
	case strings.Contains(s, "kimi-k3"):
		return 1048576
	case strings.Contains(s, "kimi-k2"):
		return 262144
	case strings.Contains(s, "kimi"):
		return 131072
	// GPT-6 家族（Astra/Sol/Luna）：延续 5.6 的 1.05M 上限
	case strings.Contains(s, "gpt-6"):
		return 1050000
	// GPT-5.4 起 API 上下文上限 105 万（OpenAI 官方模型页）；初版 GPT-5 是 400K
	case strings.Contains(s, "gpt-5.4"), strings.Contains(s, "gpt-5.5"), strings.Contains(s, "gpt-5.6"):
		return 1050000
	case strings.Contains(s, "gpt-5"):
		return 400000
	// gpt-4.1 是 1M；gpt-4o 及更早是 128K
	case strings.Contains(s, "gpt-4.1"):
		return 1048576
	case strings.Contains(s, "gpt-4"):
		return 128000
	case strings.Contains(s, "claude"):
		return 200000
	case strings.Contains(s, "gemini"):
		return 1048576
	case strings.Contains(s, "glm-4"):
		return 131072
	case strings.Contains(s, "qwen3"), strings.Contains(s, "qwen2.5"):
		return 131072
	case strings.Contains(s, "grok-4"):
		return 256000
	case strings.Contains(s, "mimo-v2"):
		// MiMo（小米）官方模型发布页：v2.5 / v2.5-pro / v2-pro 为 1M，
		// v2-omni / v2-flash 为 256K；asr/tts 是音频型号没有对话上下文的概念；
		// v2.6 系列官方未公布上下文长度 —— 拿不准不显示（返回 0）
		if strings.Contains(s, "asr") || strings.Contains(s, "tts") {
			return 0
		}
		if strings.Contains(s, "mimo-v2.5") || strings.Contains(s, "mimo-v2-pro") {
			return 1048576
		}
		if strings.Contains(s, "mimo-v2-omni") || strings.Contains(s, "mimo-v2-flash") {
			return 262144
		}
		return 0
	}
	return 0
}

func normalizeEfforts(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		switch e {
		case "minimal", "low", "medium", "high", "max":
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	if len(out) == 0 {
		return []string{}
	}
	order := map[string]int{"minimal": 0, "low": 1, "medium": 2, "high": 3, "max": 4}
	sort.Slice(out, func(i, j int) bool { return order[out[i]] < order[out[j]] })
	return out
}

// InferReasoning 按模型名猜是否支持思考。
//
// 这是启发式规则：上游没有显式声明能力时才用它。猜「支持」的代价很轻 ——
// 前端只是把思考强度下拉放开，默认那项是「不发参数」，不下发就等同于上游默认
// 行为；而猜「不支持」会让用户彻底失去调节能力（比如 deepseek-flash 和
// deepseek-v4-pro 都是默认就会思考的，却被名字骗过去了）。
// 万一上游不认识 reasoning_effort，客户端还会自动去掉该参数重试一次。
func InferReasoning(modelID string) bool {
	s := strings.ToLower(modelID)
	for _, p := range []string{
		"reasoner", "reasoning", "thinking", "-think", "qwq", "magistral",
		"gpt-5", "gpt-6", "glm-4.5", "glm-4.6", "grok-4", "grok-3-mini",
		"gemini-2.5", "gemini-3", "claude-3-7", "claude-3.7",
		"sonnet-4", "opus-4", "haiku-4", "minimax-m2", "deepseek-v3.1", "deepseek-v3.2",
		// DeepSeek 当前在架、且实测默认就会吐 reasoning_content 的型号
		"deepseek-flash", "deepseek-v4",
		// kimi-k3：始终推理（无 thinking 开关），但有 reasoning_effort 档位（low/high/max）
		"kimi-k3", "kimi-for-coding",
		"qwen3", "kimi-k2-thinking", "ernie-x1", "step-3", "hunyuan-t1",
	} {
		if strings.Contains(s, p) {
			return true
		}
	}
	// 短 token 必须整段匹配，否则 "o1" 会误伤一大堆名字
	for _, t := range strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		switch t {
		case "o1", "o3", "o4", "r1", "k3":
			return true
		}
	}
	return false
}
