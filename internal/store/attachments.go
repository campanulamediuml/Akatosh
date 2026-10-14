package store

import (
	"database/sql"
	"encoding/json"
	"strconv"
)

// Attachment 是一份已经落盘的附件（当前只支持图片）。
// 文件本体在 misc 目录里，这里只存元数据。
type Attachment struct {
	ID   int64  `json:"id"`
	Hash string `json:"-"` // 内容 sha256，同时是文件名
	Kind string `json:"kind"`
	MIME string `json:"mime"`
	Ext  string `json:"-"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	// 原图尺寸
	Width     int   `json:"width"`
	Height    int   `json:"height"`
	CreatedAt int64 `json:"created_at"`

	// 下面两个不是数据库列，是出网关前按 id 拼出来的（契约 §4 要求）。
	// store.Attachment 是「数据库行」，而任何要发给前端的消息都得带上这两个 URL，
	// 所以由调用方（见 session.fillAttachmentURLs）补。
	ThumbURL string `json:"thumb_url"`
	RawURL   string `json:"raw_url"`
}

const attachCols = `id, hash, kind, mime, ext, name, size, width, height, created_at`

func scanAttachment(sc interface{ Scan(...any) error }) (*Attachment, error) {
	var a Attachment
	if err := sc.Scan(&a.ID, &a.Hash, &a.Kind, &a.MIME, &a.Ext, &a.Name,
		&a.Size, &a.Width, &a.Height, &a.CreatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

// CreateAttachment 按内容哈希去重地登记一份附件。
// 同一张图重复上传只会有一行（hash 唯一）。
func (s *Store) CreateAttachment(a Attachment) (*Attachment, error) {
	if a.Kind == "" {
		a.Kind = "image"
	}
	if a.CreatedAt == 0 {
		a.CreatedAt = NowMs()
	}
	// 先看有没有
	if prev, err := s.GetAttachmentByHash(a.Hash); err != nil {
		return nil, err
	} else if prev != nil {
		return prev, nil
	}
	res, err := s.db.Exec(
		`INSERT INTO attachments(hash, kind, mime, ext, name, size, width, height, created_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Hash, a.Kind, a.MIME, a.Ext, a.Name, a.Size, a.Width, a.Height, a.CreatedAt)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetAttachment(id)
}

func (s *Store) GetAttachment(id int64) (*Attachment, error) {
	row := s.db.QueryRow(`SELECT `+attachCols+` FROM attachments WHERE id = ?`, id)
	a, err := scanAttachment(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}

func (s *Store) GetAttachmentByHash(hash string) (*Attachment, error) {
	row := s.db.QueryRow(`SELECT `+attachCols+` FROM attachments WHERE hash = ?`, hash)
	a, err := scanAttachment(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}

// AttachToMessage 把若干附件挂到一条消息上，顺序按传入顺序记录。
func (s *Store) AttachToMessage(messageID int64, attachmentIDs []int64) error {
	if len(attachmentIDs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, aid := range attachmentIDs {
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO message_attachments(message_id, attachment_id, seq) VALUES(?, ?, ?)`,
			messageID, aid, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListAttachments 返回一条消息的附件（按 seq 排序）。
func (s *Store) ListAttachments(messageID int64) ([]Attachment, error) {
	rows, err := s.db.Query(
		`SELECT `+prefixCols("a", attachCols)+`
		   FROM message_attachments ma
		   JOIN attachments a ON a.id = ma.attachment_id
		  WHERE ma.message_id = ?
		  ORDER BY ma.seq, a.id`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// AttachmentsByMessageIDs 批量取（给会话快照用，避免 N+1 查询）。
func (s *Store) AttachmentsByMessageIDs(ids []int64) (map[int64][]Attachment, error) {
	out := map[int64][]Attachment{}
	if len(ids) == 0 {
		return out, nil
	}
	// 参数占位符
	placeholders := ""
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, id)
	}
	rows, err := s.db.Query(
		`SELECT ma.message_id, `+prefixCols("a", attachCols)+`
		   FROM message_attachments ma
		   JOIN attachments a ON a.id = ma.attachment_id
		  WHERE ma.message_id IN (`+placeholders+`)
		  ORDER BY ma.message_id, ma.seq, a.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var mid int64
		var a Attachment
		if err := rows.Scan(&mid, &a.ID, &a.Hash, &a.Kind, &a.MIME, &a.Ext, &a.Name,
			&a.Size, &a.Width, &a.Height, &a.CreatedAt); err != nil {
			return nil, err
		}
		out[mid] = append(out[mid], a)
	}
	return out, rows.Err()
}

// GetMessageWithAttachments 取一条消息并带上附件（发完消息后要立刻广播出去用）。
func (s *Store) GetMessageWithAttachments(id int64) (*Message, error) {
	m, err := s.GetMessage(id)
	if err != nil || m == nil {
		return m, err
	}
	atts, err := s.ListAttachments(id)
	if err != nil {
		return m, err
	}
	m.Attachments = atts
	return m, nil
}

// prefixCols 给列名加上表别名前缀：`id, hash` → `a.id, a.hash`
func prefixCols(alias, cols string) string {
	parts := splitCols(cols)
	for i, p := range parts {
		parts[i] = alias + "." + p
	}
	return joinComma(parts)
}

// 极简实现，避免为了这一处引入 strings 的额外分支
func splitCols(cols string) []string {
	out := []string{}
	cur := ""
	for _, r := range cols {
		if r == ',' {
			out = append(out, trimSpace(cur))
			cur = ""
			continue
		}
		cur += string(r)
	}
	if trimSpace(cur) != "" {
		out = append(out, trimSpace(cur))
	}
	return out
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// ParseIDList 解析前端传来的附件 id 列表（JSON 数组）。空/非法一律当空。
func ParseIDList(raw string) []int64 {
	if raw == "" {
		return nil
	}
	var ids []int64
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil
	}
	return ids
}

// FormatIDList 把 id 列表序列化成 JSON 数组字符串。
func FormatIDList(ids []int64) string {
	if len(ids) == 0 {
		return "[]"
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// itoa 小工具（避免到处 import strconv）
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// GetAttachmentInConv 按 id 读附件，并校验它确实挂在该会话的消息上
// （read_attachment 工具的归属校验：不许跨会话读图）。
func (s *Store) GetAttachmentInConv(id, convID int64) (*Attachment, error) {
	row := s.db.QueryRow(
		`SELECT a.id, a.hash, a.kind, a.mime, a.ext, a.name, a.size, a.width, a.height, a.created_at
		   FROM attachments a
		   JOIN message_attachments ma ON ma.attachment_id = a.id
		   JOIN messages m ON m.id = ma.message_id
		  WHERE a.id = ? AND m.conv_id = ?`, id, convID)
	a, err := scanAttachment(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}
