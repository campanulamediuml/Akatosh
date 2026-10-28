package store

import (
	"encoding/json"

	"aiassistant/internal/llm"
)

// ToolRoundsPayload 是 messages.tool_rounds 列的 JSON 结构。
type ToolRoundsPayload struct {
	// Rounds 是本轮的中间段：assistant(tool_calls，带该段 reasoning) + tool 结果。
	// 不含最后一段——最后一段就是消息本身（content/reasoning 列），回放时从
	// 消息行现取，这样用户的编辑（正文/CoT）才能生效。
	Rounds []llm.ChatMessage `json:"rounds"`
	// FinalReasoning 是最后一段流的 reasoning。消息行的 reasoning 列是全段
	// 拼接（展示用），回放正文消息时用这一段才不重复。
	FinalReasoning string `json:"final_reasoning"`
	// SegUsages 是各段流各自的 usage（段 = 一次上游请求；工具循环里一轮一段，
	// 最后一个元素是写正文那段）。消息行的 usage 是成本口径的**累加值**，
	// 分段值用来给前端拆开展示（「这一段请求实际多大」）。
	SegUsages []Usage `json:"seg_usages,omitempty"`
}

// ParseToolRounds 解析 tool_rounds 列；空串/坏数据返回 nil。
func ParseToolRounds(s string) *ToolRoundsPayload {
	if s == "" {
		return nil
	}
	var p ToolRoundsPayload
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return nil
	}
	return &p
}

// Usage 记录一次生成的 token 用量。上游不返回时全为 0。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// 前缀缓存：命中部分通常便宜一个数量级，所以单独记下来。
	CacheHitTokens  int `json:"cache_hit_tokens"`
	CacheMissTokens int `json:"cache_miss_tokens"`
}

// Provider 是一个 LLM 供应商配置。一个部署可以配任意多个。
// APIKey 用 json:"-" 保证永远不会被序列化到任何响应里。
type Provider struct {
	ID int64 `json:"id"`
	// UserID 属主：apikey 完全隔离，每个用户只能看到/使用自己的供应商
	UserID    int64  `json:"-"`
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"-"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`

	// ModelCount 不是数据库字段，由列表查询顺带算出来。
	ModelCount int `json:"model_count"`
}

// Model 是从上游 /models 拉取后缓存在本地的模型条目。
type Model struct {
	ID          int64    `json:"id"`
	ProviderID  int64    `json:"provider_id"`
	ModelID     string   `json:"model_id"`
	DisplayName string   `json:"display_name"`
	Reasoning   bool     `json:"reasoning"`
	Efforts     []string `json:"efforts"`
	ContextLen  int      `json:"context_len"`
	FetchedAt   int64    `json:"fetched_at"`
}

type Conversation struct {
	ID int64 `json:"id"`
	// UserID 属主：多租户隔离的边界，所有读写路径都必须按它过滤/校验
	UserID int64  `json:"-"`
	Title  string `json:"title"`
	Effort string `json:"effort"`
	// 会话绑定到具体供应商：provider_id + model_id 一起决定发给谁。
	// 两个网关都有 gpt-4o 时靠 provider_id 区分。
	ProviderID   int64  `json:"provider_id"`
	ProviderName string `json:"provider_name"` // 只读冗余字段，方便界面直接显示
	ModelID      string `json:"model_id"`
	SystemPrompt string `json:"system_prompt"`
	// 联网搜索开关：开启后生成时可由模型自主调用 web_search 工具（function calling）。
	WebSearch bool `json:"web_search"`
	// 上下文压缩：Summary 是旧消息的压缩摘要，CompactedBefore 是摘要覆盖到的
	// 消息 id（含）。更早的消息已被冻结（内容折进摘要，只读）。
	Summary         string `json:"summary"`
	CompactedBefore int64  `json:"compacted_before"`
	// ContextEstimate 是压缩后对下一次请求 prompt tokens 的估算；0 = 使用上游实测值。
	ContextEstimate int `json:"context_estimate"`
	// Compacting 不是数据库列：该会话此刻是否正在压缩（压缩期间阻塞发送）。
	Compacting   bool  `json:"compacting"`
	Pinned       bool  `json:"pinned"`
	Archived     bool  `json:"archived"`
	MessageCount int   `json:"message_count"`
	CreatedAt    int64 `json:"created_at"`
	UpdatedAt    int64 `json:"updated_at"`

	// LastReadAt 是「最近一次被查看」的时间戳。
	// 用户切到这个会话、或它正在被看着时有回答落地，都会刷新它。
	LastReadAt int64 `json:"last_read_at"`

	// Unread 不是数据库字段，按「有没有比你上次读更新的已完成回答」算出来。
	// **只统计已经答完的**（finished_at > 0），所以正在生成时不会亮红点，
	// 答完那一刻才亮 —— 这正是「提示可以看了」的含义。
	Unread bool `json:"unread"`

	// Generating 不是数据库字段，是列表接口按「有没有 streaming 消息」算出来的，
	// 用于让左侧列表显示「这个会话正在生成」。
	Generating bool `json:"generating"`
}

type Message struct {
	ID              int64  `json:"id"`
	ConvID          int64  `json:"conv_id"`
	Role            string `json:"role"`
	Status          string `json:"status"`
	Content         string `json:"content"`
	Reasoning       string `json:"reasoning"`
	ContentOriginal string `json:"content_original"`
	// ReasoningOriginal 非空 = CoT 被编辑过（存的是原文，可一键还原）。
	// 编辑后的 reasoning 会随历史回传给上游（DeepSeek 思考模式的规则），
	// 这就是「改 CoT 能引导模型后续思路」的闭环。
	ReasoningOriginal string `json:"reasoning_original"`
	Edited            bool   `json:"edited"`
	// 这条消息实际是发给哪家供应商的。
	// 多供应商下两个网关可能有同名模型（都叫 gpt-4o），只记 model_id 是有歧义的。
	ProviderID int64  `json:"provider_id"`
	ModelID    string `json:"model_id"`
	Effort     string `json:"effort"`
	Error      string `json:"error"`
	// 本轮联网搜索的记录（JSON：[{query, results:[{title,url}]}...]），展示用；
	// 空串 = 这轮没搜。tool 交互本身不落库，只留这份摘要。
	SearchLog string `json:"search_log"`
	// ToolRounds 是本轮完整工具交互（JSON：assistant tool_calls 段 + tool 结果段），
	// 跨轮全量回放给上游用；空串 = 老消息/没调工具。只面向模型，不给前端（可能很大）。
	ToolRounds string `json:"-"`
	// SegUsages 是本轮各段流的 usage（从 ToolRounds 解析出来给前端的：
	// 工具轮卡片按段展示 token，把「累计值」拆成有出处的几份）。
	SegUsages  []Usage `json:"seg_usages,omitempty"`
	Usage      Usage   `json:"usage"`
	CreatedAt  int64   `json:"created_at"`
	FinishedAt int64   `json:"finished_at"`

	// Attachments 不是数据库列，是查出来顺手填上的（图片等附件）。
	Attachments []Attachment `json:"attachments,omitempty"`
	// Files 不是数据库列，是查出来顺手填上的（上传的文档）。
	Files []File `json:"files,omitempty"`
}

// 消息状态
const (
	StatusStreaming = "streaming"
	StatusDone      = "done"
	StatusError     = "error"
	StatusAborted   = "aborted"
)

// 角色
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// 设置项 key
const (
	SettingDefaultProvider     = "default_provider_id"
	SettingDefaultModel        = "default_model_id"
	SettingDefaultEffort       = "default_effort"
	SettingDefaultSystemPrompt = "default_system_prompt"
)

// 默认值
const (
	DefaultTitle     = "新对话"
	DefaultProviderN = "默认供应商"
	DefaultEffort    = "medium"
)
