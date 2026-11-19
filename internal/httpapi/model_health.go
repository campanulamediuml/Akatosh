package httpapi

import "net/http"

func (s *Server) handleGetModelHealth(w http.ResponseWriter, r *http.Request) {
	if s.modelHealth == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "models": map[string]any{}})
		return
	}
	writeJSON(w, http.StatusOK, s.modelHealth.Snapshot(userOf(r).ID))
}

// handlePutUserModelHealth 允许用户切换自己的检测，也允许管理员代管。
func (s *Server) handlePutUserModelHealth(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	current := userOf(r)
	if current == nil || (!current.IsAdmin && current.ID != id) {
		writeErr(w, http.StatusForbidden, "forbidden", "只能修改自己的模型检测设置")
		return
	}
	if s.modelHealth == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "模型检测未启用")
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "缺少 enabled")
		return
	}
	user, err := s.st.GetUser(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if user == nil {
		writeErr(w, http.StatusNotFound, "not_found", "用户不存在")
		return
	}
	if err := s.modelHealth.SetEnabled(id, *body.Enabled); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": *body.Enabled})
}

func (s *Server) handleGetModelHealthConcurrency(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if s.modelHealth == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "模型检测未启用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"concurrency": s.modelHealth.Concurrency()})
}

func (s *Server) handlePutModelHealthConcurrency(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if s.modelHealth == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "模型检测未启用")
		return
	}
	var body struct {
		Concurrency *int `json:"concurrency"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Concurrency == nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "缺少 concurrency")
		return
	}
	if err := s.modelHealth.SetConcurrency(*body.Concurrency); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"concurrency": *body.Concurrency})
}
