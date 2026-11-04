package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"aiassistant/internal/store"
)

// Manager 管理每用户独立的记忆数据库和文件树。主库只读，不做任何 schema 变更。
type Manager struct {
	main *store.Store
	root string

	mu      sync.Mutex
	users   map[int64]*userStore
	running map[int64]bool
	closed  bool
}

func NewManager(main *store.Store, root string) (*Manager, error) {
	if main == nil {
		return nil, fmt.Errorf("缺少主库")
	}
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("记忆目录不能为空")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, "users"), 0o700); err != nil {
		return nil, err
	}
	return &Manager{main: main, root: abs, users: map[int64]*userStore{}, running: map[int64]bool{}}, nil
}

func (m *Manager) user(userID int64) (*userStore, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("记忆服务已关闭")
	}
	if u := m.users[userID]; u != nil {
		return u, nil
	}
	u, err := openUserStore(m.root, userID)
	if err != nil {
		return nil, err
	}
	m.users[userID] = u
	return u, nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	var first error
	for id, u := range m.users {
		if err := u.close(); err != nil && first == nil {
			first = err
		}
		delete(m.users, id)
	}
	return first
}

func (m *Manager) UserRoot(userID int64) string {
	return filepath.Join(m.root, "users", strconv.FormatInt(userID, 10))
}

func (m *Manager) BackgroundSettings(userID int64) (BackgroundSettings, error) {
	u, err := m.user(userID)
	if err != nil {
		return BackgroundSettings{}, err
	}
	return u.background(), nil
}

func (m *Manager) SetBackgroundSettings(userID int64, cfg BackgroundSettings) error {
	if cfg.ProviderID > 0 || strings.TrimSpace(cfg.ModelID) != "" {
		p, err := m.main.GetProviderForUser(cfg.ProviderID, userID)
		if err != nil {
			return err
		}
		if p == nil {
			return fmt.Errorf("供应商不存在")
		}
		if strings.TrimSpace(cfg.ModelID) != "" {
			model, err := m.main.GetModelFor(cfg.ProviderID, strings.TrimSpace(cfg.ModelID))
			if err != nil {
				return err
			}
			if model == nil {
				return fmt.Errorf("模型不属于所选供应商")
			}
			if cfg.Effort != "" && !model.Reasoning {
				return fmt.Errorf("所选模型不支持思考强度")
			}
		}
	}
	u, err := m.user(userID)
	if err != nil {
		return err
	}
	return u.setBackground(cfg)
}

func (m *Manager) MaintenanceStatus(userID int64) (MaintenanceStatus, error) {
	u, err := m.user(userID)
	if err != nil {
		return MaintenanceStatus{}, err
	}
	return u.maintenanceStatus(), nil
}

func (m *Manager) SessionStatus(userID, convID int64) (SessionStatus, error) {
	conv, err := m.main.GetConversationForUser(convID, userID)
	if err != nil {
		return SessionStatus{}, err
	}
	if conv == nil {
		return SessionStatus{}, fmt.Errorf("会话不存在")
	}
	u, err := m.user(userID)
	if err != nil {
		return SessionStatus{}, err
	}
	r, err := u.session(convID)
	if err != nil {
		return SessionStatus{}, err
	}
	status := r.SessionStatus
	if status.Enabled && conv.MessageCount > 0 {
		msgs, err := m.main.ListMessages(convID)
		if err == nil {
			latest := latestCompletedID(msgs)
			status.Pending = latest > status.ArchivedBefore
		}
	}
	return status, nil
}

func (m *Manager) SetSessionEnabled(userID, convID int64, enabled bool) (SessionStatus, error) {
	conv, err := m.main.GetConversationForUser(convID, userID)
	if err != nil {
		return SessionStatus{}, err
	}
	if conv == nil {
		return SessionStatus{}, fmt.Errorf("会话不存在")
	}
	u, err := m.user(userID)
	if err != nil {
		return SessionStatus{}, err
	}
	if err := u.setEnabled(convID, enabled); err != nil {
		return SessionStatus{}, err
	}
	return m.SessionStatus(userID, convID)
}

func (m *Manager) Enabled(userID, convID int64) bool {
	u, err := m.user(userID)
	if err != nil {
		return false
	}
	r, err := u.session(convID)
	return err == nil && r.Enabled
}

func (m *Manager) ReadIndex(userID int64) (string, error) {
	u, err := m.user(userID)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(u.root, "index.txt"))
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(data), err
}

// ReadArchive 只允许通过当前用户清单里的 session_id 读取，绝不接受模型给的路径。
func (m *Manager) ReadArchive(userID, convID int64, offset, limit int) (string, int, bool, error) {
	conv, err := m.main.GetConversationForUser(convID, userID)
	if err != nil {
		return "", 0, false, err
	}
	if conv == nil {
		return "", 0, false, fmt.Errorf("归档会话不存在")
	}
	u, err := m.user(userID)
	if err != nil {
		return "", 0, false, err
	}
	r, err := u.session(convID)
	if err != nil {
		return "", 0, false, err
	}
	if r.ArchivePath == "" {
		return "", 0, false, fmt.Errorf("这个会话尚未归档；记忆每天 04:00 统一更新")
	}
	path, err := u.resolveArchive(r.ArchivePath)
	if err != nil {
		return "", 0, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, false, err
	}
	runes := []rune(string(data))
	total := len(runes)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	if limit <= 0 || limit > 30000 {
		limit = 30000
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return string(runes[offset:end]), total, end < total, nil
}

func (u *userStore) resolveArchive(relative string) (string, error) {
	if filepath.IsAbs(relative) || strings.Contains(relative, "..") {
		return "", fmt.Errorf("非法归档路径")
	}
	path := filepath.Join(u.root, filepath.FromSlash(relative))
	rel, err := filepath.Rel(u.root, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("归档路径越界")
	}
	return path, nil
}

func (m *Manager) DeleteSession(userID, convID int64) error {
	u, err := m.user(userID)
	if err != nil {
		return err
	}
	r, err := u.session(convID)
	if err != nil {
		return err
	}
	if r.ArchivePath != "" {
		if path, err := u.resolveArchive(r.ArchivePath); err == nil {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	if err := u.deleteSession(convID); err != nil {
		return err
	}
	return m.rebuildIndexes(u)
}

func (m *Manager) DeleteUser(userID int64) error {
	m.mu.Lock()
	if u := m.users[userID]; u != nil {
		_ = u.close()
		delete(m.users, userID)
	}
	m.mu.Unlock()
	root := m.UserRoot(userID)
	rel, err := filepath.Rel(filepath.Join(m.root, "users"), root)
	if err != nil || rel != strconv.FormatInt(userID, 10) {
		return fmt.Errorf("非法用户记忆目录")
	}
	return os.RemoveAll(root)
}

func latestCompletedID(msgs []store.Message) int64 {
	var id int64
	for _, msg := range msgs {
		if msg.Status == store.StatusDone && (msg.Role == store.RoleUser || msg.Role == store.RoleAssistant) && strings.TrimSpace(msg.Content) != "" {
			id = msg.ID
		}
	}
	return id
}

func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= 500 {
		return s
	}
	return string([]rune(s)[:500])
}
