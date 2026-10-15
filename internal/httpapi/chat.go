package httpapi

import (
	"net/http"
	"strconv"

	"aiassistant/internal/session"
	"aiassistant/internal/store"
)

// ---------- 会话 ----------

func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListConversations(userOf(r).ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// 正在生成的会话打上标记，左侧列表据此显示脉冲小圆点
	if gen, err := s.st.GeneratingConvIDs(); err == nil {
		for i := range list {
			if gen[list[i].ID] {
				list[i].Generating = true
			}
		}
	}
	// 正在压缩的会话打上标记（内存态，期间阻塞发送）
	for i := range list {
		list[i].Compacting = s.mgr.IsCompacting(list[i].ID)
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateConversation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title      string `json:"title"`
		ProviderID int64  `json:"provider_id"`
		ModelID    string `json:"model_id"`
		Effort     string `json:"effort"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	// provider_id / model_id / effort 省略时由 store 套用设置里的默认值
	conv, err := s.st.CreateConversation(userOf(r).ID, body.Title, body.ProviderID, body.ModelID, body.Effort)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// 新建会话还没有对应的 session goroutine，所以由这里直接广播，
	// 让其它已打开的标签页也能立刻看到。
	s.publishConv(userOf(r).ID, session.ActionUpdated, conv)
	writeJSON(w, http.StatusCreated, conv)
}

func (s *Server) handlePatchConversation(w http.ResponseWriter, r *http.Request) {
	convID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		Title        *string `json:"title"`
		ProviderID   *int64  `json:"provider_id"`
		ModelID      *string `json:"model_id"`
		Effort       *string `json:"effort"`
		Pinned       *bool   `json:"pinned"`
		Archived     *bool   `json:"archived"`
		SystemPrompt *string `json:"system_prompt"`
		WebSearch    *bool   `json:"web_search"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.ProviderID != nil && *body.ProviderID <= 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "provider_id 非法")
		return
	}
	conv, err := s.mgr.PatchConversation(r.Context(), userOf(r).ID, convID, store.ConversationPatch{
		Title:        body.Title,
		ProviderID:   body.ProviderID,
		ModelID:      body.ModelID,
		Effort:       body.Effort,
		Pinned:       body.Pinned,
		Archived:     body.Archived,
		SystemPrompt: body.SystemPrompt,
		WebSearch:    body.WebSearch,
	})
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conv": conv})
}

func (s *Server) handleDeleteConversation(w http.ResponseWriter, r *http.Request) {
	convID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := s.mgr.DeleteConversation(r.Context(), userOf(r).ID, convID); err != nil {
		writeDomainErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleState 返回会话的权威状态：前端所有显示内容都以它为准。
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	convID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	st, err := s.mgr.State(r.Context(), userOf(r).ID, convID)
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// ---------- 命令 ----------

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	convID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		Content       string  `json:"content"`
		ProviderID    int64   `json:"provider_id"`
		ModelID       string  `json:"model_id"`
		Effort        *string `json:"effort"`
		AttachmentIDs []int64 `json:"attachment_ids"`
		FileIDs       []int64 `json:"file_ids"`
		ClientMsgID   string  `json:"client_msg_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	userMsgID, asstID, err := s.mgr.SendMessage(
		r.Context(), userOf(r).ID, convID, body.Content, body.ProviderID, body.ModelID, body.Effort, body.AttachmentIDs, body.FileIDs)
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accepted":             true,
		"user_message_id":      userMsgID,
		"assistant_message_id": asstID,
	})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	convID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	stopped, err := s.mgr.Stop(r.Context(), userOf(r).ID, convID)
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stopped": stopped})
}

func (s *Server) handleRegenerate(w http.ResponseWriter, r *http.Request) {
	msgID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := s.mgr.Regenerate(r.Context(), userOf(r).ID, msgID); err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
}

func (s *Server) handleEditMessage(w http.ResponseWriter, r *http.Request) {
	msgID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		Content   *string `json:"content"`
		Reasoning *string `json:"reasoning"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Content == nil && body.Reasoning == nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "缺少 content 或 reasoning 字段")
		return
	}
	// 一次 PATCH 可以同时改正文和 CoT（reasoning 编辑后会随历史回传给上游）
	var msg *store.Message
	var err error
	if body.Content != nil {
		msg, err = s.mgr.EditMessage(r.Context(), userOf(r).ID, msgID, *body.Content)
		if err != nil {
			writeDomainErr(w, err)
			return
		}
	}
	if body.Reasoning != nil {
		msg, err = s.mgr.EditMessageReasoning(r.Context(), userOf(r).ID, msgID, *body.Reasoning)
		if err != nil {
			writeDomainErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": msg})
}

func (s *Server) handleRestoreMessage(w http.ResponseWriter, r *http.Request) {
	msgID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	msg, err := s.mgr.RestoreMessage(r.Context(), userOf(r).ID, msgID)
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": msg})
}

// handleRestoreMessageReasoning 把 CoT 还原成模型原文（reasoning_original 保留的那份）。
func (s *Server) handleRestoreMessageReasoning(w http.ResponseWriter, r *http.Request) {
	msgID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	msg, err := s.mgr.RestoreMessageReasoning(r.Context(), userOf(r).ID, msgID)
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": msg})
}

func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	msgID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := s.mgr.DeleteMessage(r.Context(), userOf(r).ID, msgID); err != nil {
		writeDomainErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleForkConversation 从一条消息（含）之前的历史全量复制出一个新会话。
// before_message_id 省略 = 复制整个会话。
func (s *Server) handleForkConversation(w http.ResponseWriter, r *http.Request) {
	convID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		BeforeMessageID int64 `json:"before_message_id"`
	}
	// body 可空（整会话 fork）
	_ = decodeJSON(w, r, &body)
	// fork 会全量复制历史（含 CoT），归属必须先校验：只能 fork 自己的会话
	if srcConv, err := s.st.GetConversationForUser(convID, userOf(r).ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	} else if srcConv == nil {
		writeErr(w, http.StatusNotFound, "not_found", "会话不存在")
		return
	}
	conv, err := s.st.ForkConversation(convID, body.BeforeMessageID, userOf(r).ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if conv == nil {
		writeErr(w, http.StatusNotFound, "not_found", "会话不存在")
		return
	}
	s.publishConv(userOf(r).ID, session.ActionUpdated, conv)
	writeJSON(w, http.StatusCreated, map[string]any{"conv": conv})
}

// handleCompactConversation 手动触发上下文压缩：ack 即返回，
// 进度通过 conv 状态（compacting）和 notice 走 SSE 推给前端。
func (s *Server) handleCompactConversation(w http.ResponseWriter, r *http.Request) {
	convID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := s.mgr.Compact(r.Context(), userOf(r).ID, convID); err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true})
}

// handleMarkRead 把会话标为「已看过了」——侧边栏的小红点据此熄灭。
// 前端在切到这个会话、以及它正在被看着时回答落地，都会调一次。
func (s *Server) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	convID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	conv, err := s.st.GetConversationForUser(convID, userOf(r).ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if conv == nil {
		writeErr(w, http.StatusNotFound, "not_found", "会话不存在")
		return
	}
	if err := s.st.MarkConversationRead(convID); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// 广播出去：其它标签页的小红点也要跟着灭
	s.publishConv(userOf(r).ID, session.ActionUpdated, conv)
	writeJSON(w, http.StatusOK, map[string]any{"conv": conv})
}

// publishConv 在会话层之外（新建会话、标记已读）广播会话变更。
// userID 是事件属主：只推给这个用户的连接（多租户）。
func (s *Server) publishConv(userID int64, action string, conv *store.Conversation) {
	var convID int64
	if conv != nil {
		convID = conv.ID
		// Generating 不是数据库字段，从库里读出来恒为 false。
		// 这里按数据库里的真实状态（有没有 streaming 消息）补上，
		// 否则一次「标记已读」就会把正在生成的转圈标记误清掉。
		c := *conv
		if gen, err := s.st.GeneratingConvIDs(); err == nil {
			c.Generating = gen[convID]
		}
		conv = &c
	}
	s.mgr.Bus().Publish(session.EventConv, userID, func(seq int64) any {
		return session.ConvPayload{Seq: seq, ConvID: convID, Action: action, Conv: conv}
	})
}

// handleListMessages 向上翻页：GET /api/conversations/{id}/messages?before=&limit=
// 返回 before 之前（不含）的更早消息，正序。附件一并填好 URL。
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	convID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	before, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if err != nil || before <= 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "缺少合法的 before 参数")
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	if conv, err := s.st.GetConversationForUser(convID, userOf(r).ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	} else if conv == nil {
		writeErr(w, http.StatusNotFound, "not_found", "会话不存在")
		return
	}
	msgs, err := s.st.ListMessagesBefore(convID, before, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	session.FillMessagesAttachments(s.mgr.Store(), msgs)
	writeJSON(w, http.StatusOK, msgs)
}
