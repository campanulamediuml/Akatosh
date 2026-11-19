package store

import (
	"database/sql"
	"fmt"

	_ "github.com/go-sql-driver/mysql"
)

var mainTables = []string{
	"users", "settings", "providers", "models", "conversations", "messages",
	"attachments", "message_attachments", "files", "message_files", "user_settings",
}

// OpenMySQL 打开 MySQL 并创建最新 schema；SQLite 的逐版迁移不适用于新 MySQL 库。
func OpenMySQL(dsn string) (*Store, error) {
	rawDB, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 MySQL 失败: %w", err)
	}
	rawDB.SetMaxOpenConns(1)
	rawDB.SetMaxIdleConns(1)
	rawDB.SetConnMaxLifetime(0)
	db := &compatDB{DB: rawDB, dialect: dialectMySQL}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接 MySQL 失败: %w", err)
	}
	s := &Store{db: db, path: "mysql"}
	if err := s.migrateMySQL(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrateMySQL() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS schema_meta (
			k VARCHAR(191) PRIMARY KEY, v TEXT NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS users (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			name TEXT NOT NULL DEFAULT (''),
			token_hash VARCHAR(64) NOT NULL UNIQUE,
			is_admin BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL,
			last_seen_at BIGINT NOT NULL DEFAULT 0
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS settings (
			k VARCHAR(191) PRIMARY KEY, v TEXT NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS providers (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			name TEXT NOT NULL DEFAULT (''),
			base_url TEXT NOT NULL DEFAULT (''),
			api_key TEXT NOT NULL DEFAULT (''),
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL,
			user_id BIGINT NOT NULL DEFAULT 0,
			INDEX idx_providers_user(user_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS models (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			provider_id BIGINT NOT NULL,
			model_id VARCHAR(255) NOT NULL,
			display_name TEXT NOT NULL DEFAULT (''),
			reasoning BIGINT NOT NULL DEFAULT 0,
			efforts TEXT NOT NULL DEFAULT (''),
			context_len BIGINT NOT NULL DEFAULT 0,
			raw TEXT NOT NULL DEFAULT (''),
			fetched_at BIGINT NOT NULL,
			UNIQUE KEY uq_models_provider_model(provider_id, model_id),
			CONSTRAINT fk_models_provider FOREIGN KEY(provider_id) REFERENCES providers(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS conversations (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			title TEXT NOT NULL DEFAULT (''),
			model_id TEXT NOT NULL DEFAULT (''),
			effort VARCHAR(32) NOT NULL DEFAULT 'medium',
			system_prompt TEXT NOT NULL DEFAULT (''),
			pinned BIGINT NOT NULL DEFAULT 0,
			archived BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL,
			provider_id BIGINT NOT NULL DEFAULT 0,
			last_read_at BIGINT NOT NULL DEFAULT 0,
			web_search BIGINT NOT NULL DEFAULT 0,
			summary TEXT NOT NULL DEFAULT (''),
			compacted_before BIGINT NOT NULL DEFAULT 0,
			user_id BIGINT NOT NULL DEFAULT 0,
			context_estimate BIGINT NOT NULL DEFAULT 0,
			INDEX idx_conv_user(user_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS messages (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			conv_id BIGINT NOT NULL,
			role VARCHAR(32) NOT NULL,
			status VARCHAR(32) NOT NULL DEFAULT 'done',
			content LONGTEXT NOT NULL DEFAULT (''),
			reasoning LONGTEXT NOT NULL DEFAULT (''),
			content_original LONGTEXT NOT NULL DEFAULT (''),
			edited BIGINT NOT NULL DEFAULT 0,
			model_id TEXT NOT NULL DEFAULT (''),
			effort VARCHAR(32) NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT (''),
			prompt_tokens BIGINT NOT NULL DEFAULT 0,
			completion_tokens BIGINT NOT NULL DEFAULT 0,
			reasoning_tokens BIGINT NOT NULL DEFAULT 0,
			total_tokens BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL,
			finished_at BIGINT NOT NULL DEFAULT 0,
			cache_hit_tokens BIGINT NOT NULL DEFAULT 0,
			cache_miss_tokens BIGINT NOT NULL DEFAULT 0,
			provider_id BIGINT NOT NULL DEFAULT 0,
			search_log LONGTEXT NOT NULL DEFAULT (''),
			reasoning_original LONGTEXT NOT NULL DEFAULT (''),
			tool_rounds LONGTEXT NOT NULL DEFAULT (''),
			INDEX idx_messages_conv(conv_id, id),
			INDEX idx_messages_status(status),
			CONSTRAINT fk_messages_conversation FOREIGN KEY(conv_id) REFERENCES conversations(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS attachments (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			hash VARCHAR(128) NOT NULL UNIQUE,
			kind VARCHAR(32) NOT NULL DEFAULT 'image',
			mime VARCHAR(191) NOT NULL DEFAULT '',
			ext VARCHAR(32) NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT (''),
			size BIGINT NOT NULL DEFAULT 0,
			width BIGINT NOT NULL DEFAULT 0,
			height BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL,
			user_id BIGINT NOT NULL DEFAULT 0
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS message_attachments (
			message_id BIGINT NOT NULL,
			attachment_id BIGINT NOT NULL,
			seq BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY(message_id, attachment_id),
			INDEX idx_msg_attach_msg(message_id),
			CONSTRAINT fk_ma_message FOREIGN KEY(message_id) REFERENCES messages(id) ON DELETE CASCADE,
			CONSTRAINT fk_ma_attachment FOREIGN KEY(attachment_id) REFERENCES attachments(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS files (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			hash VARCHAR(128) NOT NULL,
			name TEXT NOT NULL DEFAULT (''),
			ext VARCHAR(32) NOT NULL DEFAULT '',
			size BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL,
			user_id BIGINT NOT NULL DEFAULT 0
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS message_files (
			message_id BIGINT NOT NULL,
			file_id BIGINT NOT NULL,
			seq BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY(message_id, file_id),
			INDEX idx_msg_files_msg(message_id),
			CONSTRAINT fk_mf_message FOREIGN KEY(message_id) REFERENCES messages(id) ON DELETE CASCADE,
			CONSTRAINT fk_mf_file FOREIGN KEY(file_id) REFERENCES files(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS user_settings (
			user_id BIGINT NOT NULL,
			k VARCHAR(191) NOT NULL,
			v TEXT NOT NULL DEFAULT (''),
			PRIMARY KEY(user_id, k),
			CONSTRAINT fk_user_settings_user FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}
	for _, query := range stmts {
		if _, err := s.db.Exec(query); err != nil {
			return fmt.Errorf("初始化 MySQL schema 失败: %w\nSQL: %s", err, query)
		}
	}
	var version int
	if err := s.db.QueryRow(`SELECT v FROM schema_meta WHERE k='user_version'`).Scan(&version); err != nil && err != sql.ErrNoRows {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("MySQL schema 版本 %d 高于程序支持的 %d", version, schemaVersion)
	}
	if version < schemaVersion {
		if _, err := s.db.Exec(`INSERT INTO schema_meta(k,v) VALUES('user_version',?)
			ON DUPLICATE KEY UPDATE v=VALUES(v)`, fmt.Sprintf("%d", schemaVersion)); err != nil {
			return err
		}
	}
	return nil
}
