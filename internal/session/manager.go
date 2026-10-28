package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"aiassistant/internal/imagegen"
	"aiassistant/internal/llm"
	"aiassistant/internal/media"
	"aiassistant/internal/sandbox"
	"aiassistant/internal/search"
	"aiassistant/internal/store"
)

// NewError 构造带错误码的领域错误。HTTP 层按 Code 映射状态码。
func NewError(code, msg string) *Error { return &Error{Code: code, Msg: msg} }

func newErr(code, msg string) *Error { return NewError(code, msg) }

// ClientFactory 按（属主 userID, 供应商 id）构造上游客户端。
// 做成函数是因为用户随时可能改 URL / Key，不能把客户端固定在启动时刻；
// 带参数是因为一个部署可以配多个供应商，会话发到哪家由它自己的 provider_id 决定。
// userID 必须参与解析：providers 按用户隔离，拿不到别人供应商里的 apikey。
type ClientFactory func(userID, providerID int64) (*llm.Client, error)

// Store 是会话层需要的持久化能力。
type Store interface {
	GetConversation(id int64) (*store.Conversation, error)
	UpdateConversation(id int64, p store.ConversationPatch) (*store.Conversation, error)
	DeleteConversation(id int64) error
	// TouchConversation 把会话的 updated_at 推到当前（侧边栏按它倒序排）
	TouchConversation(id int64) error
	ListMessages(convID int64) ([]store.Message, error)
	ListMessagesTail(convID int64, limit int) ([]store.Message, int, error)
	ListMessagesBefore(convID, beforeID int64, limit int) ([]store.Message, error)
	GetMessage(id int64) (*store.Message, error)
	CreateMessage(m store.Message) (*store.Message, error)
	UpdateMessageStream(id int64, reasoning, content string) error
	FinishMessage(id int64, status, reasoning, content, errMsg string, u store.Usage, searchLog, toolRounds string) error
	UpdateMessageContent(id int64, content string) (*store.Message, error)
	UpdateMessageReasoning(id int64, reasoning string) (*store.Message, error)
	RestoreMessage(id int64) (*store.Message, error)
	RestoreMessageReasoning(id int64) (*store.Message, error)
	DeleteMessage(id int64) error
	DeleteMessagesFrom(convID, id int64, inclusive bool) (int64, error)
	AutoTitle(convID int64) (string, bool, error)
	FirstUserMessage(convID int64) (string, error)
	SetConversationSummary(id int64, summary string, beforeMsgID int64, contextEstimate int) error
	GetSetting(userID int64, key string) (string, error)
	// GetModelFor 必须带供应商：多供应商下同名模型是两条不同记录
	GetModelFor(providerID int64, modelID string) (*store.Model, error)
	FirstModelOfProvider(providerID int64) (*store.Model, error)
	// ResolveProvider 给会话用的 provider_id 兜底（供应商被删了要回退到该用户默认）
	ResolveProvider(userID, id int64) (*store.Provider, error)
	GeneratingConvIDs() (map[int64]bool, error)
	// 附件（图片）
	CountAttachmentsOwnedBy(ids []int64, userID int64) (int, error)
	AttachToMessage(messageID int64, attachmentIDs []int64) error
	CreateAttachment(a store.Attachment) (*store.Attachment, error)
	GetAttachmentInConv(id, convID int64) (*store.Attachment, error)
	ListAttachments(messageID int64) ([]store.Attachment, error)
	AttachmentsByMessageIDs(ids []int64) (map[int64][]store.Attachment, error)
	GetMessageWithAttachments(id int64) (*store.Message, error)
	// 文件（文档）
	CountFilesOwnedBy(ids []int64, userID int64) (int, error)
	CreateFile(f store.File) (*store.File, error)
	GetFileInConv(id, convID int64) (*store.File, error)
	AttachFileToMessage(messageID int64, fileIDs []int64) error
	FilesByMessageIDs(ids []int64) (map[int64][]store.File, error)
	ListFiles(messageID int64) ([]store.File, error)
}

type cmdKind int

const (
	cmdState cmdKind = iota
	cmdSend
	cmdStop
	cmdRegenerate
	cmdEditMessage
	cmdEditReasoning
	cmdRestoreMessage
	cmdRestoreReasoning
	cmdDeleteMessage
	cmdPatchConv
	cmdDeleteConv
	cmdShutdown
	cmdCompact
)

type command struct {
	kind          cmdKind
	content       string
	providerID    int64
	modelID       string
	effort        string
	effortSet     bool
	msgID         int64
	attachmentIDs []int64
	fileIDs       []int64
	patch         store.ConversationPatch
	reply         chan reply
}

type reply struct {
	res result
	err error
}

type result struct {
	userMsgID int64
	asstMsgID int64
	message   *store.Message
	conv      *store.Conversation
	state     *State
	stopped   bool
}

// Manager 持有 convID → Session 的映射，把命令分发给对应会话的 goroutine。
//
// 注意 Manager 自己只保护 sessions 这张表，会话状态一律由会话 goroutine 独占，
// 所以整个后端没有任何一处共享可变状态需要靠锁去争。
type Manager struct {
	mu       sync.Mutex
	sessions map[int64]*Session
	st       Store
	bus      *Bus
	clientFn ClientFactory
	titler   *Titler
	// media 是 misc 目录的句柄（图片读写给后端用）；可以为 nil（不带图也能跑）
	media *media.Store
	// search 是联网搜索的执行端；nil = 不启用联网搜索（会话上的开关不生效）
	search *search.Client
	// sandbox 是 Python 代码执行的执行端；nil = 不启用（会话上的开关不生效）
	sandbox *sandbox.Client
	// imagegen 是图像生成的执行端；nil = 不启用
	imagegen *imagegen.Client
	// compactThreshold 是上下文压缩的触发阈值（0~1；0 = 禁用）。
	// 一轮的 prompt_tokens / context_len 达到它就在后台压缩旧历史为摘要。
	compactThreshold float64
	// compacting 记录哪些会话正在压缩（内存态）：期间阻塞该会话的发送，
	// 并通过 conv 事件/state 显式下发给前端。别的会话不受影响。
	compacting map[int64]bool
}

func NewManager(st Store, bus *Bus, cf ClientFactory) *Manager {
	return NewManagerWithMedia(st, bus, cf, nil)
}

// NewManagerWithMedia 带上图片存储。media 传 nil 表示这个部署不启用图片。
func NewManagerWithMedia(st Store, bus *Bus, cf ClientFactory, mediaStore *media.Store) *Manager {
	m := &Manager{
		sessions:   map[int64]*Session{},
		st:         st,
		bus:        bus,
		clientFn:   cf,
		media:      mediaStore,
		compacting: map[int64]bool{},
	}
	// 起标题是后台小任务，不走会话 mailbox，避免占住会话 goroutine
	m.titler = NewTitler(st, bus, m.ActiveByConv)
	return m
}

// WithSearch 注入联网搜索的执行端（SearXNG）。nil = 这个部署不启用联网搜索。
func (m *Manager) WithSearch(c *search.Client) *Manager {
	m.search = c
	return m
}

// SearchAvailable 报告联网搜索是否可用（前端据此显示/隐藏开关）。
func (m *Manager) SearchAvailable() bool { return m.search != nil }

// WithSandbox 注入 Python 代码执行的执行端（pybox 沙箱）。nil = 不启用。
func (m *Manager) WithSandbox(c *sandbox.Client) *Manager {
	m.sandbox = c
	return m
}

// SandboxAvailable 报告 Python 执行是否可用（前端据此显示/隐藏开关）。
func (m *Manager) SandboxAvailable() bool { return m.sandbox != nil }

// WithImageGen 注入图像生成的执行端。nil = 不启用。
func (m *Manager) WithImageGen(c *imagegen.Client) *Manager {
	m.imagegen = c
	return m
}

// WithCompactThreshold 设置上下文压缩阈值（0~1；0 = 禁用）。
func (m *Manager) WithCompactThreshold(t float64) *Manager {
	m.compactThreshold = t
	return m
}

// setCompacting 标记/解除一个会话的压缩中状态（compacting map 由 mu 保护）。
func (m *Manager) setCompacting(convID int64, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if on {
		m.compacting[convID] = true
	} else {
		delete(m.compacting, convID)
	}
}

// isCompacting 报告该会话是否正在压缩（期间阻塞发送）。
func (m *Manager) IsCompacting(convID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.compacting[convID]
}

// CompactThreshold 返回上下文压缩阈值（前端展示用）。
func (m *Manager) CompactThreshold() float64 { return m.compactThreshold }

func (m *Manager) Bus() *Bus { return m.bus }

// Store 返回持久化接口（分页等只读路径用）。
func (m *Manager) Store() Store { return m.st }

// clientFor 按供应商构造上游客户端（多租户：只能解析到 userID 自己的供应商）。
// providerID <= 0 时回退到该用户的默认供应商（会话的供应商被删了会走到这里）。
func (m *Manager) clientFor(userID, providerID int64) (*llm.Client, error) {
	p, err := m.st.ResolveProvider(userID, providerID)
	if err != nil {
		return nil, newErr("internal", err.Error())
	}
	if p == nil || strings.TrimSpace(p.BaseURL) == "" {
		return nil, newErr("no_provider", "还没有配置任何供应商，请先在设置里添加一个")
	}
	c, err := m.clientFn(userID, p.ID)
	if err != nil {
		var se *Error
		if errors.As(err, &se) {
			return nil, se
		}
		return nil, newErr("no_provider", err.Error())
	}
	return c, nil
}

// ownConversation 多租户的硬边界：一切命令入口先过这里。
// 会话不是当前用户的，一律报 not_found——不暴露「有这条但是别人的」。
func (m *Manager) ownConversation(convID, userID int64) error {
	conv, err := m.st.GetConversation(convID)
	if err != nil {
		return newErr("internal", err.Error())
	}
	if conv == nil || conv.UserID != userID {
		return errNotFound
	}
	return nil
}

// getOrCreate 惰性创建会话 goroutine（双检加锁，避免并发重复创建）。
func (m *Manager) getOrCreate(convID int64) (*Session, error) {
	m.mu.Lock()
	if s, ok := m.sessions[convID]; ok {
		m.mu.Unlock()
		return s, nil
	}
	m.mu.Unlock()

	conv, err := m.st.GetConversation(convID)
	if err != nil {
		return nil, newErr("internal", err.Error())
	}
	if conv == nil {
		return nil, errNotFound
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[convID]; ok {
		return s, nil
	}
	s := newSession(convID, m, conv)
	m.sessions[convID] = s
	go s.run()
	return s, nil
}

func (m *Manager) drop(convID int64, s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.sessions[convID]; ok && (s == nil || cur == s) {
		delete(m.sessions, convID)
	}
}

// call 把命令投进目标会话的 mailbox 并等回执。
//
// 这里有个容易踩的坑：命令投进缓冲 mailbox 之后，会话可能刚好空闲退出，
// 命令就永远没人处理了。所以除了等回执，还要同时盯着 s.done，
// 一旦会话先退出就重试一次（重试会重建会话）。
func (m *Manager) call(ctx context.Context, convID int64, cmd command) (result, error) {
	for attempt := 0; attempt < 2; attempt++ {
		s, err := m.getOrCreate(convID)
		if err != nil {
			return result{}, err
		}
		cmd.reply = make(chan reply, 1)

		select {
		case s.mailbox <- cmd:
		case <-s.done:
			continue
		case <-ctx.Done():
			return result{}, ctx.Err()
		case <-time.After(5 * time.Second):
			return result{}, newErr("internal", "会话繁忙，请稍后重试")
		}

		// 先看一眼回执：会话处理完立刻退出的情况（例如删除会话）下，
		// 回执和 s.done 会同时就绪，优先取回执。
		select {
		case r := <-cmd.reply:
			if r.err == nil {
				r.res.message = messageWithAttachments(m.st, r.res.message)
			}
			return r.res, r.err
		default:
		}

		select {
		case r := <-cmd.reply:
			if r.err == nil {
				r.res.message = messageWithAttachments(m.st, r.res.message)
			}
			return r.res, r.err
		case <-s.done:
			continue
		case <-ctx.Done():
			return result{}, ctx.Err()
		case <-time.After(30 * time.Second):
			return result{}, newErr("internal", "会话响应超时")
		}
	}
	return result{}, newErr("internal", "会话不可用，请重试")
}

// ---------- 对外 API ----------

// State 返回会话的权威状态。
func (m *Manager) State(ctx context.Context, userID, convID int64) (*State, error) {
	if err := m.ownConversation(convID, userID); err != nil {
		return nil, err
	}
	m.mu.Lock()
	_, live := m.sessions[convID]
	m.mu.Unlock()

	if live {
		// 会话在内存里 → 必须走 mailbox：
		// 「正在生成」的那条消息在内存里是最新的，落库副本最多滞后 250ms，
		// 直接读库会让刷新页面的用户丢掉最后一小段 CoT。
		res, err := m.call(ctx, convID, command{kind: cmdState})
		if err != nil {
			return nil, err
		}
		if res.state == nil {
			return nil, errNotFound
		}
		return res.state, nil
	}

	// 会话不在内存里 → 不可能有正在进行的生成，直接读库。
	// seq 必须在读库之前取：这样读库期间发布的（幂等的）事件即使被重复
	// 应用也不会造成偏差，而更早的事件一定已经体现在这次读库里。
	seq := m.bus.Seq()
	conv, err := m.st.GetConversation(convID)
	if err != nil {
		return nil, newErr("internal", err.Error())
	}
	if conv == nil {
		return nil, errNotFound
	}
	msgs, total, err := m.st.ListMessagesTail(convID, stateTailMessages)
	if err != nil {
		return nil, newErr("internal", err.Error())
	}
	// 读库路径也要填附件：否则带图消息在会话空闲回收后刷新就丢图
	FillMessagesAttachments(m.st, msgs)
	return &State{Conv: conv, Seq: seq, Messages: msgs, HasMore: total > len(msgs)}, nil
}

// SendMessage 投递一条用户消息。会话忙时返回 busy 错误，消息被丢弃。
// providerID/modelID 传 0/空 表示用会话自身的配置；attachmentIDs 是已上传的图片，
// fileIDs 是已上传的文档（/api/files 返回的 id）。
func (m *Manager) SendMessage(ctx context.Context, userID, convID int64, content string, providerID int64, modelID string, effort *string, attachmentIDs []int64, fileIDs []int64) (int64, int64, error) {
	if err := m.ownConversation(convID, userID); err != nil {
		return 0, 0, err
	}
	cmd := command{kind: cmdSend, content: content, providerID: providerID, modelID: modelID, attachmentIDs: attachmentIDs, fileIDs: fileIDs}
	if effort != nil {
		cmd.effort, cmd.effortSet = *effort, true
	}
	res, err := m.call(ctx, convID, cmd)
	if err != nil {
		return 0, 0, err
	}
	return res.userMsgID, res.asstMsgID, nil
}

func (m *Manager) Stop(ctx context.Context, userID, convID int64) (bool, error) {
	if err := m.ownConversation(convID, userID); err != nil {
		return false, err
	}
	res, err := m.call(ctx, convID, command{kind: cmdStop})
	if err != nil {
		return false, err
	}
	return res.stopped, nil
}

func (m *Manager) Regenerate(ctx context.Context, userID, msgID int64) error {
	convID, err := m.convOfMessageForUser(msgID, userID)
	if err != nil {
		return err
	}
	_, err = m.call(ctx, convID, command{kind: cmdRegenerate, msgID: msgID})
	return err
}

// Compact 手动触发一次上下文压缩（用户点「压缩当前上下文」按钮）。
// 压缩是异步的：命令返回时压缩刚开始，进度通过 conv 状态（compacting）
// 和 notice 广播到前端。
func (m *Manager) Compact(ctx context.Context, userID, convID int64) error {
	if err := m.ownConversation(convID, userID); err != nil {
		return err
	}
	_, err := m.call(ctx, convID, command{kind: cmdCompact})
	return err
}

func (m *Manager) EditMessage(ctx context.Context, userID, msgID int64, content string) (*store.Message, error) {
	convID, err := m.convOfMessageForUser(msgID, userID)
	if err != nil {
		return nil, err
	}
	res, err := m.call(ctx, convID, command{kind: cmdEditMessage, msgID: msgID, content: content})
	if err != nil {
		return nil, err
	}
	return res.message, nil
}

// EditMessageReasoning 编辑 CoT（reasoning 原文会被保留，可还原）。
func (m *Manager) EditMessageReasoning(ctx context.Context, userID, msgID int64, reasoning string) (*store.Message, error) {
	convID, err := m.convOfMessageForUser(msgID, userID)
	if err != nil {
		return nil, err
	}
	res, err := m.call(ctx, convID, command{kind: cmdEditReasoning, msgID: msgID, content: reasoning})
	if err != nil {
		return nil, err
	}
	return res.message, nil
}

func (m *Manager) RestoreMessage(ctx context.Context, userID, msgID int64) (*store.Message, error) {
	convID, err := m.convOfMessageForUser(msgID, userID)
	if err != nil {
		return nil, err
	}
	res, err := m.call(ctx, convID, command{kind: cmdRestoreMessage, msgID: msgID})
	if err != nil {
		return nil, err
	}
	return res.message, nil
}

// RestoreMessageReasoning 还原 CoT 到模型原文。
func (m *Manager) RestoreMessageReasoning(ctx context.Context, userID, msgID int64) (*store.Message, error) {
	convID, err := m.convOfMessageForUser(msgID, userID)
	if err != nil {
		return nil, err
	}
	res, err := m.call(ctx, convID, command{kind: cmdRestoreReasoning, msgID: msgID})
	if err != nil {
		return nil, err
	}
	return res.message, nil
}

func (m *Manager) DeleteMessage(ctx context.Context, userID, msgID int64) error {
	convID, err := m.convOfMessageForUser(msgID, userID)
	if err != nil {
		return err
	}
	_, err = m.call(ctx, convID, command{kind: cmdDeleteMessage, msgID: msgID})
	return err
}

func (m *Manager) PatchConversation(ctx context.Context, userID, convID int64, patch store.ConversationPatch) (*store.Conversation, error) {
	if err := m.ownConversation(convID, userID); err != nil {
		return nil, err
	}
	// provider_id 换绑必须是自己的供应商，否则等于偷用别人的 apikey
	if patch.ProviderID != nil && *patch.ProviderID > 0 {
		if err := m.ownProvider(*patch.ProviderID, userID); err != nil {
			return nil, err
		}
	}
	res, err := m.call(ctx, convID, command{kind: cmdPatchConv, patch: patch})
	if err != nil {
		return nil, err
	}
	return res.conv, nil
}

func (m *Manager) DeleteConversation(ctx context.Context, userID, convID int64) error {
	if err := m.ownConversation(convID, userID); err != nil {
		return err
	}
	_, err := m.call(ctx, convID, command{kind: cmdDeleteConv})
	return err
}

// ActiveByConv 返回该会话此刻是否正在生成。
//
// 注意不能直接读会话的 s.gen —— 那个字段由会话 goroutine 独占，外部读就是数据竞争。
// 改查数据库里「有没有 status=streaming 的消息」：助手占位消息在发起生成**之前**
// 就已落库、生成结束才改状态，所以这个判断任何时刻都是准的。
func (m *Manager) ActiveByConv(convID int64) bool {
	gen, err := m.st.GeneratingConvIDs()
	if err != nil {
		return false
	}
	return gen[convID]
}

// convOfMessageForUser 把 msgID 解析成 convID 并顺带校验会话归属。
// 消息/会话是别人的一律 not_found。
func (m *Manager) convOfMessageForUser(msgID, userID int64) (int64, error) {
	msg, err := m.st.GetMessage(msgID)
	if err != nil {
		return 0, newErr("internal", err.Error())
	}
	if msg == nil {
		return 0, errNotFound
	}
	if err := m.ownConversation(msg.ConvID, userID); err != nil {
		return 0, err
	}
	return msg.ConvID, nil
}

// ownProvider 校验供应商归属（PATCH 会话换绑供应商时用）。
func (m *Manager) ownProvider(providerID, userID int64) error {
	p, err := m.st.ResolveProvider(userID, providerID)
	if err != nil {
		return newErr("internal", err.Error())
	}
	if p == nil || p.ID != providerID {
		return newErr("not_found", "供应商不存在")
	}
	return nil
}

// Shutdown 让所有会话把正在进行的生成收尾落盘后退出。
// 进程退出前调用，避免丢掉最后一段还没落库的 CoT。
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(s *Session) {
			defer wg.Done()
			select {
			case s.mailbox <- command{kind: cmdShutdown, reply: make(chan reply, 1)}:
			case <-s.done:
				return
			case <-ctx.Done():
				return
			}
			select {
			case <-s.done:
			case <-ctx.Done():
			}
		}(s)
	}
	wg.Wait()
}
