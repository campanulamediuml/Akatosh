package session

import (
	"sync"

	"aiassistant/internal/store"
)

// ---------- 事件载荷 ----------
// 每个载荷都带 seq：它是后端全局单调递增的发布序号，也是前端与后端对账的
// 唯一依据。前端拿到 state 快照时会把 seq 当作高水位，丢弃所有 seq 更小的
// 事件，从而保证「不重不漏」。

type HelloPayload struct {
	Seq        int64  `json:"seq"`
	ClientID   string `json:"client_id"`
	ServerTime int64  `json:"server_time"`
}

type DeltaPayload struct {
	Seq       int64  `json:"seq"`
	ConvID    int64  `json:"conv_id"`
	MessageID int64  `json:"message_id"`
	Channel   string `json:"channel"` // reasoning | content | search（联网搜索状态）
	Text      string `json:"text"`
}

type MessagePayload struct {
	Seq       int64          `json:"seq"`
	ConvID    int64          `json:"conv_id"`
	Action    string         `json:"action"` // created | updated | deleted | truncated
	MessageID int64          `json:"message_id"`
	Message   *store.Message `json:"message,omitempty"`
}

type ConvPayload struct {
	Seq    int64               `json:"seq"`
	ConvID int64               `json:"conv_id"`
	Action string              `json:"action"` // updated | deleted
	Conv   *store.Conversation `json:"conv,omitempty"`
}

type NoticePayload struct {
	Seq    int64  `json:"seq"`
	ConvID int64  `json:"conv_id,omitempty"`
	Level  string `json:"level"` // info | warn | error
	Text   string `json:"text"`
}

// 事件类型
const (
	EventHello   = "hello"
	EventDelta   = "delta"
	EventMessage = "message"
	EventConv    = "conv"
	EventNotice  = "notice"
)

// 消息动作
const (
	ActionCreated   = "created"
	ActionUpdated   = "updated"
	ActionDeleted   = "deleted"
	ActionTruncated = "truncated"
)

// Event 是总线上的一个事件。
//
// UserID 是事件的属主（多租户分发边界）：>0 时只推给同 userID 的订阅者，
// 0 表示全局事件（广播给所有连接——目前只用于后端重启这类系统级信号）。
type Event struct {
	Type    string
	Seq     int64
	UserID  int64
	Payload any
}

// ---------- 事件总线 ----------
//
// 后端与前端之间只有一条 SSE 长连接，所以这里是一个全局广播器：
// 所有会话发布的事件都汇到这里，再扇出给每个已连接的前端。

// clientBuffer 是单个前端连接的缓冲深度。按每 token 一个 delta 估算，
// 4096 个事件大约相当于几十秒的生成量。本地连接真落后到这个程度，
// 说明连接已经不正常了，直接断开让它重连重同步反而更安全。
const clientBuffer = 4096

type busClient struct {
	id     int64
	userID int64 // 这条连接属于哪个用户（订阅时绑定）
	ch     chan Event
	once   sync.Once
	closed chan struct{}
}

func (c *busClient) trySend(ev Event) {
	// 多租户：带属主的事件只发给属主的连接，其它连接连排队都不排。
	// 这是隔离的硬边界——事件里带着会话内容，跨用户投递就是泄露。
	if ev.UserID != 0 && ev.UserID != c.userID {
		return
	}
	select {
	case c.ch <- ev:
	default:
		// 客户端跟不上：断开连接。前端会自动重连并重新拉 state，
		// 因为「前端的真相永远来自后端」，重同步能纠正一切偏差。
		c.kill()
	}
}

func (c *busClient) kill() {
	c.once.Do(func() {
		close(c.closed)
		close(c.ch)
	})
}

type Bus struct {
	mu      sync.Mutex
	clients map[int64]*busClient
	nextID  int64
	seq     int64
}

func NewBus() *Bus {
	return &Bus{clients: map[int64]*busClient{}}
}

// Seq 返回当前发布序号。用于「会话不在内存里、直接读库」时给快照打高水位：
// 必须在读库**之前**取，这样读库期间发布的幂等事件即使被重复应用也无害。
func (b *Bus) Seq() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

// Subscribe 注册一个前端连接，并绑定这个连接所属的用户。
// 之后带属主的事件只会发给同 userID 的连接（见 trySend）。
// 返回的 cancel 必须被调用（SSE 处理函数 defer 即可），否则会泄漏。
func (b *Bus) Subscribe(userID int64) (id int64, ch <-chan Event, closed <-chan struct{}, cancel func()) {
	b.mu.Lock()
	b.nextID++
	c := &busClient{
		id:     b.nextID,
		userID: userID,
		ch:     make(chan Event, clientBuffer),
		closed: make(chan struct{}),
	}
	b.clients[c.id] = c
	b.mu.Unlock()

	return c.id, c.ch, c.closed, func() {
		b.mu.Lock()
		delete(b.clients, c.id)
		b.mu.Unlock()
		c.kill()
	}
}

// Publish 发布一个事件，返回分配的 seq。mk 负责把 seq 填进具体载荷里。
// userID 是事件属主：>0 只推给同用户连接；0 广播给全部连接（系统级事件）。
func (b *Bus) Publish(typ string, userID int64, mk func(seq int64) any) int64 {
	b.mu.Lock()
	b.seq++
	seq := b.seq
	ev := Event{Type: typ, Seq: seq, UserID: userID, Payload: mk(seq)}
	targets := make([]*busClient, 0, len(b.clients))
	for _, c := range b.clients {
		targets = append(targets, c)
	}
	b.mu.Unlock()

	for _, c := range targets {
		c.trySend(ev)
	}
	return seq
}

// Clients 返回当前连接数（仅用于日志/诊断）。
func (b *Bus) Clients() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.clients)
}
