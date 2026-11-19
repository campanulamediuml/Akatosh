package backup

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"aiassistant/internal/store"
	_ "modernc.org/sqlite"
)

const (
	LegacyFilename = "ai-assistant-backup.db"
	Interval       = time.Hour
	Retain         = 24
)

var backupFilename = regexp.MustCompile(`^ai-assistant-[0-9]{8}-[0-9]{6}(-[0-9]{2})?\.(db|sql)$`)

type Manager struct {
	st       *store.Store
	dir      string
	interval time.Duration
	retain   int
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
	return &Manager{st: st, dir: abs, interval: Interval, retain: Retain}, nil
}

func (m *Manager) RunOnce(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	target, err := m.nextTarget()
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(m.dir, "."+LegacyFilename+"-*.tmp")
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

	if m.st.IsMySQL() {
		if err := m.st.BackupMySQL(ctx, tmpPath); err != nil {
			return "", err
		}
		if err := checkMySQLDump(tmpPath); err != nil {
			return "", err
		}
	} else {
		if err := m.st.Backup(ctx, tmpPath); err != nil {
			return "", err
		}
		if err := check(tmpPath); err != nil {
			return "", err
		}
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
		return "", fmt.Errorf("写入滚存备份失败: %w", err)
	}
	if err := m.prune(); err != nil {
		return target, err
	}
	if err := os.Remove(filepath.Join(m.dir, LegacyFilename)); err != nil && !os.IsNotExist(err) {
		return target, fmt.Errorf("清理旧单文件备份失败: %w", err)
	}
	return target, nil
}

func (m *Manager) nextTarget() (string, error) {
	base := "ai-assistant-" + time.Now().Format("20060102-150405")
	ext := ".db"
	if m.st.IsMySQL() {
		ext = ".sql"
	}
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("%s-%02d%s", base, i, ext)
		path := filepath.Join(m.dir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("同一秒内的滚存备份过多")
}

func (m *Manager) prune() error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return err
	}
	names := []string{}
	for _, entry := range entries {
		if !entry.IsDir() && backupFilename.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	retain := m.retain
	if retain <= 0 {
		retain = Retain
	}
	for i := retain; i < len(names); i++ {
		if err := os.Remove(filepath.Join(m.dir, names[i])); err != nil {
			return fmt.Errorf("清理过期备份 %s 失败: %w", names[i], err)
		}
	}
	return nil
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

func checkMySQLDump(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < 128 {
		return fmt.Errorf("MySQL 备份文件不完整")
	}
	header := make([]byte, 256)
	if _, err := io.ReadFull(file, header); err != nil {
		return err
	}
	if !strings.Contains(string(header), "Akatosh MySQL backup") {
		return fmt.Errorf("MySQL 备份缺少文件头")
	}
	tail := make([]byte, 256)
	if _, err := file.Seek(-256, io.SeekEnd); err != nil {
		return err
	}
	if _, err := io.ReadFull(file, tail); err != nil {
		return err
	}
	if !strings.Contains(string(tail), "Akatosh backup complete") {
		return fmt.Errorf("MySQL 备份缺少结束标记")
	}
	return nil
}
