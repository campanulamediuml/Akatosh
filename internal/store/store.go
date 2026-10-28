package store

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，无需 CGO
)

// Store 封装 SQLite。全局只开一条连接：单用户本地应用不需要并发写，
// 这样可以从根上避免 SQLITE_BUSY，代价可以忽略（每次读写都在亚毫秒级）。
type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

func NowMs() int64 { return time.Now().UnixMilli() }

// Open 打开（必要时创建）数据库，并执行建表迁移。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录 %s 失败: %w", dir, err)
		}
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

const schemaVersion = 13

func (s *Store) migrate() error {
	var ver int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&ver); err != nil {
		return fmt.Errorf("读取 schema 版本失败: %w", err)
	}
	if ver >= schemaVersion {
		return nil
	}

	// v1：建表
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS settings (
			k TEXT PRIMARY KEY,
			v TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS providers (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			name       TEXT    NOT NULL DEFAULT '默认',
			base_url   TEXT    NOT NULL DEFAULT '',
			api_key    TEXT    NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS models (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			provider_id  INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			model_id     TEXT    NOT NULL,
			display_name TEXT    NOT NULL DEFAULT '',
			reasoning    INTEGER NOT NULL DEFAULT 0,
			efforts      TEXT    NOT NULL DEFAULT '[]',
			context_len  INTEGER NOT NULL DEFAULT 0,
			raw          TEXT    NOT NULL DEFAULT '',
			fetched_at   INTEGER NOT NULL,
			UNIQUE(provider_id, model_id)
		)`,
		`CREATE TABLE IF NOT EXISTS conversations (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			title         TEXT    NOT NULL DEFAULT '新对话',
			model_id      TEXT    NOT NULL DEFAULT '',
			effort        TEXT    NOT NULL DEFAULT 'medium',
			system_prompt TEXT    NOT NULL DEFAULT '',
			pinned        INTEGER NOT NULL DEFAULT 0,
			archived      INTEGER NOT NULL DEFAULT 0,
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			conv_id           INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
			role              TEXT    NOT NULL,
			status            TEXT    NOT NULL DEFAULT 'done',
			content           TEXT    NOT NULL DEFAULT '',
			reasoning         TEXT    NOT NULL DEFAULT '',
			content_original  TEXT    NOT NULL DEFAULT '',
			edited            INTEGER NOT NULL DEFAULT 0,
			model_id          TEXT    NOT NULL DEFAULT '',
			effort            TEXT    NOT NULL DEFAULT '',
			error             TEXT    NOT NULL DEFAULT '',
			prompt_tokens     INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
			total_tokens      INTEGER NOT NULL DEFAULT 0,
			created_at        INTEGER NOT NULL,
			finished_at       INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_conv ON messages(conv_id, id)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_status ON messages(status)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("建表失败: %w\nSQL: %s", err, q)
		}
	}

	// v2：把前缀缓存的命中/未命中也记下来（成本大头在这里）
	for _, q := range []string{
		`ALTER TABLE messages ADD COLUMN cache_hit_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE messages ADD COLUMN cache_miss_tokens INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			// 列已存在就当迁移过了（v1 库里可能手工加过）
			if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				return fmt.Errorf("迁移到 v2 失败: %w\nSQL: %s", err, q)
			}
		}
	}

	// v3：会话绑定到具体供应商（支持多供应商）
	if err := s.migrateToV3(); err != nil {
		return err
	}

	// v4：消息也记下自己是哪家供应商产的（同名模型跨供应商时要能分辨）
	if err := s.migrateToV4(); err != nil {
		return err
	}

	// v5：附件（图片）
	if err := s.migrateToV5(); err != nil {
		return err
	}

	// v6：会话的「最近已读时间」（侧边栏小红点用）
	if err := s.migrateToV6(); err != nil {
		return err
	}

	// v7：会话级联网搜索开关 + 消息的搜索记录
	if err := s.migrateToV7(); err != nil {
		return err
	}

	// v8：CoT 编辑（reasoning_original 保留原文）
	if err := s.migrateToV8(); err != nil {
		return err
	}

	// v9：上下文压缩（summary + compacted_before）
	if err := s.migrateToV9(); err != nil {
		return err
	}

	// v10：文件上传（文档给 AI 读）
	if err := s.migrateToV10(); err != nil {
		return err
	}

	// v11：消息的完整工具轮（跨轮全量回放用）
	if err := s.migrateToV11(); err != nil {
		return err
	}

	// v12：多租户（users 表 + 各业务表 user_id + 按用户的 user_settings）
	if err := s.migrateToV12(); err != nil {
		return err
	}

	// v13：压缩后的上下文用量估算（下一次上游 usage 返回后清零）
	if err := s.migrateToV13(); err != nil {
		return err
	}

	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("写入 schema 版本失败: %w", err)
	}
	return nil
}

func (s *Store) migrateToV13() error {
	if _, err := s.db.Exec(`ALTER TABLE conversations ADD COLUMN context_estimate INTEGER NOT NULL DEFAULT 0`); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return fmt.Errorf("迁移到 v13 失败: %w", err)
		}
	}
	return nil
}

// migrateToV12 多租户改造：
//   - users 表：令牌哈希 + 管理员标记（见 users.go）
//   - conversations / providers / files / attachments 加 user_id（0 = 待认领，
//     首次启动时由 BootstrapUsers 统一划给首个管理员）
//   - user_settings 按用户的键值表，替代单租户的全局 settings 表
//     （旧 settings 表保留不删，BootstrapUsers 把内容搬进 user_settings）
func (s *Store) migrateToV12() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			name         TEXT    NOT NULL DEFAULT '',
			token_hash   TEXT    NOT NULL UNIQUE,
			is_admin     INTEGER NOT NULL DEFAULT 0,
			created_at   INTEGER NOT NULL,
			last_seen_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS user_settings (
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			k       TEXT    NOT NULL,
			v       TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY (user_id, k)
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("迁移到 v12 失败: %w\nSQL: %s", err, q)
		}
	}
	// 加列：老库可能已经加过（手工或上次中断），重复列错误吞掉
	for _, table := range []string{"conversations", "providers", "files", "attachments"} {
		if _, err := s.db.Exec(
			fmt.Sprintf(`ALTER TABLE %s ADD COLUMN user_id INTEGER NOT NULL DEFAULT 0`, table)); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				return fmt.Errorf("迁移到 v12 失败（%s.user_id）: %w", table, err)
			}
		}
	}
	for _, idx := range []string{
		`CREATE INDEX IF NOT EXISTS idx_conv_user ON conversations(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_providers_user ON providers(user_id)`,
	} {
		if _, err := s.db.Exec(idx); err != nil {
			return fmt.Errorf("迁移到 v12 失败: %w", err)
		}
	}
	return nil
}

// migrateToV10 建文件表：上传的文档元信息（原始二进制存 misc/files/<hash><ext>，
// 文本提取由 pybox 沙箱按需进行——Go 不碰格式解析）。
// 与图片附件对称：按内容哈希去重，消息通过 message_files 关联。
func (s *Store) migrateToV10() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS files (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			hash       TEXT    NOT NULL,
			name       TEXT    NOT NULL,
			ext        TEXT    NOT NULL DEFAULT '',
			size       INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS message_files (
			message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			file_id    INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
			seq        INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (message_id, file_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_msg_files_msg ON message_files(message_id)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("迁移到 v10 失败: %w\nSQL: %s", err, q)
		}
	}
	return nil
}

// migrateToV11 给 messages 加 tool_rounds：本轮生成的完整工具交互
// （assistant tool_calls 段 + tool 结果段，JSON），跨轮全量回放用。
// 空串 = 老消息/没调工具——那些仍走 search_log 摘要 + 生图重建的老路。
func (s *Store) migrateToV11() error {
	if _, err := s.db.Exec(`ALTER TABLE messages ADD COLUMN tool_rounds TEXT NOT NULL DEFAULT ''`); err != nil {
		// 列已存在就当迁移过了：schemaVersion 升级会触发全量重跑，幂等是硬要求
		if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return fmt.Errorf("迁移到 v11 失败: %w", err)
		}
	}
	return nil
}

// migrateToV9 给 conversations 加上下文压缩的两个字段：
//   - summary：旧消息的压缩摘要（compact 时由模型生成）
//   - compacted_before：摘要覆盖到哪条消息 id（含）。id 更早的消息被冻结——
//     它们的内容已折进摘要，再改会和摘要不一致，所以只读。
func (s *Store) migrateToV9() error {
	for _, q := range []string{
		`ALTER TABLE conversations ADD COLUMN summary TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE conversations ADD COLUMN compacted_before INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				return fmt.Errorf("迁移到 v9 失败: %w\nSQL: %s", err, q)
			}
		}
	}
	return nil
}

// migrateToV8 给 messages 加 reasoning_original：CoT 编辑时保留原文（与
// content_original 对正文的处理对称），可一键还原。
func (s *Store) migrateToV8() error {
	q := `ALTER TABLE messages ADD COLUMN reasoning_original TEXT NOT NULL DEFAULT ''`
	if _, err := s.db.Exec(q); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return fmt.Errorf("迁移到 v8 失败（加列）: %w", err)
		}
	}
	return nil
}

// migrateToV7 给 conversations 加 web_search（会话级联网搜索开关），
// 给 messages 加 search_log（本轮搜索的 JSON 摘要，展示用）。
func (s *Store) migrateToV7() error {
	for _, q := range []string{
		`ALTER TABLE conversations ADD COLUMN web_search INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE messages ADD COLUMN search_log TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				return fmt.Errorf("迁移到 v7 失败: %w\nSQL: %s", err, q)
			}
		}
	}
	return nil
}

// migrateToV6 给 conversations 加 last_read_at，并把老数据回填成 updated_at。
//
// 回填很重要：不回填的话所有历史会话都会立刻显示成「未读」，一开就是一片红点。
func (s *Store) migrateToV6() error {
	q := `ALTER TABLE conversations ADD COLUMN last_read_at INTEGER NOT NULL DEFAULT 0`
	if _, err := s.db.Exec(q); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return fmt.Errorf("迁移到 v6 失败（加列）: %w", err)
		}
	}
	res, err := s.db.Exec(`UPDATE conversations SET last_read_at = updated_at WHERE last_read_at = 0`)
	if err != nil {
		return fmt.Errorf("迁移到 v6 失败（回填）: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[migrate] 已把 %d 个已有会话标为「已读」", n)
	}
	return nil
}

// migrateToV5 建附件表与「消息 ↔ 附件」关联表。
//
// 为什么用关联表而不是在 messages 上塞一个 JSON 列：同一张图（内容哈希相同）
// 只存一行、只占一份磁盘，但可以被多条消息引用；关联表天然支持这个。
func (s *Store) migrateToV5() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS attachments (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			hash       TEXT    NOT NULL UNIQUE,
			kind       TEXT    NOT NULL DEFAULT 'image',
			mime       TEXT    NOT NULL DEFAULT '',
			ext        TEXT    NOT NULL DEFAULT '',
			name       TEXT    NOT NULL DEFAULT '',
			size       INTEGER NOT NULL DEFAULT 0,
			width      INTEGER NOT NULL DEFAULT 0,
			height     INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS message_attachments (
			message_id    INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			attachment_id INTEGER NOT NULL REFERENCES attachments(id) ON DELETE CASCADE,
			seq           INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (message_id, attachment_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_msg_attach_msg ON message_attachments(message_id)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("迁移到 v5 失败: %w\nSQL: %s", err, q)
		}
	}
	return nil
}

// migrateToV4 给 messages 加 provider_id，并按会话回填历史消息。
func (s *Store) migrateToV4() error {
	q := `ALTER TABLE messages ADD COLUMN provider_id INTEGER NOT NULL DEFAULT 0`
	if _, err := s.db.Exec(q); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return fmt.Errorf("迁移到 v4 失败（加列）: %w", err)
		}
	}
	// 历史消息按它所属会话的供应商回填：升级前一个部署只有一家供应商，
	// 这个推断是准确的。
	res, err := s.db.Exec(
		`UPDATE messages
		    SET provider_id = COALESCE((SELECT c.provider_id FROM conversations c WHERE c.id = messages.conv_id), 0)
		  WHERE provider_id = 0`)
	if err != nil {
		return fmt.Errorf("迁移到 v4 失败（回填 provider_id）: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[migrate] 已给 %d 条历史消息补上供应商归属", n)
	}
	return nil
}

// migrateToV3 给 conversations 加上 provider_id，并把已有会话回填成
// 「当前唯一那个供应商」——升级上来的用户不该因为这次改动丢任何东西。
func (s *Store) migrateToV3() error {
	q := `ALTER TABLE conversations ADD COLUMN provider_id INTEGER NOT NULL DEFAULT 0`
	if _, err := s.db.Exec(q); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return fmt.Errorf("迁移到 v3 失败（加列）: %w", err)
		}
	}
	// 回填：只在只有一条供应商（也就是升级前的老部署）时才动手。
	// 多供应商用户的新会话由创建时显式指定，这里不碰。
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM providers`).Scan(&n); err != nil {
		return fmt.Errorf("迁移到 v3 失败（统计供应商）: %w", err)
	}
	if n == 0 {
		return nil
	}
	res, err := s.db.Exec(
		`UPDATE conversations
		    SET provider_id = (SELECT id FROM providers ORDER BY id LIMIT 1)
		  WHERE provider_id = 0`)
	if err != nil {
		return fmt.Errorf("迁移到 v3 失败（回填 provider_id）: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected > 0 {
		log.Printf("[migrate] 已把 %d 个已有会话绑定到默认供应商", affected)
	}
	// 顺手把默认供应商也记住（老部署只有一家，就是它）。
	// userID=0：这是单租户时代的迁移，写的是全局 settings 表，
	// v12 引导时再由 BootstrapUsers 整体搬进首个管理员的 user_settings。
	if v, _ := s.GetSetting(0, SettingDefaultProvider); v == "" {
		var id int64
		if err := s.db.QueryRow(`SELECT id FROM providers ORDER BY id LIMIT 1`).Scan(&id); err == nil {
			_ = s.SetSetting(0, SettingDefaultProvider, fmt.Sprintf("%d", id))
		}
	}
	return nil
}

// MarkInterrupted 把所有遗留的 streaming 状态标记为中断。
// 服务端启动时调用：上一次进程被杀掉时留下的 run 不可能再继续了。
// 已生成出来的那部分内容会保留下来，用户不会丢 CoT。
func (s *Store) MarkInterrupted() (int64, error) {
	res, err := s.db.Exec(
		`UPDATE messages SET status = ?, error = ?, finished_at = ?
		 WHERE status = ?`,
		StatusAborted, "服务重启，生成被中断", NowMs(), StatusStreaming)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ---------- settings ----------

// GetSetting 读某个用户的设置项（user_settings 表，按用户隔离）。
// userID <= 0 时读单租户时代的全局 settings 表（仅供迁移期代码使用）。
func (s *Store) GetSetting(userID int64, key string) (string, error) {
	var v string
	var err error
	if userID > 0 {
		err = s.db.QueryRow(`SELECT v FROM user_settings WHERE user_id = ? AND k = ?`, userID, key).Scan(&v)
	} else {
		err = s.db.QueryRow(`SELECT v FROM settings WHERE k = ?`, key).Scan(&v)
	}
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetSetting 写设置项。userID <= 0 时写单租户时代的全局 settings 表
// （仅迁移期代码使用——v3 回填默认供应商在 users 表还不存在时就要写）。
func (s *Store) SetSetting(userID int64, key, value string) error {
	if userID <= 0 {
		_, err := s.db.Exec(
			`INSERT INTO settings(k, v) VALUES(?, ?)
			 ON CONFLICT(k) DO UPDATE SET v = excluded.v`, key, value)
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO user_settings(user_id, k, v) VALUES(?, ?, ?)
		 ON CONFLICT(user_id, k) DO UPDATE SET v = excluded.v`, userID, key, value)
	return err
}
