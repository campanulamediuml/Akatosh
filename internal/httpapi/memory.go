package httpapi

import (
	"net/http"
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

func memoryError(w http.ResponseWriter, err error) {
	if strings.Contains(err.Error(), "不存在") {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeErr(w, http.StatusInternalServerError, "internal", err.Error())
}
