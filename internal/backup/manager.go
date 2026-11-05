package backup

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"aiassistant/internal/store"
	_ "modernc.org/sqlite"
)

const (
	Filename = "ai-assistant-backup.db"
	Interval = 12 * time.Hour
)

type Manager struct {
	st       *store.Store
	dir      string
	interval time.Duration
	mu       sync.Mutex
}

func NewManager(st *store.Store, dir string) (*Manager, error) {
	if st == nil {
		return nil, fmt.Errorf("缺少主数据库")
	}
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("备份目录不能为空")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	return &Manager{st: st, dir: abs, interval: Interval}, nil
}

func (m *Manager) RunOnce(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	target := filepath.Join(m.dir, Filename)
	tmp, err := os.CreateTemp(m.dir, "."+Filename+"-*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	if err := os.Remove(tmpPath); err != nil {
		return "", err
	}
	defer os.Remove(tmpPath)

	if err := m.st.Backup(ctx, tmpPath); err != nil {
		return "", err
	}
	if err := check(tmpPath); err != nil {
		return "", err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return "", err
	}
	file, err := os.Open(tmpPath)
	if err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return "", fmt.Errorf("替换旧备份失败: %w", err)
	}
	return target, nil
}

func (m *Manager) RunScheduler(ctx context.Context) {
	m.run(ctx)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.run(ctx)
		}
	}
}

func (m *Manager) run(ctx context.Context) {
	started := time.Now()
	path, err := m.RunOnce(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("[backup] 一致性备份失败: %v", err)
		}
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		log.Printf("[backup] 一致性备份已生成，但读取文件信息失败: %v", err)
		return
	}
	log.Printf("[backup] 一致性备份完成: %s（%.1f MB，耗时 %s）",
		path, float64(info.Size())/(1<<20), time.Since(started).Round(time.Millisecond))
}

func check(path string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.Query(`PRAGMA quick_check`)
	if err != nil {
		return fmt.Errorf("备份完整性检查失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return err
		}
		if result != "ok" {
			return fmt.Errorf("备份完整性检查失败: %s", result)
		}
	}
	return rows.Err()
}
