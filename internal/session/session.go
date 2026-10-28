package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"aiassistant/internal/imagegen"
	"aiassistant/internal/llm"
	"aiassistant/internal/sandbox"
	"aiassistant/internal/search"
	"aiassistant/internal/store"
)

const (
	// flushInterval 是流式内容落库的节流间隔。
	// 每个 token 写一次库会把 SQLite 打爆；250ms 一次既能保证刷新页面看到
	// 几乎实时（并且 state 接口还会补上内存实时值，见 state()），
	// 又只产生每秒 4 次小写入。
	flushInterval = 250 * time.Millisecond

	// idleTimeout：空闲这么久且没有在生成，会话 goroutine 就自行退出。
	// 状态本来就在 SQLite 里，下一条命令会把它原样重建，
	// 所以「一个会话一个 goroutine」不会随着会话数无上限地堆积。
	idleTimeout = 5 * time.Minute
	idleCheck   = 20 * time.Second
)

// Generating 描述 state 里「正在生成」的这段信息。
type Generating struct {
	MessageID    int64  `json:"message_id"`
	ProviderID   int64  `json:"provider_id"`
	ModelID      string `json:"model_id"`
	Effort       string `json:"effort"`
	StartedAt    int64  `json:"started_at"`
	ReasoningLen int    `json:"reasoning_len"`
	ContentLen   int    `json:"content_len"`
}

// State 是一个会话的完整权威状态。前端拿它做全量重建。
// Messages 只含最近若干条（分页首屏），HasMore 标记是否还有更早的可翻。
type State struct {
	Conv       *store.Conversation `json:"conv"`
	Seq        int64               `json:"seq"`
	Generating *Generating         `json:"generating"`
	Messages   []store.Message     `json:"messages"`
	HasMore    bool                `json:"has_more"`
}

// Error 是带错误码的领域错误，HTTP 层直接按 Code 映射状态码。
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

var errNotFound = NewError("not_found", "会话或消息不存在")

// Session 是一个会话的 actor：一条 goroutine 独占它的全部可变状态。
// 命令通过 mailbox 串行进入，所以下面的字段完全不需要加锁。
type Session struct {
	convID  int64
	mailbox chan command
	done    chan struct{}
	mgr     *Manager

	// ---- 以下字段只由 run() 这条 goroutine 读写 ----
	conv     *store.Conversation
	gen      *generation
	lastSeq  int64
	lastUsed time.Time
}

// generation 是一次正在进行的生成。
//
// 联网搜索（function calling）时的「轮」：模型可能先返回 tool_calls（第 1 段流），
// 后端执行搜索、把结果追加进 history 后再发起下一段流，如此循环最多 maxSearchRounds 次。
// tool 交互的 assistant/tool 消息在收尾时落库到消息的 tool_rounds（跨轮全量回放用）；
// 展示层另有一份 searchLog 摘要。
type generation struct {
	msgID      int64
	providerID int64
	modelID    string
	effort     string
	startedAt  int64

	reasoning strings.Builder
	content   strings.Builder
	// tool 循环按段切分 CoT/正文：续传时 assistant 消息要带「那一段」的
	// reasoning_content（DeepSeek 思考模式 tool 循环强制要求回传）和 content，
	// 落库时把各段拼回完整内容。
	cotParts     []string
	contentParts []string

	// history 是本轮发给上游的完整消息列表：buildHistory 的产物 +
	// 循环中追加的 assistant(tool_calls) 与 role=tool 消息。
	// historyBase 标记「本轮新增部分」的起点（= 初始历史长度），
	// 收尾时 history[historyBase:] 就是要落库的 tool_rounds。
	history     []llm.ChatMessage
	historyBase int
	tools       []llm.Tool // 本轮声明的工具；达到调用上限后置空（强制模型写正文）

	toolRounds int   // 已执行的工具轮数
	toolLog    []any // 本轮工具调用摘要（search.Record / sandbox.Record），收尾时落库
	// segUsages 是各段流各自的 usage（每段一个元素，最后一个 = 写正文段）。
	// 收尾时随 tool_rounds 落库，前端按段拆开展示 token（累计值太吓人）。
	segUsages []store.Usage

	ch       <-chan llm.Event
	startRes chan streamStart
	toolRes  chan toolExecResult
	ctx      context.Context
	cancel   context.CancelFunc

	flushedR int
	flushedC int
	usage    store.Usage
}

// maxToolRounds 是单条回复允许的最大工具调用轮数（一轮可含多个并行调用）。
// 研究型任务（边搜边读边算）很容易用到 5-6 轮，3 轮会把模型逼到
// 把工具调用写成 DSML 文本标记吐进正文（实测抓到）；8 轮给够余量又防失控。
const maxToolRounds = 8

// toolPreamble 声明了工具的请求里注入的纪律提示（防「嘴硬」：没调用却声称调用过）。
const toolPreamble = "你可以使用工具。凡涉及搜索网页、执行代码、生成或回看图片的内容，必须实际调用对应工具并拿到结果后再回答；绝不允许在没有调用的情况下声称已经搜索过、执行过或生成过。历史里你之前生成的图片以 generate_image 工具调用和回执的形式出现，那才是你画过的图；图片生成成功后系统会自动把图展示给用户。"

// streamStart 是「发起上游请求」那个 goroutine 的返回值。
// 发请求是网络 IO，可能等待几百毫秒甚至超时，绝不能放在会话 goroutine 里
// 同步做 —— 否则这期间用户点「停止」都投递不进来。
type streamStart struct {
	gen          *generation
	ch           <-chan llm.Event
	toolsDropped bool // 上游不认 tools 参数、已去掉声明重试
	err          error
}

// toolExecResult 是「执行一轮工具调用」那个 goroutine 的回执。
type toolExecResult struct {
	gen     *generation
	entries []toolExecEntry
}

type toolExecEntry struct {
	call   llm.ToolCall
	output string // 给模型的 tool 消息内容（执行失败也写成错误 JSON，让模型自己圆场）
	// outputParts 非空时优先于 output：read_attachment 的回图是多段 content（实测 DeepSeek 支持）
	outputParts []llm.ContentPart
	record      any // 落库摘要：search.Record 或 sandbox.Record；nil = 不记录
}

func newSession(convID int64, mgr *Manager, conv *store.Conversation) *Session {
	return &Session{
		convID:   convID,
		mailbox:  make(chan command, 64),
		done:     make(chan struct{}),
		mgr:      mgr,
		conv:     conv,
		lastUsed: time.Now(),
	}
}

// run 是会话的主循环。所有状态变更都发生在这里，天然串行、无竞态。
func (s *Session) run() {
	defer close(s.done)

	flush := time.NewTicker(flushInterval)
	idle := time.NewTicker(idleCheck)
	defer flush.Stop()
	defer idle.Stop()

	for {
		// 没在生成时这两个 channel 是 nil，select 永远不会选中它们。
		var (
			genCh   <-chan llm.Event
			startCh <-chan streamStart
			toolCh  <-chan toolExecResult
		)
		if s.gen != nil {
			genCh = s.gen.ch
			startCh = s.gen.startRes
			toolCh = s.gen.toolRes
		}

		select {
		case cmd := <-s.mailbox:
			s.lastUsed = time.Now()
			if quit := s.handle(cmd); quit {
				s.mgr.drop(s.convID, s)
				return
			}

		case sr := <-startCh:
			s.onStreamStart(sr)

		case tr := <-toolCh:
			s.onToolResult(tr)

		case ev, ok := <-genCh:
			if !ok {
				// 上游把连接关了但没发 [DONE]
				s.finishGeneration(store.StatusDone, "")
			} else {
				s.onStreamEvent(ev)
			}

		case <-flush.C:
			s.flushIfDirty()

		case <-idle.C:
			// 生成中绝不回收；mailbox 里还有没处理的命令也不能回收
			if s.gen == nil && len(s.mailbox) == 0 && time.Since(s.lastUsed) > idleTimeout {
				s.mgr.drop(s.convID, s)
				return
			}
		}
	}
}

// FillMessagesAttachments 给一批消息填附件（含 thumb_url/raw_url）和关联文件。
// state、分页、实时事件与命令回执共用：关联存在另外的表里，
// 少了这一步，前端会把缺字段的更新当成完整消息，表现为刷新前附件消失。
func FillMessagesAttachments(st Store, msgs []store.Message) {
	ids := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	if attMap, err := st.AttachmentsByMessageIDs(ids); err == nil && len(attMap) > 0 {
		for i := range msgs {
			if atts, ok := attMap[msgs[i].ID]; ok {
				msgs[i].Attachments = atts
			}
		}
		// 补上 thumb_url / raw_url（数据库行里没有这两个字段）
		fillAttachmentURLs(msgs)
	}
	if fileMap, err := st.FilesByMessageIDs(ids); err == nil && len(fileMap) > 0 {
		for i := range msgs {
			if fs, ok := fileMap[msgs[i].ID]; ok {
				msgs[i].Files = fs
			}
		}
	}
}

// messageWithAttachments 返回出网用的完整消息，不修改调用方持有的对象。
func messageWithAttachments(st Store, m *store.Message) *store.Message {
	if m == nil {
		return nil
	}
	one := []store.Message{*m}
	FillMessagesAttachments(st, one)
	return &one[0]
}

// ---------- 命令分发 ----------

func (s *Session) handle(cmd command) (quit bool) {
	var (
		res result
		err error
	)
	switch cmd.kind {
	case cmdState:
		res, err = s.state()
	case cmdSend:
		res, err = s.handleSend(cmd)
	case cmdStop:
		res, err = s.handleStop()
	case cmdRegenerate:
		res, err = s.handleRegenerate(cmd)
	case cmdEditMessage:
		res, err = s.handleEdit(cmd)
	case cmdEditReasoning:
		res, err = s.handleEditReasoning(cmd)
	case cmdRestoreMessage:
		res, err = s.handleRestore(cmd)
	case cmdRestoreReasoning:
		res, err = s.handleRestoreReasoning(cmd)
	case cmdDeleteMessage:
		res, err = s.handleDelete(cmd)
	case cmdPatchConv:
		res, err = s.handlePatchConv(cmd)
	case cmdDeleteConv:
		res, err = s.handleDeleteConv()
		quit = err == nil
	case cmdShutdown:
		res, err = s.handleShutdown()
		quit = true
	case cmdCompact:
		res, err = s.handleCompact()
	default:
		err = newErr("bad_request", "未知命令")
	}

	if cmd.reply != nil {
		// reply 是带缓冲的，即使调用方已经超时走人也不会阻塞这里
		cmd.reply <- reply{res: res, err: err}
	}
	return quit
}

// stateTailMessages 是进会话首屏的消息条数（长会话不再全量拉取，
// 更早的由前端向上翻页加载）。
const stateTailMessages = 10

// ---------- 状态快照 ----------

func (s *Session) state() (result, error) {
	msgs, total, err := s.mgr.st.ListMessagesTail(s.convID, stateTailMessages)
	if err != nil {
		return result{}, newErr("internal", err.Error())
	}
	st := &State{Conv: s.refreshConv(), Seq: s.lastSeq, Messages: msgs, HasMore: total > len(msgs)}

	// 把附件一次性填进消息里：界面要显示缩略图，但**绝不能**在这里塞原图数据，
	// 只给 id 和 URL，由浏览器按需加载缩略图、点开才拉大图。
	FillMessagesAttachments(s.mgr.st, st.Messages)

	if s.gen != nil {
		// 正在生成：用内存里的实时内容覆盖落库副本（最多滞后 250ms）。
		// 这就是「刷新页面能立刻看到完整 CoT」的关键。
		// tool 循环会按段清空 builder（回传上游用），落库/展示要拼回全文。
		reasoning := strings.Join(s.gen.cotParts, "") + s.gen.reasoning.String()
		content := strings.Join(s.gen.contentParts, "") + s.gen.content.String()
		for i := range st.Messages {
			if st.Messages[i].ID == s.gen.msgID {
				st.Messages[i].Reasoning = reasoning
				st.Messages[i].Content = content
				break
			}
		}
		st.Generating = &Generating{
			MessageID:    s.gen.msgID,
			ProviderID:   s.gen.providerID,
			ModelID:      s.gen.modelID,
			Effort:       s.gen.effort,
			StartedAt:    s.gen.startedAt,
			ReasoningLen: len(reasoning),
			ContentLen:   len(content),
		}
	}
	return result{state: st}, nil
}

// ---------- 发送消息 ----------

func (s *Session) handleSend(cmd command) (result, error) {
	// 会话正在生成 → 这条多出来的消息直接丢弃：不落库、不排队。
	if s.gen != nil {
		return result{}, newErr("busy", "该会话正在生成回复，这条消息已丢弃")
	}
	// 压缩中 → 这条消息先别进来：等压缩完成（只阻塞本会话，别的会话不受影响）
	if s.mgr.IsCompacting(s.convID) {
		return result{}, newErr("compacting", "正在压缩上下文，请稍候再发")
	}
	content := strings.TrimSpace(cmd.content)
	// 允许「只发图不打字」
	if content == "" && len(cmd.attachmentIDs) == 0 {
		return result{}, newErr("bad_request", "消息内容和图片不能都为空")
	}
	ref, err := s.resolveRef(cmd.providerID, cmd.modelID, cmd.effort, cmd.effortSet)
	if err != nil {
		return result{}, err
	}
	cli, err := s.mgr.clientFor(s.ownerID(), ref.ProviderID)
	if err != nil {
		return result{}, err
	}
	// 这一轮实际用哪家/哪个模型，落回会话上 —— 用户在下拉里选过就固定下来，
	// 后续轮次和「重新生成」都按它走。
	if s.conv == nil || s.conv.ProviderID != ref.ProviderID || s.conv.ModelID != ref.ModelID {
		if conv, err := s.mgr.st.UpdateConversation(s.convID, store.ConversationPatch{
			ProviderID: &ref.ProviderID,
			ModelID:    &ref.ModelID,
		}); err == nil && conv != nil {
			s.conv = conv
			s.publishConv(ActionUpdated, s.refreshConv())
		}
	}
	modelID, effort := ref.ModelID, ref.Effort

	userMsg, err := s.mgr.st.CreateMessage(store.Message{
		ConvID:  s.convID,
		Role:    store.RoleUser,
		Status:  store.StatusDone,
		Content: content,
	})
	if err != nil {
		return result{}, newErr("internal", "保存消息失败: "+err.Error())
	}
	// 把这一轮带的图片挂上去（id 是上传接口返回的）。
	// 多租户：附件必须属于本会话属主（本人上传的，或哈希去重共享后已挂在本人
	// 会话上的），否则挂别人的附件进来、之后就能通过附件 URL 读到内容。
	if len(cmd.attachmentIDs) > 0 {
		if owned, err := s.mgr.st.CountAttachmentsOwnedBy(cmd.attachmentIDs, s.ownerID()); err != nil || owned != len(cmd.attachmentIDs) {
			log.Printf("[session %d] 附件挂载被拒绝：%d 个附件 id 中有不属于用户 #%d 的", s.convID, len(cmd.attachmentIDs), s.ownerID())
			return result{}, newErr("bad_request", "附件不存在或无权使用")
		}
		if err := s.mgr.st.AttachToMessage(userMsg.ID, cmd.attachmentIDs); err != nil {
			log.Printf("[session %d] 附件挂载失败（消息仍会发出，但不带图）: %v", s.convID, err)
		} else if full, err := s.mgr.st.GetMessageWithAttachments(userMsg.ID); err == nil && full != nil {
			userMsg = full
		}
	}
	// 把这一轮带的文件挂上去（id 是 /api/files 返回的，同样的归属校验）
	if len(cmd.fileIDs) > 0 {
		if owned, err := s.mgr.st.CountFilesOwnedBy(cmd.fileIDs, s.ownerID()); err != nil || owned != len(cmd.fileIDs) {
			log.Printf("[session %d] 文件挂载被拒绝：%d 个文件 id 中有不属于用户 #%d 的", s.convID, len(cmd.fileIDs), s.ownerID())
			return result{}, newErr("bad_request", "文件不存在或无权使用")
		}
		if err := s.mgr.st.AttachFileToMessage(userMsg.ID, cmd.fileIDs); err != nil {
			log.Printf("[session %d] 文件挂载失败（消息仍会发出，但不带文件）: %v", s.convID, err)
		}
	}
	s.publishMessage(ActionCreated, userMsg)

	// 助手占位消息也立刻落库：这样用户在这一刻刷新页面，
	// 依然能看到「正在生成」的那条消息。
	asstMsg, err := s.mgr.st.CreateMessage(store.Message{
		ConvID:     s.convID,
		Role:       store.RoleAssistant,
		Status:     store.StatusStreaming,
		ProviderID: ref.ProviderID,
		ModelID:    modelID,
		Effort:     effort,
	})
	if err != nil {
		return result{}, newErr("internal", "创建回复失败: "+err.Error())
	}
	s.publishMessage(ActionCreated, asstMsg)

	// 历史里包含刚写入的用户消息，助手占位消息因为还是空串会被过滤掉
	s.beginGeneration(cli, asstMsg.ID, ref.ProviderID, modelID, effort, s.buildHistory(modelID))

	// 广播一次会话状态（此时 Generating=true），让所有标签页的左侧列表
	// 都能立刻显示「正在生成」，而不必依赖它们是否正在看这个会话。
	s.publishConv(ActionUpdated, s.refreshConv())
	return result{userMsgID: userMsg.ID, asstMsgID: asstMsg.ID}, nil
}

// handleCompact 手动压缩（session 窗口的「压缩当前上下文」按钮）。
// 和当初的自动压缩同一套纪律：必须在没有生成在跑的边界上进行（busy 检查），
// 压缩期间该会话阻塞新消息（别的会话不受影响）。
func (s *Session) handleCompact() (result, error) {
	if s.gen != nil {
		return result{}, newErr("busy", "正在生成回复，等生成结束后再压缩")
	}
	if s.mgr.IsCompacting(s.convID) {
		return result{}, newErr("compacting", "正在压缩中")
	}
	s.mgr.setCompacting(s.convID, true)
	s.publishConv(ActionUpdated, s.refreshConv())
	go func() {
		s.runCompact()
		s.mgr.setCompacting(s.convID, false)
		// runCompact 成功时自己会广播；提前返回（消息太少不值得压）时不会，
		// 统一在这里再广播一次解除前端的「压缩中」状态
		s.mgr.bus.Publish(EventConv, s.ownerID(), func(seq int64) any {
			conv, err := s.mgr.st.GetConversation(s.convID)
			if err != nil || conv == nil {
				return ConvPayload{Seq: seq, ConvID: s.convID, Action: ActionUpdated}
			}
			return ConvPayload{Seq: seq, ConvID: s.convID, Action: ActionUpdated, Conv: conv}
		})
	}()
	return result{}, nil
}

// ---------- 停止 ----------

func (s *Session) handleStop() (result, error) {
	if s.gen == nil {
		return result{stopped: false}, nil
	}
	// 不等上游把连接关掉 —— 立刻收尾，用户点停止就该马上停。
	s.finishGeneration(store.StatusAborted, "")
	return result{stopped: true}, nil
}

// ---------- 重新生成（截断） ----------

func (s *Session) handleRegenerate(cmd command) (result, error) {
	if s.gen != nil {
		return result{}, newErr("busy", "该会话正在生成回复，暂时无法重新生成")
	}
	msg, err := s.mgr.st.GetMessage(cmd.msgID)
	if err != nil {
		return result{}, newErr("internal", err.Error())
	}
	if msg == nil || msg.ConvID != s.convID {
		return result{}, errNotFound
	}
	if err := s.frozenErr(msg.ID); err != nil {
		return result{}, err
	}
	// 重新生成沿用会话当前的供应商 / 模型
	ref, err := s.resolveRef(0, "", "", false)
	if err != nil {
		return result{}, err
	}
	cli, err := s.mgr.clientFor(s.ownerID(), ref.ProviderID)
	if err != nil {
		return result{}, err
	}
	modelID, effort := ref.ModelID, ref.Effort

	// 目标是助手消息 → 连它一起删；目标是用户消息 → 保留它、删它之后的
	inclusive := msg.Role == store.RoleAssistant
	if _, err := s.mgr.st.DeleteMessagesFrom(s.convID, msg.ID, inclusive); err != nil {
		return result{}, newErr("internal", "截断历史失败: "+err.Error())
	}
	s.publishMessage(ActionTruncated, nil, msg.ID)

	history := s.buildHistory(modelID)
	if !hasUserMessage(history) {
		return result{}, newErr("bad_request", "截断后没有可用的用户消息")
	}
	asstMsg, err := s.mgr.st.CreateMessage(store.Message{
		ConvID:     s.convID,
		Role:       store.RoleAssistant,
		Status:     store.StatusStreaming,
		ProviderID: ref.ProviderID,
		ModelID:    modelID,
		Effort:     effort,
	})
	if err != nil {
		return result{}, newErr("internal", "创建回复失败: "+err.Error())
	}
	s.publishMessage(ActionCreated, asstMsg)
	s.beginGeneration(cli, asstMsg.ID, ref.ProviderID, modelID, effort, history)
	s.publishConv(ActionUpdated, s.refreshConv())
	return result{asstMsgID: asstMsg.ID, message: msg}, nil
}

// frozenErr 检查消息是否已被压缩冻结：id <= compacted_before 的消息只读。
// 冻结是硬保证（后端拒绝），前端的按钮隐藏只是体验层。
func (s *Session) frozenErr(msgID int64) error {
	conv, err := s.mgr.st.GetConversation(s.convID)
	if err == nil && conv != nil && conv.CompactedBefore > 0 && msgID <= conv.CompactedBefore {
		return NewError("archived", "这条消息已随上下文压缩归档，不能再修改")
	}
	return nil
}

// ---------- 编辑 / 还原 / 删除 ----------

func (s *Session) handleEdit(cmd command) (result, error) {
	msg, err := s.mgr.st.GetMessage(cmd.msgID)
	if err != nil {
		return result{}, newErr("internal", err.Error())
	}
	if msg == nil || msg.ConvID != s.convID {
		return result{}, errNotFound
	}
	if err := s.frozenErr(msg.ID); err != nil {
		return result{}, err
	}
	if s.gen != nil && s.gen.msgID == msg.ID {
		return result{}, newErr("busy", "这条消息正在生成中，无法编辑")
	}
	updated, err := s.mgr.st.UpdateMessageContent(msg.ID, cmd.content)
	if err != nil {
		return result{}, newErr("internal", "保存失败: "+err.Error())
	}
	s.publishMessage(ActionUpdated, updated)
	return result{message: updated}, nil
}

// handleEditReasoning 编辑 CoT。编辑后的 reasoning 会随历史回传给上游
// （DeepSeek 思考模式的规则）——这是「改 CoT 引导模型后续思路」的闭环。
func (s *Session) handleEditReasoning(cmd command) (result, error) {
	msg, err := s.mgr.st.GetMessage(cmd.msgID)
	if err != nil {
		return result{}, newErr("internal", err.Error())
	}
	if msg == nil || msg.ConvID != s.convID {
		return result{}, errNotFound
	}
	if err := s.frozenErr(msg.ID); err != nil {
		return result{}, err
	}
	if s.gen != nil && s.gen.msgID == msg.ID {
		return result{}, newErr("busy", "这条消息正在生成中，无法编辑")
	}
	updated, err := s.mgr.st.UpdateMessageReasoning(msg.ID, cmd.content)
	if err != nil {
		return result{}, newErr("internal", "保存失败: "+err.Error())
	}
	s.publishMessage(ActionUpdated, updated)
	return result{message: updated}, nil
}

func (s *Session) handleRestore(cmd command) (result, error) {
	msg, err := s.mgr.st.GetMessage(cmd.msgID)
	if err != nil {
		return result{}, newErr("internal", err.Error())
	}
	if msg == nil || msg.ConvID != s.convID {
		return result{}, errNotFound
	}
	if err := s.frozenErr(msg.ID); err != nil {
		return result{}, err
	}
	updated, err := s.mgr.st.RestoreMessage(msg.ID)
	if err != nil {
		return result{}, newErr("internal", "还原失败: "+err.Error())
	}
	s.publishMessage(ActionUpdated, updated)
	return result{message: updated}, nil
}

func (s *Session) handleRestoreReasoning(cmd command) (result, error) {
	msg, err := s.mgr.st.GetMessage(cmd.msgID)
	if err != nil {
		return result{}, newErr("internal", err.Error())
	}
	if msg == nil || msg.ConvID != s.convID {
		return result{}, errNotFound
	}
	if err := s.frozenErr(msg.ID); err != nil {
		return result{}, err
	}
	updated, err := s.mgr.st.RestoreMessageReasoning(msg.ID)
	if err != nil {
		return result{}, newErr("internal", "还原失败: "+err.Error())
	}
	s.publishMessage(ActionUpdated, updated)
	return result{message: updated}, nil
}

func (s *Session) handleDelete(cmd command) (result, error) {
	msg, err := s.mgr.st.GetMessage(cmd.msgID)
	if err != nil {
		return result{}, newErr("internal", err.Error())
	}
	if msg == nil || msg.ConvID != s.convID {
		return result{}, errNotFound
	}
	if err := s.frozenErr(msg.ID); err != nil {
		return result{}, err
	}
	if s.gen != nil && s.gen.msgID == msg.ID {
		return result{}, newErr("busy", "这条消息正在生成中，无法删除")
	}
	if err := s.mgr.st.DeleteMessage(msg.ID); err != nil {
		return result{}, newErr("internal", "删除失败: "+err.Error())
	}
	s.publishMessage(ActionDeleted, nil, msg.ID)
	return result{}, nil
}

// ---------- 会话属性 / 删除会话 ----------

func (s *Session) handlePatchConv(cmd command) (result, error) {
	conv, err := s.mgr.st.UpdateConversation(s.convID, cmd.patch)
	if err != nil {
		return result{}, newErr("internal", err.Error())
	}
	if conv == nil {
		return result{}, errNotFound
	}
	s.conv = conv
	s.publishConv(ActionUpdated, conv)
	// 返回带正确 Generating 的视图：库里读出来的 conv 这个字段必然是 false，
	// 直接返回给前端会让它误以为「正在生成」已经结束。
	return result{conv: s.convView()}, nil
}

func (s *Session) handleDeleteConv() (result, error) {
	if s.gen != nil {
		s.gen.cancel()
		s.gen = nil
	}
	if err := s.mgr.st.DeleteConversation(s.convID); err != nil {
		return result{}, newErr("internal", "删除会话失败: "+err.Error())
	}
	s.publishConv(ActionDeleted, nil)
	return result{}, nil
}

// handleShutdown 在进程退出前把正在进行的生成收尾落盘。
// 没有这一步的话，Ctrl+C 会丢掉最后 250ms 还没落库的 CoT。
func (s *Session) handleShutdown() (result, error) {
	if s.gen == nil {
		return result{}, nil
	}
	s.finishGeneration(store.StatusAborted, "")
	return result{stopped: true}, nil
}

// ---------- 生成流程 ----------

// effortLabel 把「实际下发的思考强度」变成日志里能看懂的字样。
// 空串表示没下发这个参数（模型不支持思考，或用户选了「默认」）。
func effortLabel(effort string) string {
	if effort == "" {
		return "默认(不下发)"
	}
	return effort
}

func (s *Session) beginGeneration(cli *llm.Client, msgID, providerID int64, modelID, effort string, history []llm.ChatMessage) {
	ctx, cancel := context.WithCancel(context.Background())
	gen := &generation{
		msgID:      msgID,
		providerID: providerID,
		modelID:    modelID,
		effort:     effort,
		startedAt:  store.NowMs(),
		history:    history,
		startRes:   make(chan streamStart, 1),
		toolRes:    make(chan toolExecResult, 1),
		ctx:        ctx,
		cancel:     cancel,
	}
	// 工具声明：联网搜索是会话级开关；Python 沙箱与图像生成没有副作用开关——
	// 配了执行端就全局可用，模型自己决定要不要用。
	if s.conv != nil && s.conv.WebSearch && s.mgr.search != nil {
		gen.tools = append(gen.tools, search.ToolDef())
	}
	if s.mgr.sandbox != nil {
		gen.tools = append(gen.tools, sandbox.ToolDef())
	}
	if s.mgr.imagegen != nil {
		gen.tools = append(gen.tools, imagegen.ToolDef())
	}
	// 回看图片：配了附件存储就可用（读本地库，零外部依赖）
	if s.mgr.media != nil {
		gen.tools = append(gen.tools, readAttachmentToolDef())
	}
	// 读文档：需要 pybox（提取）+ misc（存文件）
	if s.mgr.sandbox != nil && s.mgr.media != nil {
		gen.tools = append(gen.tools, readFileToolDef())
	}
	if len(gen.tools) > 0 {
		// 工具纪律：模型「嘴硬」（没调用却声称调用过）是 tool calling 的顽疾，
		// 一句系统级的明确纪律能显著压低它。
		gen.history = append([]llm.ChatMessage{{Role: store.RoleSystem, Content: toolPreamble}}, gen.history...)
	}
	// 「本轮新增部分」的起点：preamble 前置之后再标记，
	// 收尾时 history[historyBase:] 就是要落库的 tool_rounds。
	gen.historyBase = len(gen.history)
	s.gen = gen

	log.Printf("[gen] 会话 %d 发起  供应商=%s(%d)  model=%s  思考强度=%s  工具=%d 个  历史=%d 条",
		s.convID, s.providerName(providerID), providerID, modelID, effortLabel(effort), len(gen.tools), len(history))

	s.startStream(gen, cli)
}

// startStream 在独立 goroutine 里发起上游请求（可能带工具声明），
// 会话 goroutine 继续响应命令。tool 循环里的「续传」也走这里。
func (s *Session) startStream(gen *generation, cli *llm.Client) {
	go func() {
		ch, dropped, err := cli.Stream(gen.ctx, llm.ChatRequest{
			Model:    gen.modelID,
			Messages: gen.history,
			Effort:   gen.effort,
			Tools:    gen.tools,
		})
		gen.startRes <- streamStart{gen: gen, ch: ch, toolsDropped: dropped, err: err}
	}()
}

// ownerID 返回本会话属主的 user id：多租户下所有供应商/设置解析都按它隔离。
func (s *Session) ownerID() int64 {
	if s.conv == nil {
		return 0
	}
	return s.conv.UserID
}

// providerName 只用于日志。
func (s *Session) providerName(providerID int64) string {
	p, err := s.mgr.st.ResolveProvider(s.ownerID(), providerID)
	if err != nil || p == nil {
		return "?"
	}
	return p.Name
}

func (s *Session) onStreamStart(sr streamStart) {
	// 期间可能已经被「停止」或「删除会话」处理掉了
	if s.gen == nil || s.gen != sr.gen {
		return
	}
	if sr.err != nil {
		s.finishGeneration(store.StatusError, sr.err.Error())
		return
	}
	if sr.toolsDropped {
		// 上游不认 tools：告诉用户工具这轮没生效（正文照出，不打断）
		s.gen.toolLog = append(s.gen.toolLog, search.Record{Error: "这家上游不支持工具调用，本次工具未生效"})
		s.gen.tools = nil
	}
	s.gen.ch = sr.ch
}

func (s *Session) onStreamEvent(ev llm.Event) {
	g := s.gen
	if g == nil {
		return
	}
	switch ev.Type {
	case llm.EventReasoning:
		g.reasoning.WriteString(ev.Delta)
		s.publishDelta(g.msgID, "reasoning", ev.Delta)
	case llm.EventContent:
		g.content.WriteString(ev.Delta)
		s.publishDelta(g.msgID, "content", ev.Delta)
	case llm.EventToolArgs:
		// tool_calls 的 arguments 流式增量（模型正在写的代码/参数），实时透传给前端
		s.publishDelta(g.msgID, "toolargs", ev.Delta)
	case llm.EventUsage:
		// tool 循环里每段流都有自己的 usage：累加而不是覆盖，
		// 否则多轮搜索后日志/界面只算到最后一段的开销。
		g.usage.PromptTokens += ev.Usage.PromptTokens
		g.usage.CompletionTokens += ev.Usage.CompletionTokens
		g.usage.ReasoningTokens += ev.Usage.ReasoningTokens
		g.usage.TotalTokens += ev.Usage.TotalTokens
		g.usage.CacheHitTokens += ev.Usage.CacheHitTokens
		g.usage.CacheMissTokens += ev.Usage.CacheMissTokens
		// 分段值也记下来（展示口径：这一段请求实际多大）
		g.segUsages = append(g.segUsages, store.Usage{
			PromptTokens:     ev.Usage.PromptTokens,
			CompletionTokens: ev.Usage.CompletionTokens,
			ReasoningTokens:  ev.Usage.ReasoningTokens,
			TotalTokens:      ev.Usage.TotalTokens,
			CacheHitTokens:   ev.Usage.CacheHitTokens,
			CacheMissTokens:  ev.Usage.CacheMissTokens,
		})
	case llm.EventToolCalls:
		s.onToolCalls(ev.ToolCalls)
	case llm.EventError:
		s.finishGeneration(store.StatusError, ev.Err.Error())
	case llm.EventDone:
		s.finishGeneration(store.StatusDone, "")
	}
}

// onToolCalls 处理「模型要求调用工具」：把 assistant 的 tool_calls 追加进本轮历史，
// 然后到 goroutine 里逐个执行（IO 不占会话 goroutine，停止随时能进）。
// 达到调用上限时本轮不再声明工具，模型只能基于已有结果写正文。
func (s *Session) onToolCalls(calls []llm.ToolCall) {
	g := s.gen
	if g == nil || len(calls) == 0 {
		return
	}
	if g.toolRounds >= maxToolRounds {
		// 上限后收到的 tool_calls 只可能来自「仍然带 tools 的最后一段流」——
		// 正常不该发生（上限后请求已不带 tools）。保险起见直接当普通收尾，
		// 已累积的内容照常落库，别让这一轮断掉。
		log.Printf("[session %d] 已达工具调用上限仍收到 tool_calls，按普通完成收尾", s.convID)
		s.finishGeneration(store.StatusDone, "")
		return
	}
	g.toolRounds++

	// 这段流已经完整结束（parseStream 发出 EventToolCalls 后关闭了事件 channel）。
	// 必须把 ch 摘掉：否则 actor 下一拍读到「channel 关闭」会误以为上游断连，
	// 直接 finishGeneration 把正在进行的工具执行 cancel 掉。
	g.ch = nil

	// 纪律按上游最新规则（实测 2026-10）：思考模式下 tool 循环里的 assistant
	// 消息必须回传该段的 reasoning_content，否则 400
	// ("The `reasoning_content` in the thinking mode must be passed back to the API")。
	// 先把当前段快照下来（落库时拼全文），再从空开始累积下一段。
	g.history = append(g.history, llm.ChatMessage{
		Role:      store.RoleAssistant,
		Content:   g.content.String(),
		ToolCalls: calls,
		Reasoning: g.reasoning.String(),
	})
	g.cotParts = append(g.cotParts, g.reasoning.String())
	g.contentParts = append(g.contentParts, g.content.String())
	g.reasoning.Reset()
	g.content.Reset()

	// 先把「正在做什么」推给界面，再出 goroutine 执行
	entries := make([]toolExecEntry, 0, len(calls))
	for _, call := range calls {
		entries = append(entries, toolExecEntry{call: call})
		s.publishDelta(g.msgID, "tool", toolCallLabel(call))
	}

	go func() {
		res := toolExecResult{gen: g, entries: entries}
		for i := range res.entries {
			s.execToolCall(g, &res.entries[i])
		}
		// 缓冲为 1，actor 一定会取走；会话已结束时按 gen 失配丢弃
		g.toolRes <- res
	}()
}

// toolCallLabel 生成界面上的状态行文本（「正在搜索：x」/「正在执行 Python…」）。
func toolCallLabel(call llm.ToolCall) string {
	switch call.Function.Name {
	case search.ToolName:
		return "正在搜索：" + search.ParseQuery(call.Function.Arguments)
	case sandbox.ToolName:
		return "正在执行 Python…"
	case imagegen.ToolName:
		prompt, _ := imagegen.ParsePrompt(call.Function.Arguments)
		return "正在生成图片：" + imagegen.TruncatePrompt(prompt, 40)
	case readAttachmentToolName:
		return fmt.Sprintf("正在回看图片 #%d…", parseAttachmentID(call.Function.Arguments))
	case readFileToolName:
		return fmt.Sprintf("正在读取文件 #%d…", parseAttachmentID(call.Function.Arguments))
	default:
		return "调用了未知工具：" + call.Function.Name
	}
}

// execToolCall 执行单个工具调用。在 goroutine 里跑，IO 不占会话 goroutine。
// 任何失败（搜索超时、沙箱不可达、代码跑挂）都写成结果回传，不中断这一轮。
func (s *Session) execToolCall(g *generation, e *toolExecEntry) {
	switch e.call.Function.Name {
	case search.ToolName:
		if s.mgr.search == nil {
			e.output = search.FormatToolResult(nil, fmt.Errorf("这个部署没有配置搜索后端"))
			return
		}
		query := search.ParseQuery(e.call.Function.Arguments)
		log.Printf("[gen] 会话 %d 联网搜索（第 %d 轮）: %q", s.convID, g.toolRounds, query)
		results, err := s.mgr.search.Search(g.ctx, query)
		if err != nil {
			log.Printf("[gen] 会话 %d 搜索 %q 失败: %v", s.convID, query, err)
		}
		e.output = search.FormatToolResult(results, err)
		e.record = search.NewRecord(query, results, err)
	case sandbox.ToolName:
		if s.mgr.sandbox == nil {
			e.output = sandbox.FormatToolResult(sandbox.Result{}, fmt.Errorf("这个部署没有配置 Python 沙箱"))
			return
		}
		code := sandbox.ParseCode(e.call.Function.Arguments)
		log.Printf("[gen] 会话 %d 执行 Python（第 %d 轮，%d 字符）", s.convID, g.toolRounds, len(code))
		r, err := s.mgr.sandbox.Run(g.ctx, code)
		if err != nil {
			log.Printf("[gen] 会话 %d Python 执行失败: %v", s.convID, err)
		}
		e.output = sandbox.FormatToolResult(r, err)
		e.record = sandbox.NewRecord(code, r, err)
	case imagegen.ToolName:
		if s.mgr.imagegen == nil {
			e.output = imagegen.FormatToolResult(fmt.Errorf("这个部署没有配置图像生成"), 0)
			return
		}
		prompt, size := imagegen.ParsePrompt(e.call.Function.Arguments)
		log.Printf("[gen] 会话 %d 生成图片（第 %d 轮）: %q", s.convID, g.toolRounds, imagegen.TruncatePrompt(prompt, 60))
		img, mime, err := s.mgr.imagegen.Generate(g.ctx, prompt, size)
		if err != nil {
			log.Printf("[gen] 会话 %d 生成图片失败: %v", s.convID, err)
			e.output = imagegen.FormatToolResult(err, 0)
			e.record = imagegen.Record{Type: "image", Prompt: prompt, Error: err.Error()}
			return
		}
		// 落盘 + 挂到正在生成的这条助手消息上：列表缩略图、点开看大图，走现成附件链路
		attID, err := s.attachGeneratedImage(g.msgID, img)
		if err != nil {
			log.Printf("[gen] 会话 %d 图片落盘失败: %v", s.convID, err)
			e.output = imagegen.FormatToolResult(fmt.Errorf("图片保存失败: %v", err), 0)
			e.record = imagegen.Record{Type: "image", Prompt: prompt, Error: err.Error()}
			return
		}
		if att, err := s.mgr.st.GetAttachmentInConv(attID, s.convID); err == nil && att != nil {
			log.Printf("[gen] 会话 %d 图片生成成功  %dx%d %.0fKB", s.convID, att.Width, att.Height, float64(att.Size)/1024)
		} else {
			log.Printf("[gen] 会话 %d 图片生成成功（%.0fKB）", s.convID, float64(len(img))/1024)
		}
		e.output = imagegen.FormatToolResult(nil, attID)
		// 支持的模型（DeepSeek/Kimi，见 supportsImageToolReceipt）：回执直接带上
		// 图片本体——模型画完立刻能看到自己画了什么，当轮就能自检/迭代，
		// 不用额外花一轮 read_attachment。tool 消息不落库，这张图只占当轮上下文；
		// 跨轮回放仍是纯文本回执（imageToolRounds），需要时用 read_attachment。
		if supportsImageToolReceipt(g.modelID) {
			dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img)
			e.outputParts = []llm.ContentPart{
				llm.TextPart(imagegen.FormatToolResultWithImage(attID)),
				llm.ImagePart(dataURL),
			}
			log.Printf("[gen] 会话 %d 生图回执带图（%s，base64 约 %.0f KB）",
				s.convID, g.modelID, float64(len(dataURL))/1024)
		}
		e.record = imagegen.Record{Type: "image", Prompt: prompt}
	case readAttachmentToolName:
		id := parseAttachmentID(e.call.Function.Arguments)
		a, err := s.mgr.st.GetAttachmentInConv(id, s.convID)
		if err != nil || a == nil {
			e.output = `{"error": "附件不存在或不属于这个会话"}`
			return
		}
		dataURL, _, err := s.imageDataURL(*a) // 发送版（长边 ≤1024），模型够用
		if err != nil {
			e.output = fmt.Sprintf(`{"error": %q}`, "读取图片失败："+err.Error())
			return
		}
		e.outputParts = []llm.ContentPart{
			llm.TextPart("图片内容如下："),
			llm.ImagePart(dataURL),
		}
	case readFileToolName:
		if s.mgr.sandbox == nil {
			e.output = `{"error": "这个部署没有配置 Python 沙箱，无法读取文件"}`
			return
		}
		id := parseAttachmentID(e.call.Function.Arguments)
		f, err := s.mgr.st.GetFileInConv(id, s.convID)
		if err != nil || f == nil {
			e.output = `{"error": "文件不存在或不属于这个会话"}`
			return
		}
		// 提取丢给 pybox：/files 是宿主机只读挂载，pypdf/docx/openpyxl/chardet 已预装
		path := "/files/" + f.Hash + f.Ext
		r, err := s.mgr.sandbox.Run(g.ctx, fileExtractScript(path))
		var text string
		if err != nil {
			text = "提取失败: " + err.Error()
		} else {
			text = r.Stdout
			if strings.TrimSpace(r.Stderr) != "" {
				text += "\n[提取提示] " + strings.TrimSpace(r.Stderr)
			}
			if strings.TrimSpace(text) == "" {
				text = "（提取完成，但没有文本内容——可能是扫描件或空文件）"
			}
		}
		e.output = fmt.Sprintf(`{"name": %q, "content": %q}`, f.Name, truncateRunes(text, 30000))
		e.record = map[string]any{"type": "file", "name": f.Name, "id": f.ID}
	default:
		log.Printf("[gen] 会话 %d 模型调用了未知工具 %q，按失败结果回传", s.convID, e.call.Function.Name)
		e.output = fmt.Sprintf(`{"error": %q}`, "未知工具 "+e.call.Function.Name)
	}
}

// onToolResult 把工具结果追加进历史并续传下一段流。
func (s *Session) onToolResult(tr toolExecResult) {
	g := s.gen
	// 期间被停止/收尾过了，迟到的回执直接丢
	if g == nil || g != tr.gen {
		return
	}
	for _, e := range tr.entries {
		// read_attachment 的回图是多段 content（实测 DeepSeek 支持 tool 消息带图）
		var content any = e.output
		if len(e.outputParts) > 0 {
			content = e.outputParts
		}
		g.history = append(g.history, llm.ChatMessage{
			Role:       "tool",
			Content:    content,
			ToolCallID: e.call.ID,
		})
		if e.record != nil {
			g.toolLog = append(g.toolLog, e.record)
		}
	}

	// 达到上限后下一段流不再声明工具：模型必须基于已有结果写正文。
	// 同时明确告诉它「工具用完了」——不说的话，还想继续调用的模型会把
	// 工具调用写成 DSML 文本标记吐进正文（实测抓到：3 轮搜索后正文吐了一堆
	// <｜｜DSML｜｜invoke>）。这条提示只在当轮有效，recordToolRounds 会把它滤掉，
	// 不会带进下一轮的历史（否则模型会以为工具一直不可用）。
	if g.toolRounds >= maxToolRounds {
		g.tools = nil
		g.history = append(g.history, llm.ChatMessage{
			Role:    store.RoleSystem,
			Content: "（系统提示：本轮回答的工具调用次数已用完。请基于已经获得的信息直接给出最终回答，不要再尝试调用任何工具，也不要在正文里书写任何工具调用标记。）",
		})
		log.Printf("[gen] 会话 %d 工具调用达上限（%d 轮），转为强制收尾", s.convID, g.toolRounds)
	}
	cli, err := s.mgr.clientFor(s.ownerID(), g.providerID)
	if err != nil {
		s.finishGeneration(store.StatusError, err.Error())
		return
	}
	s.startStream(g, cli)
}

// finishGeneration 收尾一次生成：落最终内容 → 发完整消息事件 → 自动标题。
//
// 注意最后这条 message 事件带的是**完整消息**（不是增量），所以前端即使
// 中间漏掉过 delta，也会在这一刻被纠正回后端记录 —— 前端的真相永远来自后端。
func (s *Session) finishGeneration(status, errMsg string) {
	g := s.gen
	if g == nil {
		return
	}
	s.gen = nil
	if g.cancel != nil {
		g.cancel()
	}
	if status == "" || status == store.StatusStreaming {
		status = store.StatusDone
	}

	// tool 循环按段切分的 builder，落库时拼回完整内容
	reasoning := strings.Join(g.cotParts, "") + g.reasoning.String()
	content := strings.Join(g.contentParts, "") + g.content.String()
	searchLog := ""
	if len(g.toolLog) > 0 {
		if buf, err := json.Marshal(g.toolLog); err == nil {
			searchLog = string(buf)
		}
	}
	toolRounds := recordToolRounds(g)
	if err := s.mgr.st.FinishMessage(g.msgID, status, reasoning, content, errMsg, g.usage, searchLog, toolRounds); err != nil {
		log.Printf("[session %d] 写入最终内容失败: %v", s.convID, err)
	}

	// 回答落地的这一刻才碰 updated_at。
	// 侧边栏按 updated_at 倒序排，所以「答完」才会把这条会话顶到最上面 ——
	// 用户要的是「答完了，可以看了」，而不是「我刚发出去就跳」。
	if err := s.mgr.st.TouchConversation(s.convID); err != nil {
		log.Printf("[session %d] 更新会话时间失败: %v", s.convID, err)
	}

	// 一 request 一行日志：思考强度、耗时、token 分布、以及缓存命中情况。
	// 缓存这块对成本影响很大，值得每次都看得到。
	log.Printf("[gen] 会话 %d 完成  status=%s  供应商=%s(%d)  model=%s  思考强度=%s  用时=%.1fs  工具=%d 轮  "+
		"tokens: prompt=%d completion=%d(思考 %d) total=%d  缓存: 命中=%d 未命中=%d  %s",
		s.convID, status, s.providerName(g.providerID), g.providerID, g.modelID, effortLabel(g.effort),
		float64(store.NowMs()-g.startedAt)/1000, g.toolRounds,
		g.usage.PromptTokens, g.usage.CompletionTokens, g.usage.ReasoningTokens, g.usage.TotalTokens,
		g.usage.CacheHitTokens, g.usage.CacheMissTokens, cacheHint(g.usage))

	if msg, err := s.mgr.st.GetMessageWithAttachments(g.msgID); err == nil && msg != nil {
		s.publishMessage(ActionUpdated, msg)
	}

	// 出错时额外推一条 notice：用户可能已经切到别的会话了，
	// 不提示的话他要等切回来才发现这一轮失败了。
	if status == store.StatusError && errMsg != "" {
		title := "会话"
		if s.conv != nil && s.conv.Title != "" {
			title = s.conv.Title
		}
		convID := s.convID
		text := fmt.Sprintf("「%s」生成失败：%s", title, errMsg)
		s.lastSeq = s.mgr.bus.Publish(EventNotice, s.ownerID(), func(seq int64) any {
			return NoticePayload{Seq: seq, ConvID: convID, Level: "error", Text: text}
		})
	}

	// 首轮结束后用第一条用户消息给会话起名（标题还是默认值时才动手）。
	// 中止的轮次也起名：用户消息已经在了，标题一样有意义。
	//
	// 这里是「秒出的本地标题」：直接截断第一句用户消息，不花任何请求。
	// 拿到之后立刻广播，让左侧列表马上有个能看的名字；同时再在后台让模型
	// 起一个更好的标题覆盖它（见 title.go）。
	if _, changed, err := s.mgr.st.AutoTitle(s.convID); err == nil && changed {
		if conv, err := s.mgr.st.GetConversation(s.convID); err == nil && conv != nil {
			s.conv = conv
			s.publishConv(ActionUpdated, conv)
			s.startTitleGeneration(g.content.String(), conv.Title, g.providerID, g.modelID)
		}
	}

	// 最后再广播一次会话状态（此时 s.gen 已为 nil，Generating=false），
	// 让左侧列表的「正在生成」标记可靠地落下去 —— 不依赖前端是否正在看这个会话。
	if s.conv != nil {
		s.publishConv(ActionUpdated, s.refreshConv())
	}
}

// startTitleGeneration 在后台请模型起一个更好的标题来覆盖本地截断的那个。
// 失败/超时都只是打条日志，界面上仍然是本地标题，不会出现空白名。
func (s *Session) startTitleGeneration(reply, expectedTitle string, providerID int64, modelID string) {
	cli, err := s.mgr.clientFor(s.ownerID(), providerID)
	if err != nil {
		return
	}
	first, err := s.mgr.st.FirstUserMessage(s.convID)
	if err != nil || strings.TrimSpace(first) == "" {
		return
	}
	s.mgr.titler.Generate(s.convID, expectedTitle, cli, modelID, first, reply)
}

// cacheHint 把缓存命中率算成一句人话，方便一眼判断「这次是不是白花钱了」。
func cacheHint(u store.Usage) string {
	total := u.CacheHitTokens + u.CacheMissTokens
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("(前缀缓存命中率 %.0f%%)", float64(u.CacheHitTokens)*100/float64(total))
}

func (s *Session) flushIfDirty() {
	g := s.gen
	if g == nil {
		return
	}
	if g.reasoning.Len() == g.flushedR && g.content.Len() == g.flushedC {
		return
	}
	g.flushedR, g.flushedC = g.reasoning.Len(), g.content.Len()
	fullR := strings.Join(g.cotParts, "") + g.reasoning.String()
	fullC := strings.Join(g.contentParts, "") + g.content.String()
	if err := s.mgr.st.UpdateMessageStream(g.msgID, fullR, fullC); err != nil {
		log.Printf("[session %d] 流式落库失败: %v", s.convID, err)
	}
}

// ---------- 供应商 / 模型 / 强度解析 ----------

// modelRef 是「这条会话这一轮要发给谁」的完整描述。
// 多供应商下 model_id 单独一个字段是不够的：两个网关都可能有 gpt-4o。
type modelRef struct {
	ProviderID int64
	ModelID    string
	Effort     string
}

// resolveRef 把「命令里带的可选覆盖」+「会话自身配置」+「全局默认」合成一套能用的值。
// wantProvider/wantModel 为 0/空 表示沿用会话配置。
func (s *Session) resolveRef(wantProvider int64, wantModel string, wantEffort string, effortSet bool) (modelRef, error) {
	pid := wantProvider
	if pid <= 0 && s.conv != nil {
		pid = s.conv.ProviderID
	}
	// 供应商可能已被删除/从未配置：交给 store 兜底（回退默认供应商），
	// 兜不到就会得到 no_provider。
	prov, err := s.mgr.st.ResolveProvider(s.ownerID(), pid)
	if err != nil {
		return modelRef{}, newErr("internal", err.Error())
	}
	if prov == nil {
		return modelRef{}, newErr("no_provider", "还没有配置任何供应商，请先在设置里添加一个")
	}
	pid = prov.ID

	mid := strings.TrimSpace(wantModel)
	if mid == "" && s.conv != nil && s.conv.ProviderID == pid {
		// 只有会话本来就属于这家时才敢沿用它的 model_id：
		// 会话的供应商被删过的话，它的 model_id 很可能不属于现在这家。
		mid = s.conv.ModelID
	}
	if mid == "" {
		// 设置里的默认模型（必须确实属于这家，否则会发错地方）
		if v, _ := s.mgr.st.GetSetting(s.ownerID(), store.SettingDefaultModel); v != "" {
			if m, err := s.mgr.st.GetModelFor(pid, v); err == nil && m != nil {
				mid = v
			}
		}
	}
	if mid == "" {
		// 最后退回这家下的第一个模型
		if m, err := s.mgr.st.FirstModelOfProvider(pid); err == nil && m != nil {
			mid = m.ModelID
		}
	}
	if mid == "" {
		return modelRef{}, newErr("no_model", "这家供应商还没有可用模型，请先在设置里点「获取模型列表」")
	}

	return modelRef{
		ProviderID: pid,
		ModelID:    mid,
		Effort:     s.resolveEffort(pid, mid, wantEffort, effortSet),
	}, nil
}

// resolveEffort 返回最终要发给上游的 reasoning_effort。
// 空串表示「不发这个参数」—— 不支持思考的模型必须留空。
func (s *Session) resolveEffort(providerID int64, modelID, want string, wantSet bool) string {
	m, err := s.mgr.st.GetModelFor(providerID, modelID)
	if err != nil || m == nil || !m.Reasoning {
		return ""
	}
	if wantSet {
		return want // 前端显式指定（可能是空串 = 关闭思考）
	}
	if s.conv != nil && s.conv.Effort != "" {
		return s.conv.Effort
	}
	if v, err := s.mgr.st.GetSetting(s.ownerID(), store.SettingDefaultEffort); err == nil && v != "" {
		return v
	}
	return store.DefaultEffort
}

// currentDatePrompt 生成「今天几号」的系统提示（每次 buildHistory 现算，跨天自然更新）。
func currentDatePrompt() string {
	now := time.Now()
	weekdays := []string{"日", "一", "二", "三", "四", "五", "六"}
	return fmt.Sprintf("当前日期：%d年%d月%d日，星期%s。", now.Year(), int(now.Month()), now.Day(), weekdays[now.Weekday()])
}

// buildHistory 组装发给上游的消息历史。
//
// reasoning_content 的回传规则（DeepSeek 官方 thinking_mode 文档 + 实测）：
//   - 携带 tools 的请求：历史 assistant 消息均应回传 reasoning_content
//     （拼接进上下文，保持思考连贯）；tool 循环内不回传直接 400。
//   - 不携带 tools 的请求：传不传无所谓，上游会忽略。
//
// 所以这里统一回传（用户消息没有 reasoning，omitempty 不输出）。
// 带图片的用户消息会拼成多段 content（vision），读的是「发送版」而不是原图。
// 注意只有**用户消息**带图：assistant 消息挂图（比如 AI 生成的图）会被上游
// 直接拒绝（DeepSeek: "Image in assistant message is not supported"）——
// 生成的图是展示给用户的，模型不需要回看自己画的图。
// 发给模型的是**原图**（发送版 1024 会丢细节，上游自己会做尺寸归一化）；
// 原图超 ~10MB 才退回发送版（上游对单图大小有限制）。
func (s *Session) buildHistory(modelID string) []llm.ChatMessage {
	// 压缩在独立 goroutine 里写库，不会直接改 actor 独占的 s.conv。
	// 每轮组装前重读，确保下一轮立即使用最新摘要和 compacted_before。
	if conv, err := s.mgr.st.GetConversation(s.convID); err == nil && conv != nil {
		s.conv = conv
	}
	out := []llm.ChatMessage{}
	// 三条 system，顺序固定：会话级 → 全局默认 → 时间。
	// 会话级 system prompt 是这条会话的「宪法」：永远在最前面、不参与压缩。
	if s.conv != nil && strings.TrimSpace(s.conv.SystemPrompt) != "" {
		out = append(out, llm.ChatMessage{Role: store.RoleSystem, Content: s.conv.SystemPrompt})
	}
	// 全局默认 system prompt（设置里的「默认系统提示词」）：注入式——每轮现读，
	// 改设置立即影响所有会话（旧语义「创建时复制」已废弃，避免和注入重复）。
	if v, _ := s.mgr.st.GetSetting(s.ownerID(), store.SettingDefaultSystemPrompt); strings.TrimSpace(v) != "" {
		out = append(out, llm.ChatMessage{Role: store.RoleSystem, Content: v})
	}
	// 时间提示放最后（它天天变，放最前反而打扰前缀缓存的稳定段）。
	out = append(out, llm.ChatMessage{Role: store.RoleSystem, Content: currentDatePrompt()})
	msgs, err := s.mgr.st.ListMessages(s.convID)
	if err != nil {
		log.Printf("[session %d] 读取历史失败: %v", s.convID, err)
		return out
	}

	// 上下文压缩：摘要插在 system prompt 之后、近期消息之前；
	// 被压缩覆盖的旧消息不再进入历史（它们的摘要代表它们）。
	if s.conv != nil && s.conv.CompactedBefore > 0 && strings.TrimSpace(s.conv.Summary) != "" {
		out = append(out, llm.ChatMessage{
			Role:    store.RoleSystem,
			Content: "（较早的对话历史已压缩为以下摘要）\n" + s.conv.Summary,
		})
		kept := msgs[:0]
		for _, m := range msgs {
			if m.ID > s.conv.CompactedBefore {
				kept = append(kept, m)
			}
		}
		msgs = kept
	}

	// 一次把所有消息的附件取回来，避免逐条查询
	ids := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	attMap, err := s.mgr.st.AttachmentsByMessageIDs(ids)
	if err != nil {
		log.Printf("[session %d] 读取附件失败（这一轮不带图）: %v", s.convID, err)
		attMap = nil
	}
	fileMap, err := s.mgr.st.FilesByMessageIDs(ids)
	if err != nil {
		log.Printf("[session %d] 读取文件关联失败（这一轮不带文件标注）: %v", s.convID, err)
		fileMap = nil
	}

	imageCount, imageBytes := 0, 0
	// 生成图的回放：全量、原图（和用户上传的图同等待遇）。
	// 无状态 API 的语义就是每轮全量重放，长度交给压缩阈值兜底，
	// 不在这层做窗口/缩略图之类的降级策略（策略越多越容易出 bug）。
	// 只在当前模型支持「tool 消息带图」时回传图片（GPT 系要么静默丢图
	// 要么可能 400，那些会话保持纯文本回执 + read_attachment）。
	attByID := make(map[int64]store.Attachment)
	for _, m := range msgs {
		if m.Role != store.RoleAssistant {
			continue
		}
		for _, a := range attMap[m.ID] {
			attByID[a.ID] = a
		}
	}
	replayImages, replayBytes := 0, 0
	var imgResolver func(attID int64) string
	if supportsImageToolReceipt(modelID) {
		imgResolver = func(attID int64) string {
			a, ok := attByID[attID]
			if !ok {
				return ""
			}
			dataURL, n, err := s.imageDataURL(a)
			if err != nil {
				log.Printf("[session %d] 回放图 %d 读取失败，这张退回文本回执: %v", s.convID, attID, err)
				return ""
			}
			replayImages++
			replayBytes += n
			return dataURL
		}
	}

	for _, m := range msgs {
		if m.Role == store.RoleSystem {
			continue
		}
		if m.Status == store.StatusStreaming || m.Status == store.StatusError {
			continue
		}
		if strings.TrimSpace(m.Content) == "" && len(attMap[m.ID]) == 0 && strings.TrimSpace(m.ToolRounds) == "" {
			continue
		}

		parts := []llm.ContentPart{llm.TextPart(m.Content)}
		if m.Role == store.RoleUser {
			for _, a := range attMap[m.ID] {
				dataURL, n, err := s.imageDataURL(a)
				if err != nil {
					log.Printf("[session %d] 附件 %d 读取失败，这一轮跳过它: %v", s.convID, a.ID, err)
					continue
				}
				parts = append(parts, llm.ImagePart(dataURL))
				imageCount++
				imageBytes += n
			}
			// 文件标注：模型用 read_file 工具按需读取（文本不进历史，防上下文爆炸）
			for _, f := range fileMap[m.ID] {
				parts = append(parts, llm.TextPart(fmt.Sprintf("\n[文件#%d「%s」]", f.ID, f.Name)))
			}
		} else {
			// 优先回放落库的完整工具轮（v11 起录制）：assistant(tool_calls) 段 +
			// tool 结果段原样进历史，搜索结果/Python 输出全量保留——无状态 API
			// 就该全量重放，长度交给压缩阈值。老消息（没有 tool_rounds）走老路：
			// 生图按 search_log 重建 + 文本摘要。
			if replayRecordedRounds(&out, &m, imgResolver) {
				// 完整回放里中间段的 reasoning 各就各位（录制时按段存的），
				// 正文这条的 reasoning 已被换成「最后一段」（或编辑后的全文）
			} else {
				// assistant 的附件（AI 生成的图）不能进 vision content（上游 400），
				// 也不再用「系统注」文本标注——那种格式模型会学去假装调用工具（实测抓到）。
				// 生成的图重放成结构化工具轮，插在正文之前；reasoning 随硬规则挂到
				// tool_calls 消息上，正文这条就不再重复带了。
				rounds := imageToolRounds(m.SearchLog, attMap[m.ID], m.Reasoning, imgResolver)
				out = append(out, rounds...)
				if len(rounds) > 0 {
					m.Reasoning = ""
				}
				// 工具调用摘要也注入：老消息的 tool 交互没有完整记录，
				// 这份摘要让模型至少记得「我那轮搜了什么词、拿到什么来源」
				if summary := formatToolLogSummary(m.SearchLog); summary != "" {
					parts = append(parts, llm.TextPart("\n"+summary))
				}
			}
		}
		// reasoning_content 回传：DeepSeek 官方规则——携带 tools 的请求，历史轮次的
		// reasoning_content 均应回传（会拼接进上下文，保持思考连贯）；不携带 tools 时
		// 上游直接忽略，无副作用。用户消息没有 reasoning（omitempty 不序列化）。
		out = append(out, llm.ChatMessage{Role: m.Role, Content: llm.BuildContent(parts), Reasoning: m.Reasoning})
	}

	if imageCount > 0 || replayImages > 0 {
		// 图片很贵，打进日志让成本看得见
		log.Printf("[gen] 会话 %d 这轮带图：用户图 %d 张（原图共 %.0f KB）+ 回放生成图 %d 张（共 %.0f KB）",
			s.convID, imageCount, float64(imageBytes)/1024, replayImages, float64(replayBytes)/1024)
	}

	// 跨轮回传的 reasoning 预算。tool 循环内必须完整回传（上游 400 硬规则），
	// 但跨轮官方不强制——长 CoT 会话全量回传会让每轮 prompt 都重发全部历史
	// 思考（26 万字 CoT 的会话下一条消息就是 26 万起跳）。超预算时从最早的
	// 消息开始丢弃 reasoning（正文保留；最近的思路才是连贯性最需要的）。
	applyReasoningBudget(out)
	return out
}

// reasoningBudgetRunes 是跨轮历史里 reasoning_content 的总量预算（字符）。
const reasoningBudgetRunes = 32768

// applyReasoningBudget 从最新的消息往回保留 reasoning，超预算的旧消息置空。
// 例外：带 tool_calls 的消息（重放的生图工具轮）reasoning 是上游 400 硬规则，
// 永远保留、不占预算——丢正文思考可以，丢它会直接报错。
func applyReasoningBudget(msgs []llm.ChatMessage) {
	total := 0
	dropped := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Reasoning == "" || len(msgs[i].ToolCalls) > 0 {
			continue
		}
		n := utf8.RuneCountInString(msgs[i].Reasoning)
		if total+n > reasoningBudgetRunes {
			msgs[i].Reasoning = ""
			dropped++
		} else {
			total += n
		}
	}
	if dropped > 0 {
		log.Printf("[gen] 跨轮 reasoning 超预算（%d 字），丢弃了 %d 条较早的思考", reasoningBudgetRunes, dropped)
	}
}

// attachGeneratedImage 把生成的图片落到附件库并挂到消息上，返回附件 id。
// media.Save 内部按内容哈希去重（同一张图只占一份磁盘），缩略图/发送版一并生成。
func (s *Session) attachGeneratedImage(msgID int64, img []byte) (int64, error) {
	if s.mgr.media == nil {
		return 0, fmt.Errorf("没有配置 misc 目录")
	}
	saved, err := s.mgr.media.Save(bytes.NewReader(img), "")
	if err != nil {
		return 0, err
	}
	att, err := s.mgr.st.CreateAttachment(store.Attachment{
		Hash:   saved.Hash,
		Kind:   "image",
		MIME:   saved.MIME,
		Ext:    saved.Ext,
		Name:   "generated" + saved.Ext,
		Size:   saved.Size,
		Width:  saved.Width,
		Height: saved.Height,
	})
	if err != nil {
		return 0, err
	}
	return att.ID, s.mgr.st.AttachToMessage(msgID, []int64{att.ID})
}

// imageDataURL 读出一张附件并编码成 data URL。
// 发**原图**给模型：发送版（1024 压缩版）会丢细节（文档照片的小字看不清），
// 上游自己会做尺寸归一化，原图信息最全；缩略图/发送版只服务前端显示和通信。
// 保护：原图超过 ~10MB 退回发送版——上游 vision 对单图大小普遍有限制（OpenAI 20MB），
// base64 还有 33% 膨胀。
func (s *Session) imageDataURL(a store.Attachment) (string, int, error) {
	if s.mgr.media == nil {
		return "", 0, fmt.Errorf("没有配置 misc 目录")
	}
	b, mime, err := s.readOriginalOrSend(a)
	if err != nil {
		return "", 0, err
	}
	if mime == "" {
		mime = a.MIME
	}
	if mime == "" {
		mime = "image/jpeg"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b), len(b), nil
}

// maxRawImageBytes 是发给模型的原图上限（超过就用发送版）。
const maxRawImageBytes = 10 << 20

func (s *Session) readOriginalOrSend(a store.Attachment) ([]byte, string, error) {
	b, err := os.ReadFile(s.mgr.media.OriginalPath(a.Hash, a.Ext))
	if err != nil {
		return nil, "", err
	}
	if len(b) <= maxRawImageBytes {
		mime := a.MIME
		if mime == "" {
			mime = "image/" + strings.TrimPrefix(a.Ext, ".")
		}
		return b, mime, nil
	}
	// 原图太大：退回发送版（≤1024 的 JPEG）
	return s.mgr.media.OpenForModel(a.Hash, a.Ext, a.MIME)
}

func hasUserMessage(msgs []llm.ChatMessage) bool {
	for _, m := range msgs {
		if m.Role == store.RoleUser {
			return true
		}
	}
	return false
}

// ---------- 事件发布 ----------

func (s *Session) publishDelta(msgID int64, channel, text string) {
	convID := s.convID
	s.lastSeq = s.mgr.bus.Publish(EventDelta, s.ownerID(), func(seq int64) any {
		return DeltaPayload{Seq: seq, ConvID: convID, MessageID: msgID, Channel: channel, Text: text}
	})
}

// fillAttachmentURLs 给消息里的附件补上界面要用的相对 URL。
//
// store.Attachment 是数据库行，本身不含 URL；而契约 §4 要求前端拿到
// thumb_url / raw_url。**任何出网关的消息都必须先过这一道**，否则前端
// 拿到的附件没有 URL，<img src> 就是空的。
func fillAttachmentURLs(msgs []store.Message) {
	for i := range msgs {
		for j := range msgs[i].Attachments {
			a := &msgs[i].Attachments[j]
			if a.ID <= 0 {
				continue
			}
			a.ThumbURL = fmt.Sprintf("/api/attachments/%d/thumb", a.ID)
			a.RawURL = fmt.Sprintf("/api/attachments/%d/raw", a.ID)
		}
	}
}

// publishMessage 发布完整消息（含文件与图片）。m 为 nil 时只带 message_id
// （删除/截断），前端收到这类事件应重新拉 state。
func (s *Session) publishMessage(action string, m *store.Message, explicitID ...int64) {
	convID := s.convID
	var msgID int64
	if m != nil {
		m = messageWithAttachments(s.mgr.st, m)
		msgID = m.ID
	} else if len(explicitID) > 0 {
		msgID = explicitID[0]
	}
	s.lastSeq = s.mgr.bus.Publish(EventMessage, s.ownerID(), func(seq int64) any {
		return MessagePayload{Seq: seq, ConvID: convID, Action: action, MessageID: msgID, Message: m}
	})
}

func (s *Session) publishConv(action string, conv *store.Conversation) {
	convID := s.convID
	if conv != nil {
		// Conversation.Generating 在数据库里并不存在，是从「有没有 streaming 消息」
		// 算出来的。从库里读出来的 conv（比如改标题、自动起名之后）这个字段必然是
		// false，直接广播出去会让前端把「正在生成」的转圈给误清掉。
		// 这里按会话自己的真实状态补上。
		c := *conv
		c.Generating = s.gen != nil
		c.Compacting = s.mgr.IsCompacting(convID)
		conv = &c
	}
	s.lastSeq = s.mgr.bus.Publish(EventConv, s.ownerID(), func(seq int64) any {
		return ConvPayload{Seq: seq, ConvID: convID, Action: action, Conv: conv}
	})
}

// convView 返回带正确 Generating 标记的会话视图。
func (s *Session) convView() *store.Conversation {
	if s.conv == nil {
		return nil
	}
	c := *s.conv
	c.Generating = s.gen != nil
	c.Compacting = s.mgr.IsCompacting(s.convID)
	return &c
}

// refreshConv 从库里重读一次会话再返回视图。
//
// 会话 goroutine 里缓存的 s.conv 是「命令发生时」的快照，而 message_count /
// updated_at 会随消息增删变化，直接用缓存广播或放进 state 会让界面显示旧的
// 条数。这里统一先读库（一个走索引的小查询），保证对外的一致。
func (s *Session) refreshConv() *store.Conversation {
	conv, err := s.mgr.st.GetConversation(s.convID)
	if err == nil && conv != nil {
		s.conv = conv
	}
	return s.convView()
}
