package store

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// convCols 里带上 provider_name：界面要直接显示「这条会话是哪家的」，
// 让前端再查一次供应商列表没必要。
//
// unread 是算出来的，不是存出来的：只要存在「比你上次查看更新的、已经答完的」
// 助手消息就算未读。只认 finished_at > 0 的（正在生成的那条不算），
// 所以红点是在**答完那一刻**才亮的。
const convCols = `c.id, c.user_id, c.title, c.effort, c.provider_id,
	COALESCE((SELECT p.name FROM providers p WHERE p.id = c.provider_id), '') AS provider_name,
	c.model_id, c.system_prompt, c.web_search, c.pinned, c.archived,
	c.summary, c.compacted_before,
	(SELECT COUNT(*) FROM messages m WHERE m.conv_id = c.id), c.created_at, c.updated_at, c.last_read_at,
	EXISTS(SELECT 1 FROM messages m WHERE m.conv_id = c.id AND m.role = 'assistant'
	       AND m.finished_at > 0 AND m.finished_at > c.last_read_at) AS unread`

func scanConv(sc interface{ Scan(...any) error }) (*Conversation, error) {
	var c Conversation
	var pinned, archived, unread, webSearch int
	if err := sc.Scan(&c.ID, &c.UserID, &c.Title, &c.Effort, &c.ProviderID, &c.ProviderName,
		&c.ModelID, &c.SystemPrompt, &webSearch, &pinned, &archived,
		&c.Summary, &c.CompactedBefore,
		&c.MessageCount, &c.CreatedAt, &c.UpdatedAt, &c.LastReadAt, &unread); err != nil {
		return nil, err
	}
	c.WebSearch = webSearch != 0
	c.Pinned = pinned != 0
	c.Archived = archived != 0
	c.Unread = unread != 0
	return &c, nil
}

// ListConversations 返回某个用户的全部会话（多租户：严格按属主过滤）。
func (s *Store) ListConversations(userID int64) ([]Conversation, error) {
	rows, err := s.db.Query(
		`SELECT `+convCols+` FROM conversations c WHERE c.user_id = ?
		 ORDER BY c.pinned DESC, c.updated_at DESC, c.id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Conversation{}
	for rows.Next() {
		c, err := scanConv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// GetConversation 按 id 取会话（不做归属校验——给已完成校验的内部路径用，
// 比如 session goroutine 按 convID 反查）。
func (s *Store) GetConversation(id int64) (*Conversation, error) {
	row := s.db.QueryRow(`SELECT `+convCols+` FROM conversations c WHERE c.id = ?`, id)
	c, err := scanConv(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

// GetConversationForUser 按 id 取会话并校验归属：别人的会话返回 (nil, nil)，
// 对外表现得像不存在，不泄露「有这条但不是你的」。
func (s *Store) GetConversationForUser(id, userID int64) (*Conversation, error) {
	c, err := s.GetConversation(id)
	if err != nil || c == nil {
		return c, err
	}
	if c.UserID != userID {
		return nil, nil
	}
	return c, nil
}

// CreateConversation 建会话。providerID/modelID 为 0/空 时套用设置里的默认值。
// 注意：全局默认 system prompt 是注入式（buildHistory 每轮现读），
// 创建时不再复制进会话（避免和注入重复）。
func (s *Store) CreateConversation(userID int64, title string, providerID int64, modelID, effort string) (*Conversation, error) {
	if title == "" {
		title = DefaultTitle
	}
	pid, mid, eff := s.ResolveDefaults(userID, providerID, modelID, effort)
	now := NowMs()
	res, err := s.db.Exec(
		`INSERT INTO conversations(user_id, title, provider_id, model_id, effort, created_at, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`, userID, title, pid, mid, eff, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetConversation(id)
}

// ResolveDefaults 把「可能为空的会话配置」补成一套能用的值：
// 供应商取设置里的默认（再退回第一条），模型取设置里的默认（再退回该供应商下第一个），
// 思考强度取设置里的默认（再退回 "medium"）。
// 供应商归属校验：指定了 providerID 但不属于这个用户时静默回退到默认，
// 绝不把会话绑到别人的供应商上（那是偷用别人 apikey 的通道）。
func (s *Store) ResolveDefaults(userID, providerID int64, modelID, effort string) (int64, string, string) {
	pid := providerID
	if pid > 0 {
		if p, err := s.GetProviderForUser(pid, userID); err != nil || p == nil {
			pid = 0
		}
	}
	if pid <= 0 {
		if v, _ := s.GetSetting(userID, SettingDefaultProvider); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				pid = n
			}
		}
	}
	if pid <= 0 {
		if p, err := s.FirstProvider(userID); err == nil && p != nil {
			pid = p.ID
		}
	}

	mid := modelID
	if mid == "" {
		if v, _ := s.GetSetting(userID, SettingDefaultModel); v != "" {
			// 默认模型必须属于这个供应商，否则会被静默发错地方
			if m, err := s.GetModelFor(pid, v); err == nil && m != nil {
				mid = v
			}
		}
	}
	if mid == "" && pid > 0 {
		if m, err := s.FirstModelOfProvider(pid); err == nil && m != nil {
			mid = m.ModelID
		}
	}

	eff := effort
	if eff == "" {
		if v, _ := s.GetSetting(userID, SettingDefaultEffort); v != "" {
			eff = v
		} else {
			eff = DefaultEffort
		}
	}
	return pid, mid, eff
}

// ConversationPatch 只更新显式给出的字段（nil = 不动）。
type ConversationPatch struct {
	Title        *string
	ProviderID   *int64
	ModelID      *string
	Effort       *string
	SystemPrompt *string
	WebSearch    *bool
	Pinned       *bool
	Archived     *bool
}

func (s *Store) UpdateConversation(id int64, p ConversationPatch) (*Conversation, error) {
	sets := []string{}
	args := []any{}

	if p.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *p.Title)
	}
	if p.ProviderID != nil {
		sets = append(sets, "provider_id = ?")
		args = append(args, *p.ProviderID)
	}
	if p.ModelID != nil {
		sets = append(sets, "model_id = ?")
		args = append(args, *p.ModelID)
	} else if p.ProviderID != nil {
		// 只换了供应商却没给模型：把模型清掉，避免出现
		// 「provider=甲 但 model 只存在于乙」这种发不出去的状态。
		// 前端随后会从新供应商里挑一个模型写回来。
		sets = append(sets, "model_id = ?")
		args = append(args, "")
	}
	if p.Effort != nil {
		sets = append(sets, "effort = ?")
		args = append(args, *p.Effort)
	}
	if p.SystemPrompt != nil {
		sets = append(sets, "system_prompt = ?")
		args = append(args, *p.SystemPrompt)
	}
	if p.WebSearch != nil {
		sets = append(sets, "web_search = ?")
		args = append(args, b2i(*p.WebSearch))
	}
	if p.Pinned != nil {
		sets = append(sets, "pinned = ?")
		args = append(args, b2i(*p.Pinned))
	}
	if p.Archived != nil {
		sets = append(sets, "archived = ?")
		args = append(args, b2i(*p.Archived))
	}

	if len(sets) > 0 {
		sets = append(sets, "updated_at = ?")
		args = append(args, NowMs())
		args = append(args, id)
		if _, err := s.db.Exec(
			`UPDATE conversations SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
			return nil, err
		}
	}
	return s.GetConversation(id)
}

func (s *Store) DeleteConversation(id int64) error {
	_, err := s.db.Exec(`DELETE FROM conversations WHERE id = ?`, id)
	return err
}

func (s *Store) TouchConversation(id int64) error {
	_, err := s.db.Exec(`UPDATE conversations SET updated_at = ? WHERE id = ?`, NowMs(), id)
	return err
}

// MarkConversationRead 把会话标为「已看过了」——侧边栏的小红点据此熄灭。
//
// 刻意**不动 updated_at**：看过一条消息不该把它顶到列表最前面，
// 那会让「按最近活跃排序」变得毫无意义。
func (s *Store) MarkConversationRead(id int64) error {
	_, err := s.db.Exec(`UPDATE conversations SET last_read_at = ? WHERE id = ?`, NowMs(), id)
	return err
}

// AutoTitle 在首轮对话结束后，用第一条用户消息给对话起名。
// 只有当标题还是默认值时才动手；返回是否真的改了。
func (s *Store) AutoTitle(convID int64) (string, bool, error) {
	c, err := s.GetConversation(convID)
	if err != nil || c == nil {
		return "", false, err
	}
	if c.Title != DefaultTitle && c.Title != "" {
		return c.Title, false, nil
	}
	first, err := s.FirstUserMessage(convID)
	if err == sql.ErrNoRows || strings.TrimSpace(first) == "" {
		return c.Title, false, nil
	}
	if err != nil {
		return c.Title, false, err
	}
	title := ellipsis(first, 24)
	if title == "" {
		return c.Title, false, nil
	}
	if _, err := s.db.Exec(`UPDATE conversations SET title = ? WHERE id = ?`, title, convID); err != nil {
		return c.Title, false, err
	}
	return title, true, nil
}

// FirstUserMessage 取会话里第一条用户消息的正文（起标题用）。
func (s *Store) FirstUserMessage(convID int64) (string, error) {
	var c string
	err := s.db.QueryRow(
		`SELECT content FROM messages WHERE conv_id = ? AND role = ? ORDER BY id LIMIT 1`,
		convID, RoleUser).Scan(&c)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return c, err
}

// ---------- 生成中标记 ----------

// GeneratingConvIDs 返回当前有消息处于 streaming 状态的会话集合。
//
// 用数据库而不是内存态来判断「哪些会话正在生成」：助手占位消息在发起生成
// 之前就已经落库、生成结束后才改状态，所以这个查询永远是对的，而且从会话
// goroutine 之外读取也不会构成数据竞争。
func (s *Store) GeneratingConvIDs() (map[int64]bool, error) {
	rows, err := s.db.Query(`SELECT DISTINCT conv_id FROM messages WHERE status = ?`, StatusStreaming)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (s *Store) String() string { return fmt.Sprintf("store(%s)", s.path) }

// SetConversationSummary 写入一次上下文压缩的结果：
// summary 是 beforeMsgID（含）之前消息的压缩摘要，那些消息随之被冻结。
// 滚动压缩（已有摘要再次压缩）由调用方负责把旧摘要合并进新摘要。
func (s *Store) SetConversationSummary(id int64, summary string, beforeMsgID int64) error {
	_, err := s.db.Exec(
		`UPDATE conversations SET summary = ?, compacted_before = ? WHERE id = ?`,
		summary, beforeMsgID, id)
	return err
}

// ForkConversation 从 beforeMsgID（含）之前的历史全量值复制出一个新会话：
// conversations / messages / message_attachments / message_files 四表各复制一段，
// reasoning、search_log、usage、编辑标记、summary、compacted_before 全部带走。
// beforeMsgID = 0 表示复制整个会话（= 从最后一条消息分叉）。
// 图片/文档本体不复制——按内容哈希去重，新消息只复制关联行（共享同一份）。
// 新会话标题 = 原标题 + "-消息{id}-fork"（分叉锚点一目了然）。
// 注意：fork 出的新会话属于 newUserID（可以是别的用户把分享链接 fork 走？
// —— 当前只允许 fork 自己的会话，归属校验在 httpapi 层做）。
func (s *Store) ForkConversation(convID, beforeMsgID, newUserID int64) (*Conversation, error) {
	src, err := s.GetConversation(convID)
	if err != nil || src == nil {
		return nil, err
	}

	msgs, err := s.ListMessages(convID)
	if err != nil {
		return nil, err
	}
	// beforeMsgID = 0：锚到最后一条消息（整会话分叉 = 从最后一条分叉的子集）
	if beforeMsgID <= 0 && len(msgs) > 0 {
		beforeMsgID = msgs[len(msgs)-1].ID
	}
	// 锚点必须在会话里真实存在，否则静默 fork 出空会话
	anchor := int64(0)
	for _, m := range msgs {
		if m.ID <= beforeMsgID {
			anchor = m.ID
		}
	}
	if anchor == 0 {
		return nil, nil
	}

	now := NowMs()
	res, err := s.db.Exec(
		`INSERT INTO conversations(user_id, title, provider_id, model_id, effort, system_prompt, web_search, summary, compacted_before, created_at, updated_at, last_read_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		newUserID, fmt.Sprintf("%s-消息%d-fork", src.Title, anchor), src.ProviderID, src.ModelID, src.Effort, src.SystemPrompt,
		b2i(src.WebSearch), src.Summary, src.CompactedBefore, now, now, now)
	if err != nil {
		return nil, err
	}
	newID, _ := res.LastInsertId()

	// 旧消息 id → 新消息 id，用来复制附件/文件关联
	idMap := map[int64]int64{}
	// 按原消息顺序收集的新 id（compacted_before 重映射要用，map 遍历无序）
	orderedNewIDs := []int64{}
	for _, m := range msgs {
		if m.ID > anchor {
			continue
		}
		status := m.Status
		if status == StatusStreaming {
			status = StatusAborted // 生成中的消息复制过去不可能继续，按中止处理
		}
		r, err := s.db.Exec(
			`INSERT INTO messages(conv_id, role, status, content, reasoning, content_original, edited, reasoning_original,
				provider_id, model_id, effort, error, prompt_tokens, completion_tokens, reasoning_tokens, total_tokens,
				cache_hit_tokens, cache_miss_tokens, created_at, finished_at, search_log, tool_rounds)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			newID, m.Role, status, m.Content, m.Reasoning, m.ContentOriginal, b2i(m.Edited), m.ReasoningOriginal,
			m.ProviderID, m.ModelID, m.Effort, m.Error,
			m.Usage.PromptTokens, m.Usage.CompletionTokens, m.Usage.ReasoningTokens, m.Usage.TotalTokens,
			m.Usage.CacheHitTokens, m.Usage.CacheMissTokens, m.CreatedAt, m.FinishedAt, m.SearchLog, m.ToolRounds)
		if err != nil {
			return nil, err
		}
		nid, _ := r.LastInsertId()
		idMap[m.ID] = nid
		orderedNewIDs = append(orderedNewIDs, nid)
	}
	// compacted_before 必须重映射：消息复制后拿了新 id，原样拷贝会让
	// 冻结边界指到不存在的旧 id（fork 里全部消息变相解冻，归档纪律失效）。
	// 冻结的是原消息的「前缀」（id <= compacted_before），按位置数出来对应的新 id。
	newCompactedBefore := int64(0)
	if src.CompactedBefore > 0 {
		// 冻结的是原消息的前缀（id <= compacted_before），数出前几条对应的新 id
		frozenTotal := 0
		for _, m := range msgs {
			if m.ID <= anchor && m.ID <= src.CompactedBefore {
				frozenTotal++
			}
		}
		if frozenTotal > 0 && frozenTotal <= len(orderedNewIDs) {
			newCompactedBefore = orderedNewIDs[frozenTotal-1]
		}
	}
	if newCompactedBefore > 0 {
		if _, err := s.db.Exec(`UPDATE conversations SET compacted_before = ? WHERE id = ?`, newCompactedBefore, newID); err != nil {
			return nil, err
		}
	}

	// 附件关联：图片本体不复制（哈希去重共享），只复制关联行
	for oldID, newMsgID := range idMap {
		atts, err := s.ListAttachments(oldID)
		if err != nil {
			return nil, err
		}
		for _, a := range atts {
			if _, err := s.db.Exec(
				`INSERT OR IGNORE INTO message_attachments(message_id, attachment_id, seq)
				 VALUES(?, ?, (SELECT COALESCE(MAX(seq),0)+1 FROM message_attachments WHERE message_id = ?))`,
				newMsgID, a.ID, newMsgID); err != nil {
				return nil, err
			}
		}
		// 文档关联：同样只复制关联行，原始文件按哈希共享
		files, err := s.ListFiles(oldID)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if _, err := s.db.Exec(
				`INSERT OR IGNORE INTO message_files(message_id, file_id, seq)
				 VALUES(?, ?, (SELECT COALESCE(MAX(seq),0)+1 FROM message_files WHERE message_id = ?))`,
				newMsgID, f.ID, newMsgID); err != nil {
				return nil, err
			}
		}
	}
	return s.GetConversation(newID)
}
