package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"aiassistant/internal/session"
	"aiassistant/internal/store"
)

// ---------- 当前用户 / 用户管理 ----------

// handleMe 返回当前登录用户（前端据此决定显不显示「用户管理」入口）。
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       u.ID,
		"name":     u.Name,
		"is_admin": u.IsAdmin,
	})
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	list, err := s.st.ListUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleCreateUser 建用户并返回**明文令牌**（只在这一刻可见，库里只存哈希）。
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "缺少用户名")
		return
	}
	token := GenerateToken()
	if token == "" {
		writeErr(w, http.StatusInternalServerError, "internal", "生成令牌失败")
		return
	}
	u, err := s.st.CreateUser(body.Name, store.HashToken(token), false)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"user":  u,
		"token": token, // 仅此一次可见，提醒管理员当场复制
	})
}

// handleRotateUserToken 重置用户令牌：旧令牌立即失效，新令牌只返回这一次。
func (s *Server) handleRotateUserToken(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	token := GenerateToken()
	if token == "" {
		writeErr(w, http.StatusInternalServerError, "internal", "生成令牌失败")
		return
	}
	found, err := s.st.RotateUserToken(id, store.HashToken(token))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "not_found", "用户不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token})
}

// handleDeleteUser 删除用户（连带其名下会话/文件/设置/供应商）。
// 不能删自己，也不能删最后一个管理员。
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if id == userOf(r).ID {
		writeErr(w, http.StatusBadRequest, "bad_request", "不能删除当前登录的用户（自己）")
		return
	}
	if err := s.st.DeleteUser(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "用户不存在")
			return
		}
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// 广播给被删用户的连接：它们下一次 SSE 重连/请求会 401，前端弹回令牌框
	s.mgr.Bus().Publish(session.EventNotice, id, func(seq int64) any {
		return session.NoticePayload{Seq: seq, Level: "error", Text: "这个账号已被管理员删除"}
	})
	w.WriteHeader(http.StatusNoContent)
}
