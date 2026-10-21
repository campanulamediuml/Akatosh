package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"
)

// ---------- 用户（多租户） ----------
//
// 访问令牌不直接存：只存 SHA-256 哈希。库泄露了令牌也不泄露。
// token → user 的解析在 httpapi 鉴权中间件里做（见 auth.go）。

type User struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	IsAdmin    bool   `json:"is_admin"`
	CreatedAt  int64  `json:"created_at"`
	LastSeenAt int64  `json:"last_seen_at"`
}

// HashToken 把明文令牌转成存库用的哈希（hex 编码的 SHA-256）。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

// CreateUser 建用户。tokenHash 用 HashToken 先算好（不存明文）。
func (s *Store) CreateUser(name, tokenHash string, isAdmin bool) (*User, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "user"
	}
	res, err := s.db.Exec(
		`INSERT INTO users(name, token_hash, is_admin, created_at) VALUES(?, ?, ?, ?)`,
		name, tokenHash, b2i(isAdmin), NowMs())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetUser(id)
}

func (s *Store) GetUser(id int64) (*User, error) {
	row := s.db.QueryRow(`SELECT id, name, is_admin, created_at, last_seen_at FROM users WHERE id = ?`, id)
	return scanUser(row)
}

// UserByTokenHash 鉴权主路径：按令牌哈希找人。
func (s *Store) UserByTokenHash(hash string) (*User, error) {
	row := s.db.QueryRow(`SELECT id, name, is_admin, created_at, last_seen_at FROM users WHERE token_hash = ?`, hash)
	return scanUser(row)
}

func scanUser(row *sql.Row) (*User, error) {
	var u User
	var admin int
	if err := row.Scan(&u.ID, &u.Name, &admin, &u.CreatedAt, &u.LastSeenAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	u.IsAdmin = admin != 0
	return &u, nil
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT id, name, is_admin, created_at, last_seen_at FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		var admin int
		if err := rows.Scan(&u.ID, &u.Name, &admin, &u.CreatedAt, &u.LastSeenAt); err != nil {
			return nil, err
		}
		u.IsAdmin = admin != 0
		out = append(out, u)
	}
	return out, rows.Err()
}

// TouchUser 刷新 last_seen_at。鉴权中间件节流调用（比如每分钟最多一次）。
func (s *Store) TouchUser(id int64) {
	_, _ = s.db.Exec(`UPDATE users SET last_seen_at = ? WHERE id = ?`, NowMs(), id)
}

// RotateUserToken 重置用户令牌（旧令牌立即失效），返回 true 表示用户存在。
func (s *Store) RotateUserToken(id int64, newHash string) (bool, error) {
	res, err := s.db.Exec(`UPDATE users SET token_hash = ? WHERE id = ?`, newHash, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteUser 删用户并清掉他名下的数据：
// conversations（messages/message_attachments/message_files 靠外键级联）、
// files 记录、user_settings。attachments 是内容寻址的共享元数据（别的用户可能
// 因为哈希去重引用同一条），不跟着删；磁盘上的 misc 文件也保留（按哈希共享）。
// 不允许删掉最后一个管理员（系统会再也无法管理用户）。
func (s *Store) DeleteUser(id int64) error {
	var adminCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin = 1`).Scan(&adminCount); err != nil {
		return err
	}
	u, err := s.GetUser(id)
	if err != nil {
		return err
	}
	if u == nil {
		return ErrNotFound
	}
	if u.IsAdmin && adminCount <= 1 {
		return fmt.Errorf("不能删除最后一个管理员")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM conversations WHERE user_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM files WHERE user_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM user_settings WHERE user_id = ?`, id); err != nil {
		return err
	}
	// 供应商 + 模型（models 靠 providers 外键级联）
	if _, err := tx.Exec(`DELETE FROM providers WHERE user_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM users WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------- 首次启动引导（单租户 → 多租户迁移） ----------

// BootstrapUsers 保证系统里至少有一个管理员：
//   - users 表为空：用 legacyToken（启动参数 -token，可空 → 自动生成）建一个
//     管理员「admin」，并把全部存量数据（conversations/providers/files/attachments
//     的 user_id=0、settings 表的键值）划到这个管理员名下。
//     返回 created=true 和实际生效的 token（main 打印给用户）。
//   - users 表非空但 legacyToken 非空且没有任何用户持有它：再给这个令牌建一个
//     管理员（应急通道：令牌丢了可以用 -token 重启来恢复管理员入口）。
func (s *Store) BootstrapUsers(legacyToken string) (token string, created bool, err error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return "", false, err
	}
	legacyToken = strings.TrimSpace(legacyToken)

	if n == 0 {
		if legacyToken == "" {
			legacyToken = generateToken()
		}
		u, err := s.CreateUser("admin", HashToken(legacyToken), true)
		if err != nil {
			return "", false, err
		}
		if err := s.adoptAllData(u.ID); err != nil {
			return "", false, err
		}
		return legacyToken, true, nil
	}

	if legacyToken != "" {
		u, err := s.UserByTokenHash(HashToken(legacyToken))
		if err != nil {
			return "", false, err
		}
		if u == nil {
			if _, err := s.CreateUser("admin", HashToken(legacyToken), true); err != nil {
				return "", false, err
			}
			return legacyToken, true, nil
		}
	}
	return "", false, nil
}

// adoptAllData 把单租户时代的存量数据全部划给指定用户。
func (s *Store) adoptAllData(userID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"conversations", "providers", "files", "attachments"} {
		res, err := tx.Exec(`UPDATE `+table+` SET user_id = ? WHERE user_id = 0`, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("[store] 单租户数据迁移：%s 表 %d 行划给用户 #%d", table, n, userID)
		}
	}
	rows, err := tx.Query(`SELECT k, v FROM settings`)
	if err != nil {
		return err
	}
	type kv struct{ k, v string }
	var settings []kv
	for rows.Next() {
		var x kv
		if err := rows.Scan(&x.k, &x.v); err != nil {
			rows.Close()
			return err
		}
		settings = append(settings, x)
	}
	rows.Close()
	for _, x := range settings {
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO user_settings(user_id, k, v) VALUES(?, ?, ?)`, userID, x.k, x.v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// generateToken 生成本地令牌（32 字节随机，hex）。
// 和 httpapi.GenerateToken 分开一份：store 不能依赖 httpapi（循环依赖）。
func generateToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败在正常系统上不可能发生；退化到时间戳也好过弱随机被撞
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
