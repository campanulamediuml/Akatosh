package session

import (
	"strings"
	"testing"

	"aiassistant/internal/imagegen"
	"aiassistant/internal/llm"
	"aiassistant/internal/store"
)

// 生图历史重放成结构化工具轮：assistant(tool_calls) + tool 回执，
// 不再用「系统注」文本标注（那种格式模型会学去假装调用工具）。
func TestImageToolRounds(t *testing.T) {
	log := `[
		{"type":"search","query":"猫","results":[{"title":"t","url":"u"}]},
		{"type":"image","prompt":"a cat on windowsill"},
		{"type":"image","prompt":"a dog","error":"生成图片失败：超时"}
	]`
	atts := []store.Attachment{{ID: 65}}

	rounds := imageToolRounds(log, atts, "当时的思考", nil)
	if len(rounds) != 4 {
		t.Fatalf("两条 image 记录应重放成 4 条消息（各 assistant+tool），实际 %d", len(rounds))
	}

	// 第一条：成功的生图 → 回执带 attachment_id
	a1, r1 := rounds[0], rounds[1]
	if a1.Role != store.RoleAssistant || len(a1.ToolCalls) != 1 {
		t.Fatalf("第一条应是带 tool_calls 的 assistant: %+v", a1)
	}
	if a1.ToolCalls[0].Function.Name != imagegen.ToolName {
		t.Fatalf("工具名应是 generate_image: %q", a1.ToolCalls[0].Function.Name)
	}
	if !strings.Contains(a1.ToolCalls[0].Function.Arguments, "a cat on windowsill") {
		t.Fatalf("arguments 应带 prompt: %q", a1.ToolCalls[0].Function.Arguments)
	}
	if a1.Reasoning != "当时的思考" {
		t.Fatalf("reasoning 应挂在第一条重放消息上（上游硬规则）: %q", a1.Reasoning)
	}
	if r1.Role != "tool" || r1.ToolCallID != a1.ToolCalls[0].ID {
		t.Fatalf("应为成对的 tool 回执: %+v", r1)
	}
	s1, _ := r1.Content.(string)
	if !strings.Contains(s1, `"attachment_id": 65`) {
		t.Fatalf("回执应带配对上的附件 id: %q", s1)
	}

	// 第二条：失败的生图 → 错误回执，不占附件
	a2, r2 := rounds[2], rounds[3]
	if a2.Reasoning != "" {
		t.Fatalf("reasoning 只挂第一条: %q", a2.Reasoning)
	}
	s2, _ := r2.Content.(string)
	if !strings.Contains(s2, "超时") {
		t.Fatalf("失败回执应带错误信息: %q", s2)
	}
}

// 旧记录是截断落库的（末尾带「…」）：回放时剥掉省略号，别传染给新调用。
func TestImageToolRoundsStripsLegacyTruncationMark(t *testing.T) {
	log := `[{"type":"image","prompt":"a very long prompt cut off mid-wor…"}]`
	rounds := imageToolRounds(log, nil, "", nil)
	if len(rounds) != 2 {
		t.Fatalf("应重放一轮，实际 %d 条", len(rounds))
	}
	args := rounds[0].ToolCalls[0].Function.Arguments
	if strings.Contains(args, "…") {
		t.Fatalf("回放的 arguments 不应带截断省略号: %q", args)
	}
	if !strings.Contains(args, "mid-wor") {
		t.Fatalf("prompt 正文应保留: %q", args)
	}
}

// 附件比 image 记录多（老数据只有附件没有记录）：补成回放轮，prompt 不可考。
func TestImageToolRoundsOrphanAttachment(t *testing.T) {
	rounds := imageToolRounds("", []store.Attachment{{ID: 7}}, "", nil)
	if len(rounds) != 2 {
		t.Fatalf("孤儿附件应补一轮回放，实际 %d 条", len(rounds))
	}
	if !strings.Contains(rounds[0].ToolCalls[0].Function.Arguments, "prompt 未记录") {
		t.Fatalf("prompt 不可考应有占位说明: %q", rounds[0].ToolCalls[0].Function.Arguments)
	}
	s, _ := rounds[1].Content.(string)
	if !strings.Contains(s, `"attachment_id": 7`) {
		t.Fatalf("回执应带附件 id: %q", s)
	}
}

// 工具调用摘要不再包含 image 条目（它们走结构化回放，文本摘要会重复且可被模仿）。
func TestToolLogSummarySkipsImage(t *testing.T) {
	log := `[
		{"type":"image","prompt":"a cat"},
		{"type":"search","query":"天气","results":[{"title":"预报","url":"u"}]}
	]`
	sum := formatToolLogSummary(log)
	if strings.Contains(sum, "生成了图片") || strings.Contains(sum, "a cat") {
		t.Fatalf("摘要不应再包含 image 条目: %q", sum)
	}
	if !strings.Contains(sum, "天气") {
		t.Fatalf("search 条目应保留: %q", sum)
	}
}

func TestReasoningPolicyWithToolsKeepsFullHistory(t *testing.T) {
	long := strings.Repeat("想", 40000)
	call := llm.ToolCall{ID: "c1", Type: "function"}
	call.Function.Name = imagegen.ToolName
	msgs := []llm.ChatMessage{
		{Role: store.RoleAssistant, Content: "旧回复", Reasoning: long},
		{Role: store.RoleAssistant, Content: "", Reasoning: "工具轮的思考", ToolCalls: []llm.ToolCall{call}},
		{Role: store.RoleAssistant, Content: "新回复", Reasoning: "最近的思考"},
	}
	applyReasoningPolicy(msgs, true)
	for i, want := range []string{long, "工具轮的思考", "最近的思考"} {
		if msgs[i].Reasoning != want {
			t.Fatalf("带 tools 时第 %d 条 reasoning 应完整保留", i)
		}
	}
}

func TestReasoningPolicyWithoutToolsDropsHistory(t *testing.T) {
	msgs := []llm.ChatMessage{
		{Role: store.RoleAssistant, Content: "旧回复", Reasoning: "旧思考"},
		{Role: store.RoleAssistant, Content: "新回复", Reasoning: "最近的思考"},
	}
	applyReasoningPolicy(msgs, false)
	for i := range msgs {
		if msgs[i].Reasoning != "" {
			t.Fatalf("不带 tools 时第 %d 条 reasoning 不应回传", i)
		}
		if msgs[i].Content == "" {
			t.Fatalf("第 %d 条正文不应受 reasoning 策略影响", i)
		}
	}
}

// 生图回执带图的模型判定：DeepSeek/Kimi 实测支持，OpenAI 系静默丢图要排除。
func TestSupportsImageToolReceipt(t *testing.T) {
	yes := []string{"deepseek-v4.1-flash", "deepseek-flash", "kimi-k3", "kimi-k2.6", "k3", "k3-256k", "kimi-for-coding", "moonshot-v1"}
	no := []string{"gpt-5.5", "gpt-4o", "codex-auto-review", "kkai-5.5", "claude-sonnet"}
	for _, m := range yes {
		if !supportsImageToolReceipt(m) {
			t.Errorf("%s 应判定为支持", m)
		}
	}
	for _, m := range no {
		if supportsImageToolReceipt(m) {
			t.Errorf("%s 应判定为不支持", m)
		}
	}
}

// 回放带图：resolver 给图 → 回执是多段（文本+image_url）且带 attachment_id；
// resolver 给空 → 退回纯文本回执。
func TestImageToolRoundsWithImageParts(t *testing.T) {
	log := `[{"type":"image","prompt":"p1"},{"type":"image","prompt":"p2"}]`
	atts := []store.Attachment{{ID: 1}, {ID: 2}}
	resolver := func(attID int64) string {
		if attID == 1 {
			return "" // 读失败：退回文本回执
		}
		return "data:image/jpeg;base64,FULL"
	}
	rounds := imageToolRounds(log, atts, "", resolver)
	if len(rounds) != 4 {
		t.Fatalf("应重放 4 条，实际 %d", len(rounds))
	}

	// 第一张（resolver 给空）：纯文本回执
	s1, ok := rounds[1].Content.(string)
	if !ok || !strings.Contains(s1, `"attachment_id": 1`) {
		t.Fatalf("读失败的回执应是纯文本并带 attachment_id: %#v", rounds[1].Content)
	}

	// 第二张（有图）：多段 content + 带图回执文案
	parts2, ok := rounds[3].Content.([]llm.ContentPart)
	if !ok || len(parts2) != 2 {
		t.Fatalf("带图回执应是两段 content: %#v", rounds[3].Content)
	}
	if parts2[1].Type != "image_url" || parts2[1].ImageURL.URL != "data:image/jpeg;base64,FULL" {
		t.Fatalf("图片段不对: %+v", parts2[1])
	}
	if !strings.Contains(parts2[0].Text, "自检") || !strings.Contains(parts2[0].Text, `"attachment_id": 2`) {
		t.Fatalf("带图回执应用带图文案并带 attachment_id: %q", parts2[0].Text)
	}
}
