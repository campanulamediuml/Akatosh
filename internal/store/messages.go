package store

import "database/sql"

const msgCols = `id, conv_id, role, status, content, reasoning, content_original, edited,
	provider_id, model_id, effort, error, prompt_tokens, completion_tokens, reasoning_tokens, total_tokens,
	cache_hit_tokens, cache_miss_tokens, created_at, finished_at, search_log, reasoning_original, tool_rounds`

func scanMsg(sc interface{ Scan(...any) error }) (*Message, error) {
	var m Message
	var edited int
	if err := sc.Scan(&m.ID, &m.ConvID, &m.Role, &m.Status, &m.Content, &m.Reasoning,
		&m.ContentOriginal, &edited, &m.ProviderID, &m.ModelID, &m.Effort, &m.Error,
		&m.Usage.PromptTokens, &m.Usage.CompletionTokens, &m.Usage.ReasoningTokens,
		&m.Usage.TotalTokens, &m.Usage.CacheHitTokens, &m.Usage.CacheMissTokens,
		&m.CreatedAt, &m.FinishedAt, &m.SearchLog, &m.ReasoningOriginal, &m.ToolRounds); err != nil {
		return nil, err
	}
	m.Edited = edited != 0
	// tool_rounds 里的分段 usage 拆出来给前端（工具轮卡片按段展示 token）；
	// rounds 本体只面向模型，不解析不进 API。
	if p := ParseToolRounds(m.ToolRounds); p != nil {
		m.SegUsages = p.SegUsages
	}
	return &m, nil
}

func (s *Store) ListMessages(convID int64) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT `+msgCols+` FROM messages WHERE conv_id = ? ORDER BY id`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *Store) GetMessage(id int64) (*Message, error) {
	row := s.db.QueryRow(`SELECT `+msgCols+` FROM messages WHERE id = ?`, id)
	m, err := scanMsg(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// CreateMessage 插入一条消息。streaming 状态的助手占位消息也走这里，
// 这样即使用户在生成中途刷新页面，也能从数据库看到这条正在生成的消息。
func (s *Store) CreateMessage(m Message) (*Message, error) {
	if m.Status == "" {
		m.Status = StatusDone
	}
	now := NowMs()
	if m.CreatedAt == 0 {
		m.CreatedAt = now
	}
	res, err := s.db.Exec(
		`INSERT INTO messages(conv_id, role, status, content, reasoning, provider_id, model_id, effort, created_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ConvID, m.Role, m.Status, m.Content, m.Reasoning, m.ProviderID, m.ModelID, m.Effort, m.CreatedAt)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	_ = s.TouchConversation(m.ConvID)
	return s.GetMessage(id)
}

// UpdateMessageStream 把当前累积的完整内容写回数据库（幂等覆盖，不是拼接）。
// hub 每 250ms 调一次，避免每个 token 一次写事务。
func (s *Store) UpdateMessageStream(id int64, reasoning, content string) error {
	_, err := s.db.Exec(
		`UPDATE messages SET reasoning = ?, content = ? WHERE id = ?`,
		reasoning, content, id)
	return err
}

// FinishMessage 落最终状态。searchLog 是本轮工具调用的 JSON 摘要（展示用，无调用传空串）；
// toolRounds 是本轮完整工具交互（跨轮全量回放用，无调用传空串）。
func (s *Store) FinishMessage(id int64, status, reasoning, content, errMsg string, u Usage, searchLog, toolRounds string) error {
	_, err := s.db.Exec(
		`UPDATE messages SET status = ?, reasoning = ?, content = ?, error = ?,
			prompt_tokens = ?, completion_tokens = ?, reasoning_tokens = ?, total_tokens = ?,
			cache_hit_tokens = ?, cache_miss_tokens = ?,
			finished_at = ?, search_log = ?, tool_rounds = ?
		 WHERE id = ?`,
		status, reasoning, content, errMsg,
		u.PromptTokens, u.CompletionTokens, u.ReasoningTokens, u.TotalTokens,
		u.CacheHitTokens, u.CacheMissTokens,
		NowMs(), searchLog, toolRounds, id)
	return err
}

// UpdateMessageContent 用户编辑正文。
// content_original 只在第一次编辑时记录，之后编辑不会覆盖它，保证「还原原文」始终有效。
func (s *Store) UpdateMessageContent(id int64, content string) (*Message, error) {
	_, err := s.db.Exec(
		`UPDATE messages
		 SET content_original = CASE WHEN edited = 0 THEN content ELSE content_original END,
		     content = ?,
		     edited = 1
		 WHERE id = ?`, content, id)
	if err != nil {
		return nil, err
	}
	m, err := s.GetMessage(id)
	if err != nil || m == nil {
		return m, err
	}
	_ = s.TouchConversation(m.ConvID)
	return m, nil
}

// RestoreMessage 把正文还原成模型原文。
func (s *Store) RestoreMessage(id int64) (*Message, error) {
	_, err := s.db.Exec(
		`UPDATE messages SET content = content_original, content_original = '', edited = 0
		 WHERE id = ? AND edited = 1`, id)
	if err != nil {
		return nil, err
	}
	return s.GetMessage(id)
}

// UpdateMessageReasoning 编辑 CoT。与正文编辑对称：
// reasoning_original 只在第一次编辑时记录原文，之后编辑不覆盖它，保证「还原原文」始终有效。
func (s *Store) UpdateMessageReasoning(id int64, reasoning string) (*Message, error) {
	_, err := s.db.Exec(
		`UPDATE messages
		 SET reasoning_original = CASE WHEN reasoning_original = '' THEN reasoning ELSE reasoning_original END,
		     reasoning = ?
		 WHERE id = ?`, reasoning, id)
	if err != nil {
		return nil, err
	}
	m, err := s.GetMessage(id)
	if err != nil || m == nil {
		return m, err
	}
	_ = s.TouchConversation(m.ConvID)
	return m, nil
}

// RestoreMessageReasoning 把 CoT 还原成模型原文。
func (s *Store) RestoreMessageReasoning(id int64) (*Message, error) {
	_, err := s.db.Exec(
		`UPDATE messages SET reasoning = reasoning_original, reasoning_original = ''
		 WHERE id = ? AND reasoning_original != ''`, id)
	if err != nil {
		return nil, err
	}
	return s.GetMessage(id)
}

func (s *Store) DeleteMessage(id int64) error {
	_, err := s.db.Exec(`DELETE FROM messages WHERE id = ?`, id)
	return err
}

// DeleteMessagesFrom 截断用：inclusive=true 时连目标一起删（重新生成助手回复），
// inclusive=false 时保留目标、删它之后的（基于这条用户消息重新生成）。
func (s *Store) DeleteMessagesFrom(convID, id int64, inclusive bool) (int64, error) {
	op := ">"
	if inclusive {
		op = ">="
	}
	res, err := s.db.Exec(
		`DELETE FROM messages WHERE conv_id = ? AND id `+op+` ?`, convID, id)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	_ = s.TouchConversation(convID)
	return n, nil
}

// ListMessagesTail 取会话里最近 limit 条消息（进会话的首屏用，避免全量拉取长会话）。
// 返回（正序的最近 limit 条, 总数）。
func (s *Store) ListMessagesTail(convID int64, limit int) ([]Message, int, error) {
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conv_id = ?`, convID).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.Query(
		`SELECT `+msgCols+` FROM messages WHERE conv_id = ? ORDER BY id DESC LIMIT ?`, convID, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *m)
	}
	// 取回的是倒序，翻成正序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, total, rows.Err()
}

// ListMessagesBefore 取 id < beforeID 的更早消息（向上翻页用），
// 按 id 倒序取 limit 条再翻成正序。
func (s *Store) ListMessagesBefore(convID, beforeID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(
		`SELECT `+msgCols+` FROM messages WHERE conv_id = ? AND id < ? ORDER BY id DESC LIMIT ?`,
		convID, beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}
