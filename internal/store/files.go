package store

import (
	"database/sql"
	"fmt"
)

// File 是一个上传的文档。原始二进制存在 misc/files/<hash><ext>（内容哈希去重），
// 内容按需读取——read_file 工具调 pybox 沙箱现解文本（pypdf/docx/openpyxl/chardet），
// PDF 指定页也可用 pymupdf 渲染成图片；Go 不碰任何格式解析。
type File struct {
	ID int64 `json:"id"`
	// UserID 首个上传者（同 attachments 的哈希去重语义）
	UserID    int64  `json:"-"`
	Hash      string `json:"hash"`
	Name      string `json:"name"`
	Ext       string `json:"ext"`
	Size      int    `json:"size"` // 原始文件字节数
	CreatedAt int64  `json:"created_at"`
}

// CreateFile 存一个文件记录（按内容哈希去重：同内容复用旧记录并刷新名字）。
func (s *Store) CreateFile(f File) (*File, error) {
	if existing, err := s.GetFileByHash(f.Hash); err == nil && existing != nil {
		// 同名同内容重复上传：复用，但把名字更新成最新的（用户可能改了文件名）
		_, _ = s.db.Exec(`UPDATE files SET name = ? WHERE id = ?`, f.Name, existing.ID)
		existing.Name = f.Name
		return existing, nil
	}
	res, err := s.db.Exec(
		`INSERT INTO files(user_id, hash, name, ext, size, created_at) VALUES(?, ?, ?, ?, ?, ?)`,
		f.UserID, f.Hash, f.Name, f.Ext, f.Size, NowMs())
	if err != nil {
		return nil, err
	}
	f.ID, _ = res.LastInsertId()
	f.CreatedAt = NowMs()
	return &f, nil
}

// GetFileByHash 按内容哈希找文件（去重用）。
func (s *Store) GetFileByHash(hash string) (*File, error) {
	row := s.db.QueryRow(
		`SELECT id, user_id, hash, name, ext, size, created_at FROM files WHERE hash = ?`, hash)
	return scanFile(row)
}

// GetFile 按 id 读文件（不含 content——列表/卡片用）。
func (s *Store) GetFile(id int64) (*File, error) {
	row := s.db.QueryRow(
		`SELECT id, user_id, hash, name, ext, size, created_at FROM files WHERE id = ?`, id)
	return scanFile(row)
}

// GetFile 按 id 读文件（不含 content——列表/卡片用）。
func (s *Store) GetFileInConv(id, convID int64) (*File, error) {
	row := s.db.QueryRow(
		`SELECT f.id, f.user_id, f.hash, f.name, f.ext, f.size, f.created_at
		   FROM files f
		   JOIN message_files mf ON mf.file_id = f.id
		   JOIN messages m ON m.id = mf.message_id
		  WHERE f.id = ? AND m.conv_id = ?`, id, convID)
	return scanFile(row)
}

// AttachFileToMessage 把文件挂到消息上。
func (s *Store) AttachFileToMessage(messageID int64, fileIDs []int64) error {
	for _, fid := range fileIDs {
		if _, err := s.db.Exec(
			`INSERT OR IGNORE INTO message_files(message_id, file_id, seq)
			 VALUES(?, ?, (SELECT COALESCE(MAX(seq),0)+1 FROM message_files WHERE message_id = ?))`,
			messageID, fid, messageID); err != nil {
			return err
		}
	}
	return nil
}

// FilesByMessageIDs 批量取若干消息挂的文件（state/事件出网时用）。
func (s *Store) FilesByMessageIDs(ids []int64) (map[int64][]File, error) {
	out := map[int64][]File{}
	if len(ids) == 0 {
		return out, nil
	}
	q := `SELECT mf.message_id, f.id, f.user_id, f.hash, f.name, f.ext, f.size, f.created_at
	        FROM message_files mf JOIN files f ON f.id = mf.file_id
	       WHERE mf.message_id IN (` + placeholders(len(ids)) + `) ORDER BY mf.message_id, mf.seq`
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var mid int64
		var f File
		if err := rows.Scan(&mid, &f.ID, &f.UserID, &f.Hash, &f.Name, &f.Ext, &f.Size, &f.CreatedAt); err != nil {
			return nil, err
		}
		out[mid] = append(out[mid], f)
	}
	return out, rows.Err()
}

// CountFilesOwnedBy 数这批文件 id 里有多少条属于指定用户（含哈希去重共享的）：
// 归属 = 首个上传者，或挂在该用户会话的消息上。发消息挂文件前的越权校验用。
func (s *Store) CountFilesOwnedBy(ids []int64, userID int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var n int
	q := `SELECT COUNT(DISTINCT f.id) FROM files f
	       WHERE f.id IN (` + placeholders(len(ids)) + `)
	         AND (f.user_id = ? OR EXISTS(
	           SELECT 1 FROM message_files mf
	             JOIN messages m ON m.id = mf.message_id
	             JOIN conversations c ON c.id = m.conv_id
	            WHERE mf.file_id = f.id AND c.user_id = ?))`
	args := make([]any, 0, len(ids)+2)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, userID, userID)
	err := s.db.QueryRow(q, args...).Scan(&n)
	return n, err
}

// ListFiles 列一个消息挂的文件。
func (s *Store) ListFiles(messageID int64) ([]File, error) {
	m, err := s.FilesByMessageIDs([]int64{messageID})
	return m[messageID], err
}

func scanFile(sc interface{ Scan(...any) error }) (*File, error) {
	var f File
	err := sc.Scan(&f.ID, &f.UserID, &f.Hash, &f.Name, &f.Ext, &f.Size, &f.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("扫描文件记录失败: %w", err)
	}
	return &f, nil
}

// placeholders 生成 SQL IN 用的 "?, ?, ?" 占位串。
func placeholders(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			s += ","
		}
		s += "?"
	}
	return s
}
