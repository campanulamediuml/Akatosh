package session

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"aiassistant/internal/llm"
	"aiassistant/internal/store"
)

// 上下文压缩（compact）。
//
// 触发时机：手动——session 窗口右上角的「压缩当前上下文」按钮（cmdCompact）。
// 曾经做过按阈值自动触发，后来改成手动：工具轮的 prompt_tokens 是多段流累加
// （成本口径），拿它当触发条件会虚高误触发；用户自己判断时机更可靠。
// 纪律不变：必须在没有生成在跑的边界上进行（handleCompact 的 busy 检查，
// 绝不在 tool 循环中）；压缩期间该会话阻塞新消息（别的会话不受影响）。
//
// 做什么：把 keepRecentMessages 条之前的历史（外加滚动合并上一次摘要）交给
// 当前会话的模型压成一份摘要，写入 conversations.summary / compacted_before；
// 之后 buildHistory 的历史 = 用户 system prompt + 摘要 + 近期消息。
//
// 纪律：用户手动设置的 system_prompt 是「宪法」——永远在历史最前面、
// 不参与压缩、不被摘要替代（这是用户的明确要求，写死在这里）。
//
// 冻结：id <= compacted_before 的消息只读（编辑/还原/删除/重新生成一律拒绝），
// 否则改旧消息会造成「摘要与原文不一致」。

// keepRecentMessages 是压缩时保留的最近消息条数（不参与压缩）。
const keepRecentMessages = 10

// compactInputRunes 是喂给摘要模型的文本上限（超出部分从最早的开始丢弃——
// 它们本来就是要被压缩掉的最老内容；滚动压缩会把信息折进摘要，不会真丢）。
const compactInputRunes = 60000

// compactRatio 是目标压缩比：摘要长度约为输入的 1/4（用户要求 4:1，
// 保证质量和长度可靠——太短会丢信息，太长失去压缩意义）。
const compactRatio = 4

// compactPromptTmpl 是摘要生成的指令（%d 为目标字数，按 4:1 压缩比算出）。
const compactPromptTmpl = `以下是一段对话的早期历史（可能附带之前的摘要）。请把它压缩成约 %d 字的摘要（原文的 1/4），供后续对话作为背景。要求：
- 保留：用户的真实需求与偏好、关键结论、重要事实、做过的决定、未完成的待办
- 丢弃：寒暄、过程性讨论、已被后续覆盖的过时结论
- 已有摘要要与新内容合并，不是覆盖
- 用第三人称客观陈述（"用户想要…"、"AI 给出了…"）

对话历史：
`

// compactTimeout 是摘要生成的超时。输入上限 6 万字符 + 最长 6000 tokens 输出，
// 慢模型 120s 可能不够（线上就超时失败过一次），放宽到 8 分钟。
const compactTimeout = 8 * time.Minute

// notify 给前端发一条压缩结果通知（独立 goroutine，只能用线程安全的 bus）。
// 压缩是用户手动点的按钮，每个结局都必须有反馈——成功/失败/没有可压的内容，
// 否则用户看到的永远是「开始压缩…」然后什么都没发生。
func (s *Session) notifyCompact(level, text string) {
	s.mgr.bus.Publish(EventNotice, s.ownerID(), func(seq int64) any {
		return NoticePayload{Seq: seq, ConvID: s.convID, Level: level, Text: text}
	})
}

const (
	compactRunesPerToken   = 4
	compactMessageOverhead = 4
)

func compactTextTokens(text string) int {
	return (utf8.RuneCountInString(text) + compactRunesPerToken - 1) / compactRunesPerToken
}

func compactMessageTokens(m store.Message) int {
	return compactMessageOverhead + compactTextTokens(m.Content) + compactTextTokens(m.Reasoning) + compactTextTokens(m.ToolRounds)
}

func compactSummaryTokens(summary string) int {
	if strings.TrimSpace(summary) == "" {
		return 0
	}
	return compactMessageOverhead + compactTextTokens(summary)
}

// estimateCompactedContext 用最近一次上游实测 prompt 为锚点，只估算本次压缩的增量。
// 没有锚点时才对摘要、system prompt 和保留消息做完整的本地估算。
func estimateCompactedContext(conv *store.Conversation, msgs []store.Message, beforeID int64, removedTokens int, summary string) int {
	anchor := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == store.RoleAssistant && m.Status != store.StatusStreaming && m.Usage.PromptTokens > 0 {
			anchor = m.Usage.PromptTokens
			break
		}
	}
	if anchor > 0 {
		estimate := anchor - removedTokens + compactSummaryTokens(summary)
		if estimate < 0 {
			return 0
		}
		return estimate
	}

	estimate := compactSummaryTokens(summary) + compactTextTokens(conv.SystemPrompt) + compactTextTokens(currentDatePrompt())
	for _, m := range msgs {
		if m.ID > beforeID && m.Status != store.StatusStreaming && m.Status != store.StatusError {
			estimate += compactMessageTokens(m)
		}
	}
	return estimate
}

// runCompact 执行一次压缩。所有结局都会广播通知（成功/失败/无需压缩），
// 日志同步留底。compacting 状态的置位/解除由调用方（handleCompact）负责。
func (s *Session) runCompact() {
	ctx, cancel := context.WithTimeout(context.Background(), compactTimeout)
	defer cancel()

	conv, err := s.mgr.st.GetConversation(s.convID)
	if err != nil || conv == nil {
		return // 会话没了：没人会看通知，只留日志
	}
	msgs, err := s.mgr.st.ListMessages(s.convID)
	if err != nil {
		log.Printf("[compact] 会话 %d 读取消息失败: %v", s.convID, err)
		s.notifyCompact("error", "压缩失败：读取消息出错")
		return
	}
	if len(msgs) <= keepRecentMessages+2 {
		s.notifyCompact("info", "消息还太少，不需要压缩")
		return
	}

	old := msgs[:len(msgs)-keepRecentMessages]
	// 跳过已经压过的部分：新一轮只处理 (compacted_before, 新边界] 的消息
	fresh := old
	if conv.CompactedBefore > 0 {
		fresh = nil
		for _, m := range old {
			if m.ID > conv.CompactedBefore {
				fresh = append(fresh, m)
			}
		}
	}
	if len(fresh) == 0 {
		s.notifyCompact("info", "没有新内容需要压缩（较早的历史已经压成摘要了）")
		return
	}

	// 组装摘要输入：旧摘要（滚动合并）+ 新增旧消息的 content（不要 reasoning——
	// 摘要要的是内容，CoT 折进去只会浪费）。用户 system prompt 不参与（宪法）。
	var sb strings.Builder
	if conv.Summary != "" {
		sb.WriteString("【之前的摘要】\n" + conv.Summary + "\n\n【之后的对话】\n")
	}
	runes := 0
	included := 0
	beforeID := int64(0)
	removedTokens := compactSummaryTokens(conv.Summary)
	for _, m := range fresh {
		if m.Status != store.StatusDone || strings.TrimSpace(m.Content) == "" {
			continue
		}
		line := fmt.Sprintf("%s: %s\n", m.Role, m.Content)
		n := utf8.RuneCountInString(line)
		// 超输入上限就停在这里：摘要覆盖范围（compacted_before）必须以**实际进了
		// 摘要的最后一条**为准。否则没进摘要的消息也被冻结、被历史排除——
		// 内容既不在摘要里也不在上下文里，就静默丢了（至少保留一条，防单条超巨卡死）
		if included > 0 && runes+n > compactInputRunes {
			break
		}
		runes += n
		sb.WriteString(line)
		included++
		beforeID = m.ID
		removedTokens += compactMessageTokens(m)
	}
	// 少于 2 条实际内容不值得压（防生成失败后反复触发琐碎压缩）；
	// 它们会留到下次一起压（没被冻结，历史里仍在）
	if beforeID == 0 || included < 2 {
		s.notifyCompact("info", "新增的有效内容太少，留到下次一起压")
		return
	}

	// 目标长度：输入的 1/4（4:1 压缩比），上下限保护
	target := runes / compactRatio
	if target < 300 {
		target = 300
	}
	if target > 3000 {
		target = 3000
	}

	cli, err := s.mgr.clientFor(conv.UserID, conv.ProviderID)
	if err != nil {
		log.Printf("[compact] 会话 %d 构造上游客户端失败: %v", s.convID, err)
		s.notifyCompact("error", "压缩失败：构造模型客户端出错")
		return
	}
	summary, usage, err := cli.Complete(ctx, conv.ModelID, []llm.ChatMessage{
		{Role: store.RoleUser, Content: fmt.Sprintf(compactPromptTmpl, target) + sb.String()},
	}, target*2, conv.Effort)
	if err != nil {
		log.Printf("[compact] 会话 %d 摘要生成失败: %v", s.convID, err)
		if ctx.Err() == context.DeadlineExceeded {
			s.notifyCompact("error", "压缩失败：生成摘要超时（历史太长或上游太慢），稍后再试")
		} else {
			s.notifyCompact("error", "压缩失败：生成摘要出错（"+err.Error()+"）")
		}
		return
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		log.Printf("[compact] 会话 %d 摘要为空，跳过", s.convID)
		s.notifyCompact("error", "压缩失败：模型返回了空摘要")
		return
	}
	contextEstimate := estimateCompactedContext(conv, msgs, beforeID, removedTokens, summary)
	if err := s.mgr.st.SetConversationSummary(s.convID, summary, beforeID, contextEstimate); err != nil {
		log.Printf("[compact] 会话 %d 写入摘要失败: %v", s.convID, err)
		s.notifyCompact("error", "压缩失败：写入摘要出错")
		return
	}
	log.Printf("[compact] 会话 %d 已压缩：%d 字 → %d 字摘要（冻结至 #%d，消耗 %d tokens）",
		s.convID, runes, utf8.RuneCountInString(summary), beforeID, usage.TotalTokens)

	// 广播会话更新与提示。注意这是独立 goroutine：会话 goroutine 独占的字段
	// （s.conv / s.lastSeq / refreshConv / publishConv）一律不能碰，
	// 只能用线程安全的 store 和 bus。
	s.mgr.bus.Publish(EventConv, s.ownerID(), func(seq int64) any {
		conv, err := s.mgr.st.GetConversation(s.convID)
		if err != nil || conv == nil {
			return ConvPayload{Seq: seq, ConvID: s.convID, Action: ActionUpdated}
		}
		return ConvPayload{Seq: seq, ConvID: s.convID, Action: ActionUpdated, Conv: conv}
	})
	s.mgr.bus.Publish(EventNotice, s.ownerID(), func(seq int64) any {
		return NoticePayload{Seq: seq, ConvID: s.convID, Level: "info",
			Text: "上下文已压缩：较早的消息已归档为摘要（只读），对话照常继续"}
	})
}
