package session

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"aiassistant/internal/llm"
	"aiassistant/internal/store"
)

// 起标题用的提示词。
// 刻意不依赖 system 消息（个别网关对 system 位置敏感），全部塞进一条 user 消息里；
// 也刻意不用流式 —— 这是一次性的小请求，不需要逐字。
const titleInstruction = `请给下面这段对话起一个标题。

要求：
- 不超过 15 个字，要具体，能一眼看出这段对话在讲什么
- 只输出标题本身：不要引号、不要书名号、不要结尾标点、不要任何解释或前缀
- 用对话所用的语言

对话内容：
用户：%s
助手：%s

标题：`

// titleTimeout 是起标题请求的超时。它跑在后台，超时就放弃（保留本地截断的标题）。
const titleTimeout = 45 * time.Second

// titleSnippet 用来截断喂给模型的对话片段，避免把一整篇回答都塞进去。
const titleSnippet = 600

const titleMaxRunes = 20

// Titler 异步让模型给会话起个好标题。
//
// 为什么要异步：起标题是一次额外的上游请求，可能要几秒。它绝不能挡住
// 生成收尾、更不能占着会话 goroutine 的 mailbox。所以流程是——
// 先用本地截断秒出一个能看的标题，再由这里在后台覆盖成更好的。
type Titler struct {
	st           Store
	bus          *Bus
	isGenerating func(convID int64) bool

	mu      sync.Mutex
	running map[int64]bool
}

func NewTitler(st Store, bus *Bus, isGenerating func(int64) bool) *Titler {
	return &Titler{st: st, bus: bus, isGenerating: isGenerating, running: map[int64]bool{}}
}

// Generate 在后台起标题。
//
// expectedTitle 是「此刻的标题」（本地截断出来的那个）。请求回来时如果标题已经
// 不是它了，说明用户手动改过名，直接放弃覆盖。
func (t *Titler) Generate(convID int64, expectedTitle string, cli *llm.Client, model, userMsg, reply string) {
	if cli == nil || model == "" || strings.TrimSpace(userMsg) == "" {
		return
	}

	t.mu.Lock()
	if t.running[convID] {
		t.mu.Unlock()
		return
	}
	t.running[convID] = true
	t.mu.Unlock()

	go func() {
		defer func() {
			t.mu.Lock()
			delete(t.running, convID)
			t.mu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), titleTimeout)
		defer cancel()
		start := time.Now()

		prompt := strings.Replace(titleInstruction,
			"%s", truncateRunes(strings.TrimSpace(userMsg), titleSnippet), 1)
		prompt = strings.Replace(prompt,
			"%s", truncateRunes(strings.TrimSpace(reply), titleSnippet), 1)

		raw, usage, err := cli.Complete(ctx, model, []llm.ChatMessage{{Role: "user", Content: prompt}}, 512, "none")
		elapsed := time.Since(start)
		if err != nil {
			// 失败就保留本地截断的标题，不打扰用户
			log.Printf("[title] 会话 %d 起标题失败（保留本地标题）: %v", convID, err)
			return
		}
		// 起标题也是一次真金白银的上游请求，用量一样打在日志里。
		// 注意它刻意用 reasoning_effort=none：不需要思考，也就几乎不花 token。
		log.Printf("[title] 会话 %d 起标题  model=%s  思考强度=none(显式关闭)  用时=%.1fs  "+
			"tokens: prompt=%d completion=%d(思考 %d)  缓存: 命中=%d 未命中=%d",
			convID, model, elapsed.Seconds(),
			usage.PromptTokens, usage.CompletionTokens, usage.ReasoningTokens,
			usage.CacheHitTokens, usage.CacheMissTokens)

		title := sanitizeTitle(raw)
		if title == "" {
			log.Printf("[title] 会话 %d 的标题清洗后为空，保留本地标题", convID)
			return
		}

		conv, err := t.st.GetConversation(convID)
		if err != nil || conv == nil {
			return
		}
		if conv.Title != expectedTitle {
			return // 期间被改过名，不覆盖
		}
		if title == conv.Title {
			return
		}

		if _, err := t.st.UpdateConversation(convID, store.ConversationPatch{Title: &title}); err != nil {
			log.Printf("[title] 会话 %d 写入标题失败: %v", convID, err)
			return
		}
		updated, err := t.st.GetConversation(convID)
		if err != nil || updated == nil {
			return
		}
		// Generating 是从库里读不出来的（它不是数据库字段），按会话真实状态补上，
		// 否则前端会把这个正在生成的会话的转圈标记误清掉。
		updated.Generating = t.isGenerating != nil && t.isGenerating(convID)
		log.Printf("[title] 会话 %d 标题已生成: %q", convID, title)
		t.bus.Publish(EventConv, conv.UserID, func(seq int64) any {
			return ConvPayload{Seq: seq, ConvID: convID, Action: ActionUpdated, Conv: updated}
		})
	}()
}

// sanitizeTitle 把模型爱加的那些零碎收拾干净。
// 模型经常不听话：加引号、写「标题：」、先来一句解释、结尾带句号。
func sanitizeTitle(raw string) string {
	s := strings.TrimSpace(raw)
	// 只取第一行（有的模型会输出「标题：xxx\n\n理由：yyy」）
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)

	// 去掉常见前缀
	for _, p := range []string{"标题：", "标题:", "Title:", "title:", "标题是", "#"} {
		if strings.HasPrefix(s, p) {
			s = strings.TrimSpace(strings.TrimPrefix(s, p))
		}
	}
	// 去掉包裹的引号 / 书名号
	s = strings.Trim(s, "「」『』“”‘’\"'《》〈〉[]【】")
	s = strings.TrimSpace(s)

	// 模型有时候先说一句「这段对话讲的是…」，取第一个句号前的内容。
	// 注意 IndexAny 给的是**字节**下标，中文一个字 3 字节，必须换成字符数再比，
	// 否则「20 个字以内才截」这个条件对中文永远不成立。
	if i := strings.IndexAny(s, "。！？!?"); i > 0 {
		if prefix := []rune(s[:i]); len(prefix) <= titleMaxRunes {
			s = string(prefix)
		}
	}
	s = strings.TrimSpace(s)

	return truncateRunes(s, titleMaxRunes)
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
