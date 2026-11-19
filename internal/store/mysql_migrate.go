package store

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// MigrateSQLiteToMySQL 把主库逐表复制到空 MySQL 库，保留全部 id 和关联关系。
func MigrateSQLiteToMySQL(target *Store, sqlitePath string) error {
	if target == nil || !target.IsMySQL() {
		return fmt.Errorf("迁移目标不是 MySQL")
	}
	for _, table := range mainTables {
		var count int
		if err := target.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("MySQL 表 %s 已有 %d 行，拒绝覆盖非空数据库", table, count)
		}
	}

	source, err := sql.Open("sqlite", "file:"+strings.ReplaceAll(sqlitePath, "\\", "/")+"?mode=ro")
	if err != nil {
		return err
	}
	defer source.Close()
	source.SetMaxOpenConns(1)
	if err := source.Ping(); err != nil {
		return fmt.Errorf("读取 SQLite 主库失败: %w", err)
	}

	tx, err := target.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	counts := map[string]int{}
	for _, table := range mainTables {
		count, err := copyTable(source, tx, table)
		if err != nil {
			return fmt.Errorf("迁移表 %s 失败: %w", table, err)
		}
		counts[table] = count
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for table, want := range counts {
		var got int
		if err := target.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("表 %s 迁移后行数不符: 源 %d，目标 %d", table, want, got)
		}
	}
	return nil
}

func copyTable(source *sql.DB, target *compatTx, table string) (int, error) {
	rows, err := source.Query(`SELECT * FROM ` + table)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	quoted := make([]string, len(columns))
	placeholders := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = "`" + column + "`"
		placeholders[i] = "?"
	}
	insert := `INSERT INTO ` + table + ` (` + strings.Join(quoted, ",") + `) VALUES(` + strings.Join(placeholders, ",") + `)`
	count := 0
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return count, err
		}
		if _, err := target.Exec(insert, values...); err != nil {
			return count, err
		}
		count++
	}
	return count, rows.Err()
}
