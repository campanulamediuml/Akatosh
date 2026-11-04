package memory

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	settingBackgroundProvider = "background_provider_id"
	settingBackgroundModel    = "background_model_id"
	settingBackgroundEffort   = "background_effort"
	maintenanceLastSuccess    = "last_successful_archive_at"
)

// BackgroundSettings 决定起标题和记忆归档走哪条上游线路。
type BackgroundSettings struct {
	ProviderID int64  `json:"provider_id"`
	ModelID    string `json:"model_id"`
	Effort     string `json:"effort"`
}

// SessionStatus 是 sidecar 中的记忆状态，不属于主库 Conversation。
type SessionStatus struct {
	ConvID         int64  `json:"conv_id"`
	Enabled        bool   `json:"enabled"`
	Status         string `json:"status"`
	ArchivedBefore int64  `json:"archived_before"`
	ArchivedAt     int64  `json:"archived_at"`
	LastError      string `json:"last_error"`
	Pending        bool   `json:"pending"`
}

type sessionRecord struct {
	SessionStatus
	ArchivePath       string
	ArchiveHash       string
	SourceFingerprint string
	IndexCard         string
}

// MaintenanceStatus 给设置页展示最近一次每日维护结果。
type MaintenanceStatus struct {
	LastSuccessfulAt int64  `json:"last_successful_at"`
	LastReport       string `json:"last_report"`
}

type userStore struct {
	userID    int64
	root      string
	db        *sql.DB
	archiveMu sync.Mutex
}

func openUserStore(root string, userID int64) (*userStore, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("非法用户 id")
	}
	dir := filepath.Join(root, "users", strconv.FormatInt(userID, 10))
	if err := os.MkdirAll(filepath.Join(dir, ".staging"), 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "memory.db")
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	u := &userStore{userID: userID, root: dir, db: db}
	if err := u.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return u, nil
}

func (u *userStore) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS settings (k TEXT PRIMARY KEY, v TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS maintenance (k TEXT PRIMARY KEY, v TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS session_states (
			conv_id INTEGER PRIMARY KEY,
			enabled INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'pending',
			archived_before INTEGER NOT NULL DEFAULT 0,
			archived_at INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			archive_path TEXT NOT NULL DEFAULT '',
			archive_hash TEXT NOT NULL DEFAULT '',
			source_fingerprint TEXT NOT NULL DEFAULT '',
			index_card TEXT NOT NULL DEFAULT ''
		)`,
	}
	for _, q := range stmts {
		if _, err := u.db.Exec(q); err != nil {
			return fmt.Errorf("初始化用户 %d 的记忆库失败: %w", u.userID, err)
		}
	}
	return nil
}

func (u *userStore) close() error { return u.db.Close() }

func (u *userStore) setting(key string) string {
	var v string
	_ = u.db.QueryRow(`SELECT v FROM settings WHERE k = ?`, key).Scan(&v)
	return v
}

func (u *userStore) setSetting(key, value string) error {
	_, err := u.db.Exec(`INSERT INTO settings(k,v) VALUES(?,?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`, key, value)
	return err
}

func (u *userStore) background() BackgroundSettings {
	pid, _ := strconv.ParseInt(u.setting(settingBackgroundProvider), 10, 64)
	effort := u.setting(settingBackgroundEffort)
	if effort == "" {
		effort = "low"
	}
	return BackgroundSettings{ProviderID: pid, ModelID: u.setting(settingBackgroundModel), Effort: effort}
}

func (u *userStore) setBackground(v BackgroundSettings) error {
	if v.ProviderID <= 0 {
		for _, key := range []string{settingBackgroundProvider, settingBackgroundModel} {
			if err := u.setSetting(key, ""); err != nil {
				return err
			}
		}
		return u.setSetting(settingBackgroundEffort, "low")
	}
	if err := u.setSetting(settingBackgroundProvider, strconv.FormatInt(v.ProviderID, 10)); err != nil {
		return err
	}
	if err := u.setSetting(settingBackgroundModel, strings.TrimSpace(v.ModelID)); err != nil {
		return err
	}
	return u.setSetting(settingBackgroundEffort, strings.TrimSpace(v.Effort))
}

func (u *userStore) session(convID int64) (*sessionRecord, error) {
	var r sessionRecord
	var enabled int
	err := u.db.QueryRow(`SELECT conv_id,enabled,status,archived_before,archived_at,last_error,
		archive_path,archive_hash,source_fingerprint,index_card FROM session_states WHERE conv_id = ?`, convID).
		Scan(&r.ConvID, &enabled, &r.Status, &r.ArchivedBefore, &r.ArchivedAt, &r.LastError,
			&r.ArchivePath, &r.ArchiveHash, &r.SourceFingerprint, &r.IndexCard)
	if err == sql.ErrNoRows {
		return &sessionRecord{SessionStatus: SessionStatus{ConvID: convID, Status: "pending"}}, nil
	}
	if err != nil {
		return nil, err
	}
	r.Enabled = enabled != 0
	return &r, nil
}

func (u *userStore) sessions() ([]sessionRecord, error) {
	rows, err := u.db.Query(`SELECT conv_id,enabled,status,archived_before,archived_at,last_error,
		archive_path,archive_hash,source_fingerprint,index_card FROM session_states ORDER BY conv_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []sessionRecord{}
	for rows.Next() {
		var r sessionRecord
		var enabled int
		if err := rows.Scan(&r.ConvID, &enabled, &r.Status, &r.ArchivedBefore, &r.ArchivedAt, &r.LastError,
			&r.ArchivePath, &r.ArchiveHash, &r.SourceFingerprint, &r.IndexCard); err != nil {
			return nil, err
		}
		r.Enabled = enabled != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func (u *userStore) setEnabled(convID int64, enabled bool) error {
	_, err := u.db.Exec(`INSERT INTO session_states(conv_id,enabled,status) VALUES(?,?,'pending')
		ON CONFLICT(conv_id) DO UPDATE SET enabled=excluded.enabled,
		status=CASE WHEN excluded.enabled=1 AND session_states.status='disabled' THEN 'pending' ELSE session_states.status END`,
		convID, boolInt(enabled))
	return err
}

func (u *userStore) markError(convID int64, message string) error {
	_, err := u.db.Exec(`UPDATE session_states SET status='error',last_error=? WHERE conv_id=?`, message, convID)
	return err
}

func (u *userStore) saveArchive(r *sessionRecord) error {
	_, err := u.db.Exec(`UPDATE session_states SET status='done',archived_before=?,archived_at=?,last_error='',
		archive_path=?,archive_hash=?,source_fingerprint=?,index_card=? WHERE conv_id=?`,
		r.ArchivedBefore, r.ArchivedAt, r.ArchivePath, r.ArchiveHash, r.SourceFingerprint, r.IndexCard, r.ConvID)
	return err
}

func (u *userStore) updateArchiveHash(convID int64, hash string) error {
	_, err := u.db.Exec(`UPDATE session_states SET archive_hash=? WHERE conv_id=?`, hash, convID)
	return err
}

func (u *userStore) deleteSession(convID int64) error {
	_, err := u.db.Exec(`DELETE FROM session_states WHERE conv_id=?`, convID)
	return err
}

func (u *userStore) maintenanceValue(key string) int64 {
	var raw string
	_ = u.db.QueryRow(`SELECT v FROM maintenance WHERE k=?`, key).Scan(&raw)
	n, _ := strconv.ParseInt(raw, 10, 64)
	return n
}

func (u *userStore) maintenanceText(key string) string {
	var raw string
	_ = u.db.QueryRow(`SELECT v FROM maintenance WHERE k=?`, key).Scan(&raw)
	return raw
}

func (u *userStore) setMaintenance(key, value string) error {
	_, err := u.db.Exec(`INSERT INTO maintenance(k,v) VALUES(?,?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`, key, value)
	return err
}

func (u *userStore) maintenanceStatus() MaintenanceStatus {
	return MaintenanceStatus{
		LastSuccessfulAt: u.maintenanceValue(maintenanceLastSuccess),
		LastReport:       u.maintenanceText("last_report"),
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func nowMs() int64 { return time.Now().UnixMilli() }
