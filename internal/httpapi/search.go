package httpapi

import (
	"net/http"
	"strings"
)

// 消息全文搜索：GET /api/search/messages?q=xxx
//
// 范围：当前用户的全部会话（含已归档的冻结消息，不含 summary 摘要文本）。
// 匹配正文 content 和思维链 reasoning（LIKE 子串，大小写不敏感）。
// 每个会话只返回**最新一条**匹配 + 总会话命中条数，点击可跳到该消息。
type searchResult struct {
	ConvID     int64  `json:"conv_id"`
	Title      string `json:"title"`
	MessageID  int64  `json:"message_id"`
	Role       string `json:"role"`
	Snippet    string `json:"snippet"`
	CreatedAt  int64  `json:"created_at"`
	MatchCount int    `json:"match_count"`
}

// searchSnippet 从命中位置前后各取一段做摘要（rune 切片，中文安全）。
func searchSnippet(content, reasoning, kw string) string {
	// 优先展示正文的命中；只有 CoT 命中时展示 CoT 那段
	src := content
	idx := strings.Index(strings.ToLower(src), strings.ToLower(kw))
	if idx < 0 {
		src = reasoning
		idx = strings.Index(strings.ToLower(src), strings.ToLower(kw))
	}
	if idx < 0 {
		return ""
	}
	r := []rune(src)
	// idx 是字节下标，换算成 rune 下标
	pos := len([]rune(src[:idx]))
	const span = 60
	start := pos - span
	if start < 0 {
		start = 0
	}
	end := pos + len([]rune(kw)) + span
	if end > len(r) {
		end = len(r)
	}
	out := strings.TrimSpace(strings.ReplaceAll(string(r[start:end]), "\n", " "))
	if start > 0 {
		out = "…" + out
	}
	if end < len(r) {
		out += "…"
	}
	return out
}

func (s *Server) handleSearchMessages(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	if len([]rune(q)) > 100 {
		writeErr(w, http.StatusBadRequest, "bad_request", "搜索词太长了")
		return
	}
	// LIKE 通配符转义（用户输入 % _ 按字面量处理）
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
	hits, err := s.st.SearchMessages(userOf(r).ID, "%"+escaped+"%", 30)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]searchResult, 0, len(hits))
	for _, h := range hits {
		out = append(out, searchResult{
			ConvID:     h.ConvID,
			Title:      h.Title,
			MessageID:  h.MessageID,
			Role:       h.Role,
			Snippet:    searchSnippet(h.Content, h.Reasoning, q),
			CreatedAt:  h.CreatedAt,
			MatchCount: h.MatchCount,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
