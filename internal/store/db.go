package store

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
)

type dbDialect string

const (
	dialectSQLite dbDialect = "sqlite"
	dialectMySQL  dbDialect = "mysql"
)

var (
	conflictPattern = regexp.MustCompile(`(?i)ON CONFLICT\s*\([^)]*\)\s*DO UPDATE SET`)
	excludedPattern = regexp.MustCompile(`(?i)excluded\.([a-zA-Z_][a-zA-Z0-9_]*)`)
)

// compatDB 保持现有 database/sql 调用形态，只在少数方言差异处做转换。
type compatDB struct {
	*sql.DB
	dialect dbDialect
}

func (db *compatDB) sql(query string) string {
	if db.dialect != dialectMySQL {
		return query
	}
	query = strings.ReplaceAll(query, "INSERT OR IGNORE", "INSERT IGNORE")
	query = strings.ReplaceAll(query, "INSERT OR REPLACE", "REPLACE")
	query = conflictPattern.ReplaceAllString(query, "ON DUPLICATE KEY UPDATE")
	return excludedPattern.ReplaceAllString(query, "VALUES($1)")
}

func (db *compatDB) Exec(query string, args ...any) (sql.Result, error) {
	return db.DB.Exec(db.sql(query), args...)
}
func (db *compatDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return db.DB.ExecContext(ctx, db.sql(query), args...)
}
func (db *compatDB) Query(query string, args ...any) (*sql.Rows, error) {
	return db.DB.Query(db.sql(query), args...)
}
func (db *compatDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.DB.QueryContext(ctx, db.sql(query), args...)
}
func (db *compatDB) QueryRow(query string, args ...any) *sql.Row {
	return db.DB.QueryRow(db.sql(query), args...)
}
func (db *compatDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return db.DB.QueryRowContext(ctx, db.sql(query), args...)
}
func (db *compatDB) Begin() (*compatTx, error) {
	tx, err := db.DB.Begin()
	return &compatTx{Tx: tx, dialect: db.dialect}, err
}
func (db *compatDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*compatTx, error) {
	tx, err := db.DB.BeginTx(ctx, opts)
	return &compatTx{Tx: tx, dialect: db.dialect}, err
}

type compatTx struct {
	*sql.Tx
	dialect dbDialect
}

func (tx *compatTx) sql(query string) string {
	return (&compatDB{dialect: tx.dialect}).sql(query)
}
func (tx *compatTx) Exec(query string, args ...any) (sql.Result, error) {
	return tx.Tx.Exec(tx.sql(query), args...)
}
func (tx *compatTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.Tx.ExecContext(ctx, tx.sql(query), args...)
}
func (tx *compatTx) Query(query string, args ...any) (*sql.Rows, error) {
	return tx.Tx.Query(tx.sql(query), args...)
}
func (tx *compatTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.Tx.QueryContext(ctx, tx.sql(query), args...)
}
func (tx *compatTx) QueryRow(query string, args ...any) *sql.Row {
	return tx.Tx.QueryRow(tx.sql(query), args...)
}
func (tx *compatTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.Tx.QueryRowContext(ctx, tx.sql(query), args...)
}
