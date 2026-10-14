package httpapi

import (
	"fmt"
	"net/http"
	"time"

	"aiassistant/internal/session"
	"aiassistant/internal/store"
)

const heartbeatInterval = 15 * time.Second

// handleStream 是前后端之间**唯一**的长连接。
//
// 所有会话的所有事件都从这里下来，靠事件里的 conv_id / message_id 区分。
// 这样前端不需要为每次生成开流，也就不会出现「多条流各自维护一份状态、
// 刷新后对不上」的问题。
//
// 客户端断开只是断开订阅：会话 goroutine 完全不受影响，生成继续，
// 内容继续落库。前端重连后重新拉一次 state 就完全追平。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	sw, err := newSSEWriter(w)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	id, ch, closed, cancel := s.mgr.Bus().Subscribe()
	defer cancel()

	_ = sw.Event(session.EventHello, session.HelloPayload{
		Seq:        s.mgr.Bus().Seq(),
		ClientID:   fmt.Sprintf("c-%d", id),
		ServerTime: store.NowMs(),
	})

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				// 客户端跟不上，被总线主动断开。前端会自动重连并重新拉 state。
				return
			}
			if err := sw.Event(ev.Type, ev.Payload); err != nil {
				return
			}
		case <-closed:
			return
		case <-ticker.C:
			sw.Comment("ping")
		case <-ctx.Done():
			return
		}
	}
}
