package memory

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"aiassistant/internal/store"
)

// MigrateSQLiteToMySQL 把各用户的 memory.db 记录迁入统一 MySQL 表，文件正文不动。
func MigrateSQLiteToMySQL(main *store.Store, root string) error {
	if main == nil || !main.IsMySQL() {
		return fmt.Errorf("记忆迁移目标不是 MySQL")
	}
	usersRoot := filepath.Join(root, "users")
	entries, err := os.ReadDir(usersRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		userID, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil || userID <= 0 {
			continue
		}
		path := filepath.Join(usersRoot, entry.Name(), "memory.db")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		if err := migrateUserMemory(main.SQLDB(), path, root, userID); err != nil {
			return fmt.Errorf("迁移用户 %d 记忆失败: %w", userID, err)
		}
	}
	return nil
}

func migrateUserMemory(target *sql.DB, sqlitePath, root string, userID int64) error {
	if _, err := openUserStoreMySQL(target, root, userID); err != nil {
		return err
	}
	for _, table := range []string{"memory_settings", "memory_maintenance", "memory_session_states"} {
		var count int
		if err := target.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE user_id=?`, userID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("MySQL 表 %s 已有该用户数据，拒绝覆盖", table)
		}
	}
	source, err := sql.Open("sqlite", "file:"+strings.ReplaceAll(sqlitePath, "\\", "/")+"?mode=ro")
	if err != nil {
		return err
	}
	defer source.Close()
	tx, err := target.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, mapping := range []struct{ source, target string }{
		{"settings", "memory_settings"},
		{"maintenance", "memory_maintenance"},
		{"session_states", "memory_session_states"},
	} {
		if err := copyMemoryTable(source, tx, mapping.source, mapping.target, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func copyMemoryTable(source *sql.DB, target *sql.Tx, sourceTable, targetTable string, userID int64) error {
	rows, err := source.Query(`SELECT * FROM ` + sourceTable)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	quoted := []string{"`user_id`"}
	placeholders := []string{"?"}
	for _, column := range columns {
		quoted = append(quoted, "`"+column+"`")
		placeholders = append(placeholders, "?")
	}
	insert := `INSERT INTO ` + targetTable + ` (` + strings.Join(quoted, ",") + `) VALUES(` + strings.Join(placeholders, ",") + `)`
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		values = append([]any{userID}, values...)
		if _, err := target.Exec(insert, values...); err != nil {
			return err
		}
	}
	return rows.Err()
}
