package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"aiassistant/internal/memory"
)

func (s *Server) memoryManager(w http.ResponseWriter) *memory.Manager {
	m := s.mgr.Memory()
	if m == nil {
		writeErr(w, http.StatusServiceUnavailable, "memory_unavailable", "记忆功能未启用")
	}
	return m
}

func (s *Server) handleGetConversationMemory(w http.ResponseWriter, r *http.Request) {
	m := s.memoryManager(w)
	if m == nil {
		return
	}
	convID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	status, err := m.SessionStatus(userOf(r).ID, convID)
	if err != nil {
		memoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handlePutConversationMemory(w http.ResponseWriter, r *http.Request) {
	m := s.memoryManager(w)
	if m == nil {
		return
	}
	convID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "enabled 不能为空")
		return
	}
	status, err := m.SetSessionEnabled(userOf(r).ID, convID, *body.Enabled)
	if err != nil {
		memoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleGetMemorySettings(w http.ResponseWriter, r *http.Request) {
	m := s.memoryManager(w)
	if m == nil {
		return
	}
	background, err := m.BackgroundSettings(userOf(r).ID)
	if err != nil {
		memoryError(w, err)
		return
	}
	maintenance, err := m.MaintenanceStatus(userOf(r).ID)
	if err != nil {
		memoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"background":  background,
		"maintenance": maintenance,
		"schedule":    "每天 04:00（Asia/Shanghai）",
	})
}

func (s *Server) handlePutMemorySettings(w http.ResponseWriter, r *http.Request) {
	m := s.memoryManager(w)
	if m == nil {
		return
	}
	var body memory.BackgroundSettings
	if !decodeJSON(w, r, &body) {
		return
	}
	body.ModelID = strings.TrimSpace(body.ModelID)
	body.Effort = strings.TrimSpace(body.Effort)
	if body.ProviderID <= 0 {
		body = memory.BackgroundSettings{Effort: "low"}
	}
	if err := m.SetBackgroundSettings(userOf(r).ID, body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	s.handleGetMemorySettings(w, r)
}

func (s *Server) handleListMemoryArchives(w http.ResponseWriter, r *http.Request) {
	m := s.memoryManager(w)
	if m == nil {
		return
	}
	list, err := m.ListArchives(userOf(r).ID)
	if err != nil {
		memoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetMemoryArchive(w http.ResponseWriter, r *http.Request) {
	m := s.memoryManager(w)
	if m == nil {
		return
	}
	convID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	offset, limit := 0, 30000
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "bad_request", "offset 必须是非负整数")
			return
		}
		offset = n
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 30000 {
			writeErr(w, http.StatusBadRequest, "bad_request", "limit 必须在 1～30000 之间")
			return
		}
		limit = n
	}
	content, total, more, err := m.ReadArchive(userOf(r).ID, convID, offset, limit)
	if err != nil {
		memoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"conv_id": convID, "content": content, "total_chars": total,
		"next_offset": offset + len([]rune(content)), "has_more": more,
	})
}

func memoryError(w http.ResponseWriter, err error) {
	if strings.Contains(err.Error(), "不存在") {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeErr(w, http.StatusInternalServerError, "internal", err.Error())
}
