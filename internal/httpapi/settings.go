package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aiassistant/internal/llm"
	"aiassistant/internal/session"
	"aiassistant/internal/store"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"version":        s.version,
		"stream_clients": s.mgr.Bus().Clients(),
	})
}

// providerView 是供应商对外的样子。**永远不含明文 key。**
func providerView(p *store.Provider) map[string]any {
	if p == nil {
		return nil
	}
	return map[string]any{
		"id":                  p.ID,
		"name":                p.Name,
		"base_url":            p.BaseURL,
		"normalized_base_url": llm.NormalizeBaseURL(p.BaseURL),
		"api_key_set":         p.APIKey != "",
		"api_key_hint":        maskKey(p.APIKey),
		"model_count":         p.ModelCount,
		"created_at":          p.CreatedAt,
		"updated_at":          p.UpdatedAt,
	}
}

func providerViews(list []store.Provider) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for i := range list {
		out = append(out, providerView(&list[i]))
	}
	return out
}

// ---------- 供应商 CRUD ----------

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListProviders()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, providerViews(list))
}

func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string `json:"name"`
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	base, ok := validBaseURL(body.BaseURL)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "API 地址不能为空，且必须以 http:// 或 https:// 开头")
		return
	}
	p, err := s.st.CreateProvider(strings.TrimSpace(body.Name), base, strings.TrimSpace(body.APIKey))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.publishProvider()
	writeJSON(w, http.StatusCreated, providerView(p))
}

func (s *Server) handlePatchProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		Name    string `json:"name"`
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.BaseURL) != "" {
		if _, ok := validBaseURL(body.BaseURL); !ok {
			writeErr(w, http.StatusBadRequest, "bad_request", "API 地址必须以 http:// 或 https:// 开头")
			return
		}
	}
	p, err := s.st.UpdateProvider(id, body.Name, strings.TrimSpace(body.BaseURL), body.APIKey)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "供应商不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, providerView(p))
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := s.st.DeleteProvider(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "供应商不存在")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.publishProvider()
	w.WriteHeader(http.StatusNoContent)
}

// publishProvider 供应商变了会连带影响「默认值」与各会话可选的模型，
// 推一个 notice 让所有标签页重新拉设置（比逐个会话推 conv 事件简单可靠）。
func (s *Server) publishProvider() {
	s.mgr.Bus().Publish(session.EventNotice, func(seq int64) any {
		return session.NoticePayload{
			Seq:   seq,
			Level: "info",
			Text:  "provider-changed",
		}
	})
}

// ---------- 单个供应商的操作 ----------

// handleFetchModels 代理请求该供应商的 /models，把模型列表拉回来入库。
// 由后端代理而不是前端直连：既避开 CORS，也让 API Key 不离开后端。
func (s *Server) handleFetchModels(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	provider, err := s.st.GetProviderByID(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if provider == nil {
		writeErr(w, http.StatusNotFound, "not_found", "供应商不存在")
		return
	}
	if strings.TrimSpace(provider.BaseURL) == "" {
		writeErr(w, http.StatusBadRequest, "no_provider", "这家供应商还没填 API 地址")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	cli := llm.New(provider.BaseURL, provider.APIKey)
	remote, err := cli.ListModels(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "upstream_error", upstreamMessage(err))
		return
	}
	if len(remote) == 0 {
		writeErr(w, http.StatusBadGateway, "upstream_error", "上游返回了空的模型列表")
		return
	}

	models := make([]store.Model, 0, len(remote))
	for _, rm := range remote {
		models = append(models, store.Model{
			ProviderID:  provider.ID,
			ModelID:     rm.ID,
			DisplayName: rm.ID,
			Reasoning:   rm.Reasoning,
			Efforts:     rm.Efforts,
			ContextLen:  rm.ContextLen,
		})
	}
	added, err := s.st.UpsertModels(provider.ID, models)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// 还没设过默认模型（或者设的那个不属于这家）就自动选一个，省得用户再点一次
	s.ensureDefaultModel(provider.ID)

	saved, err := s.st.ListModels(provider.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":  len(remote),
		"added":  added,
		"models": saved,
	})
}

// ensureDefaultModel 让设置里的默认模型落到「真的属于这家供应商」的模型上。
func (s *Server) ensureDefaultModel(providerID int64) {
	cur, _ := s.st.GetSetting(store.SettingDefaultModel)
	defProvider, _ := s.st.DefaultProvider()
	// 只有默认供应商才有资格定默认模型
	if defProvider == nil || defProvider.ID != providerID {
		return
	}
	if cur != "" {
		if m, err := s.st.GetModelFor(providerID, cur); err == nil && m != nil {
			return // 已经是这家的合法模型
		}
	}
	if m, err := s.st.FirstModelOfProvider(providerID); err == nil && m != nil {
		_ = s.st.SetSetting(store.SettingDefaultModel, m.ModelID)
	}
}

// handleTestProvider 发一个 max_tokens=1 的最小请求验证连通性。
// 上游报错也返回 200，把 message 交给前端展示，避免前端把业务失败当网络异常。
func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		ModelID string `json:"model_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	provider, err := s.st.GetProviderByID(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if provider == nil {
		writeErr(w, http.StatusNotFound, "not_found", "供应商不存在")
		return
	}
	if strings.TrimSpace(provider.BaseURL) == "" {
		writeErr(w, http.StatusBadRequest, "no_provider", "这家供应商还没填 API 地址")
		return
	}

	model := strings.TrimSpace(body.ModelID)
	if model == "" {
		if v, _ := s.st.GetSetting(store.SettingDefaultModel); v != "" {
			model = v
		}
	}
	if model == "" {
		if ms, err := s.st.ListModels(provider.ID); err == nil && len(ms) > 0 {
			model = ms[0].ModelID
		}
	}
	if model == "" {
		writeErr(w, http.StatusBadRequest, "no_model", "这家供应商还没有模型可用于测试，请先点「获取模型列表」")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	latency, err := llm.New(provider.BaseURL, provider.APIKey).Ping(ctx, model)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "latency_ms": 0, "model_id": model, "message": upstreamMessage(err),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "latency_ms": latency.Milliseconds(), "model_id": model,
		"message": "连接正常，模型 " + model + " 可用",
	})
}

// ---------- 模型与设置 ----------

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	var providerID int64
	if v := strings.TrimSpace(r.URL.Query().Get("provider_id")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "provider_id 必须是数字")
			return
		}
		providerID = n
	}
	list, err := s.st.ListModels(providerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// settingsView 组装设置响应：供应商列表 + 新建会话的默认值。
func (s *Server) settingsView() (map[string]any, error) {
	list, err := s.st.ListProviders()
	if err != nil {
		return nil, err
	}
	defaults := map[string]any{}
	if v, _ := s.st.GetSetting(store.SettingDefaultProvider); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			defaults["provider_id"] = n
		}
	}
	if v, _ := s.st.GetSetting(store.SettingDefaultModel); v != "" {
		defaults["model_id"] = v
	}
	if v, _ := s.st.GetSetting(store.SettingDefaultEffort); v != "" {
		defaults["effort"] = v
	}
	if v, _ := s.st.GetSetting(store.SettingDefaultSystemPrompt); v != "" {
		defaults["system_prompt"] = v
	}
	// 没设过默认供应商时，把「实际会用的那家」告诉前端，免得下拉是空的
	if _, ok := defaults["provider_id"]; !ok {
		if p, err := s.st.DefaultProvider(); err == nil && p != nil {
			defaults["provider_id"] = p.ID
		}
	}
	return map[string]any{
		"providers": providerViews(list),
		"defaults":  defaults,
		// 联网搜索是否可用（启动时配了 -searxng 才为 true），前端据此显示开关
		"web_search_available": s.mgr.SearchAvailable(),
		// 上下文压缩阈值（前端在进度条详情里展示「达到 X% 触发自动压缩」）
		"compact_threshold": s.mgr.CompactThreshold(),
	}, nil
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	view, err := s.settingsView()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handlePutDefaults 保存「新建会话时用哪个供应商 / 哪个模型 / 什么思考强度」。
// 字段省略（null）表示不动；传空串或 0 表示清空该项。
func (s *Server) handlePutDefaults(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProviderID   *int64  `json:"provider_id"`
		ModelID      *string `json:"model_id"`
		Effort       *string `json:"effort"`
		SystemPrompt *string `json:"system_prompt"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.ProviderID != nil {
		if *body.ProviderID <= 0 {
			_ = s.st.SetSetting(store.SettingDefaultProvider, "")
		} else if p, err := s.st.GetProviderByID(*body.ProviderID); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		} else if p == nil {
			writeErr(w, http.StatusNotFound, "not_found", "供应商不存在")
			return
		} else {
			_ = s.st.SetSetting(store.SettingDefaultProvider, strconv.FormatInt(p.ID, 10))
			// 换了默认供应商，原来那个默认模型多半不属于这家了
			s.ensureDefaultModel(p.ID)
		}
	}
	if body.ModelID != nil {
		_ = s.st.SetSetting(store.SettingDefaultModel, strings.TrimSpace(*body.ModelID))
	}
	if body.Effort != nil {
		_ = s.st.SetSetting(store.SettingDefaultEffort, strings.TrimSpace(*body.Effort))
	}
	if body.SystemPrompt != nil {
		// 系统提示词不做 TrimSpace 截断以外的任何加工（用户可能在意前导换行/缩进）
		_ = s.st.SetSetting(store.SettingDefaultSystemPrompt, *body.SystemPrompt)
	}
	view, err := s.settingsView()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func validBaseURL(raw string) (string, bool) {
	base := strings.TrimSpace(raw)
	if base == "" {
		return "", false
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return "", false
	}
	return base, true
}

func upstreamMessage(err error) string {
	var ae *llm.APIError
	if errors.As(err, &ae) {
		return ae.Error()
	}
	return err.Error()
}
