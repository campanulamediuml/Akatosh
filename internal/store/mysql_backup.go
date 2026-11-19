package store

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

var memoryTables = []string{"memory_settings", "memory_maintenance", "memory_session_states"}

// BackupMySQL 在一致性快照中导出可直接导入 MySQL 的 SQL 文件。
func (s *Store) BackupMySQL(ctx context.Context, path string) error {
	if !s.IsMySQL() {
		return fmt.Errorf("当前数据库不是 MySQL")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(file)
	tables := append([]string{"schema_meta"}, mainTables...)
	tables = append(tables, memoryTables...)
	if _, err := writer.WriteString("-- Akatosh MySQL backup\nSET FOREIGN_KEY_CHECKS=0;\n\n"); err != nil {
		file.Close()
		return err
	}
	for _, table := range tables {
		if err := dumpMySQLTable(ctx, tx, writer, table); err != nil {
			file.Close()
			return fmt.Errorf("备份表 %s 失败: %w", table, err)
		}
	}
	if _, err := writer.WriteString("\nSET FOREIGN_KEY_CHECKS=1;\n-- Akatosh backup complete\n"); err != nil {
		file.Close()
		return err
	}
	if err := writer.Flush(); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func dumpMySQLTable(ctx context.Context, tx *compatTx, writer *bufio.Writer, table string) error {
	if _, err := fmt.Fprintf(writer, "\nDROP TABLE IF EXISTS `%s`;\n", table); err != nil {
		return err
	}
	var name, ddl string
	if err := tx.QueryRowContext(ctx, `SHOW CREATE TABLE `+table).Scan(&name, &ddl); err != nil {
		return err
	}
	if _, err := writer.WriteString(ddl + ";\n"); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT * FROM `+table)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = "`" + column + "`"
	}
	prefix := `INSERT INTO ` + table + ` (` + strings.Join(quoted, ",") + `) VALUES(`
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		encoded := make([]string, len(values))
		for i, value := range values {
			encoded[i] = mysqlLiteral(value)
		}
		if _, err := writer.WriteString(prefix + strings.Join(encoded, ",") + ");\n"); err != nil {
			return err
		}
	}
	return rows.Err()
}

func mysqlLiteral(value any) string {
	switch v := value.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(v, 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case bool:
		if v {
			return "1"
		}
		return "0"
	case time.Time:
		return "'" + v.Format("2006-01-02 15:04:05.999999") + "'"
	case []byte:
		return mysqlString(string(v))
	case string:
		return mysqlString(v)
	default:
		return mysqlString(fmt.Sprint(v))
	}
}

func mysqlString(value string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\", "\x00", "\\0", "\n", "\\n", "\r", "\\r", "'", "''", "\x1a", "\\Z",
	)
	return "'" + replacer.Replace(value) + "'"
}
