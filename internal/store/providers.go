package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"aiassistant/internal/llm"
)

// ErrNotFound 表示要找的记录不存在。
var ErrNotFound = errors.New("not found")

// ---------- providers ----------
//
// 多租户：providers 完全按用户隔离（apikey 隔离是硬性要求）。
// 所有读写都必须带 userID；GetProviderByID 是唯一不带的方法，
// 只给「已经做过归属校验」的内部路径用（比如按会话里记录的 provider_id 反查）。

// provCols 不含模型数：模型数在列表接口里单独算。
const provCols = `p.id, p.user_id, p.name, p.base_url, p.api_key, p.created_at, p.updated_at`

func scanProvider(sc interface{ Scan(...any) error }, withCount bool) (*Provider, error) {
	var p Provider
	var err error
	if withCount {
		err = sc.Scan(&p.ID, &p.UserID, &p.Name, &p.BaseURL, &p.APIKey, &p.CreatedAt, &p.UpdatedAt, &p.ModelCount)
	} else {
		err = sc.Scan(&p.ID, &p.UserID, &p.Name, &p.BaseURL, &p.APIKey, &p.CreatedAt, &p.UpdatedAt)
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ListProviders 返回某个用户的全部供应商（按 id 升序），顺带带上各自的模型数。
func (s *Store) ListProviders(userID int64) ([]Provider, error) {
	rows, err := s.db.Query(
		`SELECT `+provCols+`, (SELECT COUNT(*) FROM models m WHERE m.provider_id = p.id)
		   FROM providers p WHERE p.user_id = ? ORDER BY p.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Provider{}
	for rows.Next() {
		p, err := scanProvider(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetProviderByID 按 id 取供应商；不存在返回 (nil, nil)。
// 不带归属校验——调用方必须自己保证这条记录属于当前用户（或做系统级反查）。
func (s *Store) GetProviderByID(id int64) (*Provider, error) {
	row := s.db.QueryRow(
		`SELECT `+provCols+`, (SELECT COUNT(*) FROM models m WHERE m.provider_id = p.id)
		   FROM providers p WHERE p.id = ?`, id)
	p, err := scanProvider(row, true)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// GetProviderForUser 按 id 取供应商并校验归属：不是当前用户的返回 (nil, nil)
// —— 对外一律表现得像「不存在」，不暴露「有这么一条但是别人的」。
func (s *Store) GetProviderForUser(id, userID int64) (*Provider, error) {
	p, err := s.GetProviderByID(id)
	if err != nil || p == nil {
		return p, err
	}
	if p.UserID != userID {
		return nil, nil
	}
	return p, nil
}

// DefaultProvider 返回「当前用户的默认供应商」：
// 先看设置里记的 id，取不到就退回该用户 id 最小的那条。都没有则返回 (nil, nil)。
func (s *Store) DefaultProvider(userID int64) (*Provider, error) {
	if v, _ := s.GetSetting(userID, SettingDefaultProvider); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			p, err := s.GetProviderForUser(id, userID)
			if err != nil {
				return nil, err
			}
			if p != nil {
				return p, nil
			}
		}
	}
	return s.FirstProvider(userID)
}

// FirstProvider 退回用：该用户 id 最小的那条供应商。
func (s *Store) FirstProvider(userID int64) (*Provider, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM providers WHERE user_id = ? ORDER BY id LIMIT 1`, userID).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.GetProviderByID(id)
}

// CountProviders 返回该用户的供应商总数（判断「还没配过」用）。
func (s *Store) CountProviders(userID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM providers WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// CreateProvider 给某个用户新建一个供应商。
func (s *Store) CreateProvider(userID int64, name, baseURL, apiKey string) (*Provider, error) {
	if strings.TrimSpace(name) == "" {
		name = DefaultProviderN
	}
	now := NowMs()
	res, err := s.db.Exec(
		`INSERT INTO providers(user_id, name, base_url, api_key, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?)`,
		userID, name, baseURL, apiKey, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	// 第一个建的自动成为默认供应商，省得用户还要去设一次
	if v, _ := s.GetSetting(userID, SettingDefaultProvider); v == "" {
		_ = s.SetSetting(userID, SettingDefaultProvider, strconv.FormatInt(id, 10))
	}
	return s.GetProviderByID(id)
}

// UpdateProvider 更新供应商（带归属校验）。apiKey 为空串表示「保留原 key 不动」。
func (s *Store) UpdateProvider(id, userID int64, name, baseURL, apiKey string) (*Provider, error) {
	cur, err := s.GetProviderForUser(id, userID)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, ErrNotFound
	}
	if strings.TrimSpace(name) != "" {
		cur.Name = strings.TrimSpace(name)
	}
	if strings.TrimSpace(baseURL) != "" {
		cur.BaseURL = strings.TrimSpace(baseURL)
	}
	if strings.TrimSpace(apiKey) != "" {
		cur.APIKey = strings.TrimSpace(apiKey)
	}
	if _, err := s.db.Exec(
		`UPDATE providers SET name = ?, base_url = ?, api_key = ?, updated_at = ? WHERE id = ?`,
		cur.Name, cur.BaseURL, cur.APIKey, NowMs(), id); err != nil {
		return nil, err
	}
	return s.GetProviderByID(id)
}

// DeleteProvider 删除供应商（带归属校验），连同它的模型缓存（外键 ON DELETE CASCADE）。
//
// 引用它的会话不会被删：那些会话的 provider_id 留着，读取时如果发现供应商没了
// 会回退到默认供应商（见 ResolveProvider）。这样用户误删一个供应商
// 不会连带炸掉聊天记录。
func (s *Store) DeleteProvider(id, userID int64) error {
	res, err := s.db.Exec(`DELETE FROM providers WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	// 如果删掉的正是默认供应商，把默认指向该用户剩下第一条
	if v, _ := s.GetSetting(userID, SettingDefaultProvider); v == strconv.FormatInt(id, 10) {
		next, err := s.FirstProvider(userID)
		if err != nil {
			return err
		}
		if next != nil {
			_ = s.SetSetting(userID, SettingDefaultProvider, strconv.FormatInt(next.ID, 10))
		} else {
			_ = s.SetSetting(userID, SettingDefaultProvider, "")
			_ = s.SetSetting(userID, SettingDefaultModel, "")
		}
	}
	return nil
}

// ResolveProvider 给一个会话用的 provider_id：属于这个用户就用它，
// 不存在（比如供应商被删了）就回退到该用户的默认供应商。都没有则返回 (nil, nil)。
func (s *Store) ResolveProvider(userID, id int64) (*Provider, error) {
	if id > 0 {
		p, err := s.GetProviderForUser(id, userID)
		if err != nil {
			return nil, err
		}
		if p != nil {
			return p, nil
		}
	}
	return s.DefaultProvider(userID)
}

// ---------- models ----------

// UpsertModels 把上游拉到的模型列表写进缓存（按 provider 隔离），返回新增条数。
// 已存在的模型会更新 display_name/context_len/reasoning/efforts，但不会因为
// 一次拉取失败就消失（这里不做删除）。
func (s *Store) UpsertModels(providerID int64, models []Model) (int, error) {
	now := NowMs()
	added := 0
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	for _, m := range models {
		efforts, _ := json.Marshal(m.Efforts)
		if len(m.Efforts) == 0 {
			efforts = []byte("[]")
		}
		var prevID int64
		err := tx.QueryRow(`SELECT id FROM models WHERE provider_id = ? AND model_id = ?`,
			providerID, m.ModelID).Scan(&prevID)
		switch {
		case err == sql.ErrNoRows:
			if _, err := tx.Exec(
				`INSERT INTO models(provider_id, model_id, display_name, reasoning, efforts, context_len, fetched_at)
				 VALUES(?, ?, ?, ?, ?, ?, ?)`,
				providerID, m.ModelID, m.DisplayName, b2i(m.Reasoning), string(efforts), m.ContextLen, now); err != nil {
				return added, err
			}
			added++
		case err != nil:
			return added, err
		default:
			if _, err := tx.Exec(
				`UPDATE models SET display_name = ?, reasoning = ?, efforts = ?, context_len = ?, fetched_at = ?
				 WHERE id = ?`,
				m.DisplayName, b2i(m.Reasoning), string(efforts), m.ContextLen, now, prevID); err != nil {
				return added, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return added, err
	}
	return added, nil
}

const modelCols = `id, provider_id, model_id, display_name, reasoning, efforts, context_len, fetched_at`

func scanModel(rows interface{ Scan(...any) error }) (*Model, error) {
	var m Model
	var reasoning int
	var efforts string
	if err := rows.Scan(&m.ID, &m.ProviderID, &m.ModelID, &m.DisplayName,
		&reasoning, &efforts, &m.ContextLen, &m.FetchedAt); err != nil {
		return nil, err
	}
	m.Reasoning = reasoning != 0
	m.Efforts = []string{}
	if efforts != "" {
		_ = json.Unmarshal([]byte(efforts), &m.Efforts)
	}
	if m.ContextLen == 0 {
		// 存量数据兜底：升级前拉的模型可能没有上下文长度（上游没报），
		// 读时按模型名推断——不用迁移，立即生效，永远不写回库里
		m.ContextLen = llm.InferContextLen(m.ModelID)
	}
	return &m, nil
}

// ListModels 返回某个用户的模型缓存（经 providers 归属过滤，看不到别家的）。
// providerID > 0 时只看那一家（归属同样校验）。
func (s *Store) ListModels(userID, providerID int64) ([]Model, error) {
	q := `SELECT ` + modelCols + ` FROM models WHERE provider_id IN (SELECT id FROM providers WHERE user_id = ?)`
	args := []any{userID}
	if providerID > 0 {
		q += ` AND provider_id = ?`
		args = append(args, providerID)
	}
	q += ` ORDER BY provider_id, model_id`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Model{}
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// GetModelFor 按 (供应商, 模型名) 精确取一条。
// 多供应商下同名模型是两条不同记录，所以必须带 providerID，
// 否则「gpt-4o 到底属于哪家」就是歧义的。
func (s *Store) GetModelFor(providerID int64, modelID string) (*Model, error) {
	row := s.db.QueryRow(
		`SELECT `+modelCols+` FROM models WHERE provider_id = ? AND model_id = ? LIMIT 1`,
		providerID, modelID)
	m, err := scanModel(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// FirstModelOfProvider 取某供应商下的第一个模型（挑默认模型时用）。
func (s *Store) FirstModelOfProvider(providerID int64) (*Model, error) {
	row := s.db.QueryRow(
		`SELECT `+modelCols+` FROM models WHERE provider_id = ? ORDER BY model_id LIMIT 1`, providerID)
	m, err := scanModel(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// ---------- 小工具 ----------

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ellipsis 按「字符」而不是字节截断，避免把一个汉字切一半。
func ellipsis(s string, max int) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	r := []rune(s)
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}
