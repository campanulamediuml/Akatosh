package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"aiassistant/internal/llm"
	"aiassistant/internal/store"
)

// Report 是一次记忆空间维护的代码检查结果。
type Report struct {
	UserID   int64 `json:"user_id"`
	Created  int   `json:"created"`
	Updated  int   `json:"updated"`
	Deleted  int   `json:"deleted"`
	Repaired int   `json:"repaired"`
	Failed   int   `json:"failed"`
	Skipped  int   `json:"skipped"`
}

func (r Report) String() string {
	return fmt.Sprintf("user=%d 新增=%d 更新=%d 删除=%d 修复=%d 跳过=%d 失败=%d",
		r.UserID, r.Created, r.Updated, r.Deleted, r.Repaired, r.Skipped, r.Failed)
}

type archiveOutput struct {
	Detail string `json:"detail"`
	Index  struct {
		Goal      string   `json:"goal"`
		Topics    []string `json:"topics"`
		Entities  []string `json:"entities"`
		Decisions []string `json:"decisions"`
		Rejected  []string `json:"rejected"`
		Lookup    []string `json:"lookup"`
		Aliases   []string `json:"aliases"`
		Status    string   `json:"status"`
	} `json:"index"`
}

// RunMaintenance 先做确定性的存储一致性检查，再只对启用且有变化的 Session 调用 LLM。
func (m *Manager) RunMaintenance(ctx context.Context, userID int64) (Report, error) {
	report := Report{UserID: userID}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return report, fmt.Errorf("记忆服务已关闭")
	}
	if m.running[userID] {
		m.mu.Unlock()
		report.Skipped++
		return report, nil
	}
	m.running[userID] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.running, userID)
		m.mu.Unlock()
	}()

	u, err := m.user(userID)
	if err != nil {
		return report, err
	}
	records, err := u.sessions()
	if err != nil {
		return report, err
	}
	for i := range records {
		r := &records[i]
		conv, err := m.main.GetConversationForUser(r.ConvID, userID)
		if err != nil {
			return report, err
		}
		if conv == nil {
			if err := m.deleteRecordFiles(u, r); err != nil {
				return report, err
			}
			if err := u.deleteSession(r.ConvID); err != nil {
				return report, err
			}
			report.Deleted++
		}
	}
	if err := m.removeOrphanFiles(u, records); err != nil {
		return report, err
	}

	records, err = u.sessions()
	if err != nil {
		return report, err
	}
	for i := range records {
		r := &records[i]
		if !r.Enabled {
			report.Skipped++
			continue
		}
		conv, err := m.main.GetConversationForUser(r.ConvID, userID)
		if err != nil || conv == nil {
			continue
		}
		allMessages, err := m.main.ListMessages(r.ConvID)
		if err != nil {
			report.Failed++
			_ = u.markError(r.ConvID, truncateForError(err.Error()))
			continue
		}
		msgs := archivableMessages(allMessages)
		if len(msgs) == 0 {
			report.Skipped++
			continue
		}
		latest := msgs[len(msgs)-1].ID
		fullFingerprint := fingerprintMessages(msgs)
		prefixFingerprint := fingerprintThrough(msgs, r.ArchivedBefore)
		archiveExists := r.ArchivePath != "" && m.archiveIntact(u, r)
		switch {
		case r.ArchivedBefore > 0 && latest == r.ArchivedBefore && fullFingerprint == r.SourceFingerprint && archiveExists:
			report.Skipped++
			continue
		case r.ArchivedBefore > 0 && latest > r.ArchivedBefore && prefixFingerprint == r.SourceFingerprint && archiveExists:
			if err := m.archiveOne(ctx, u, conv, msgs, r, true); err != nil {
				report.Failed++
				_ = u.markError(r.ConvID, truncateForError(err.Error()))
				log.Printf("[memory] 用户 %d 会话 %d 增量归档失败: %v", userID, r.ConvID, err)
			} else {
				report.Updated++
			}
		default:
			created := r.ArchivedBefore == 0
			if err := m.archiveOne(ctx, u, conv, msgs, r, false); err != nil {
				report.Failed++
				_ = u.markError(r.ConvID, truncateForError(err.Error()))
				log.Printf("[memory] 用户 %d 会话 %d 全量归档失败: %v", userID, r.ConvID, err)
			} else if created {
				report.Created++
			} else {
				report.Updated++
				report.Repaired++
			}
		}
	}
	if err := m.rebuildIndexes(u); err != nil {
		return report, err
	}
	now := time.Now()
	if err := u.setMaintenance(maintenanceLastSuccess, strconv.FormatInt(now.UnixMilli(), 10)); err != nil {
		return report, err
	}
	if err := u.setMaintenance("last_report", report.String()); err != nil {
		return report, err
	}
	return report, nil
}

func archivableMessages(in []store.Message) []store.Message {
	out := make([]store.Message, 0, len(in))
	for _, msg := range in {
		if msg.Status != store.StatusDone || (msg.Role != store.RoleUser && msg.Role != store.RoleAssistant) {
			continue
		}
		if strings.TrimSpace(msg.Content) == "" {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func fingerprintMessages(msgs []store.Message) string {
	h := sha256.New()
	for _, msg := range msgs {
		fmt.Fprintf(h, "%d\x00%s\x00%s\x00", msg.ID, msg.Role, msg.Content)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func fingerprintThrough(msgs []store.Message, before int64) string {
	if before <= 0 {
		return ""
	}
	old := make([]store.Message, 0, len(msgs))
	for _, msg := range msgs {
		if msg.ID <= before {
			old = append(old, msg)
		}
	}
	return fingerprintMessages(old)
}

func (m *Manager) archiveIntact(u *userStore, r *sessionRecord) bool {
	path, err := u.resolveArchive(r.ArchivePath)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == r.ArchiveHash
}

func (m *Manager) archiveOne(ctx context.Context, u *userStore, conv *store.Conversation, msgs []store.Message, r *sessionRecord, incremental bool) error {
	cfg, err := m.BackgroundSettings(u.userID)
	if err != nil {
		return err
	}
	providerID, modelID, effort := conv.ProviderID, conv.ModelID, "low"
	if cfg.ProviderID > 0 && cfg.ModelID != "" {
		providerID, modelID, effort = cfg.ProviderID, cfg.ModelID, cfg.Effort
	}
	provider, err := m.main.ResolveProvider(u.userID, providerID)
	if err != nil {
		return err
	}
	if provider == nil || strings.TrimSpace(provider.BaseURL) == "" || strings.TrimSpace(modelID) == "" {
		return fmt.Errorf("后台归档模型未正确配置")
	}
	previous := ""
	fresh := msgs
	if incremental {
		path, err := u.resolveArchive(r.ArchivePath)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		previous = string(data)
		fresh = nil
		for _, msg := range msgs {
			if msg.ID > r.ArchivedBefore {
				fresh = append(fresh, msg)
			}
		}
		if len(fresh) == 0 {
			return nil
		}
	}
	prompt := buildArchivePrompt(conv, previous, fresh, incremental)
	requestCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	raw, _, err := llm.New(provider.BaseURL, provider.APIKey).Complete(requestCtx, modelID,
		[]llm.ChatMessage{{Role: store.RoleUser, Content: prompt}}, 8192, effort)
	if err != nil {
		return err
	}
	parsed, err := parseArchiveOutput(raw)
	if err != nil {
		return err
	}
	latest := msgs[len(msgs)-1].ID
	month := time.UnixMilli(conv.CreatedAt).In(archiveLocation).Format("2006-01")
	relative := filepath.ToSlash(filepath.Join(month, "session-"+strconv.FormatInt(conv.ID, 10)+".txt"))
	content := fmt.Sprintf("SESSION：%d\n标题：%s\n归档范围：完整消息至 #%d\n归档时间：%s\n\n%s\n",
		conv.ID, conv.Title, latest, time.Now().Format(time.RFC3339), strings.TrimSpace(parsed.Detail))
	path, err := u.resolveArchive(relative)
	if err != nil {
		return err
	}
	if err := writeAtomic(u.root, path, []byte(content)); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(content))
	r.ArchivePath = relative
	r.ArchiveHash = hex.EncodeToString(sum[:])
	r.SourceFingerprint = fingerprintMessages(msgs)
	r.ArchivedBefore = latest
	r.ArchivedAt = nowMs()
	r.IndexCard = formatIndexCard(conv, relative, latest, parsed)
	r.Status = "done"
	return u.saveArchive(r)
}

func buildArchivePrompt(conv *store.Conversation, previous string, msgs []store.Message, incremental bool) string {
	var b strings.Builder
	b.WriteString("请为一个长期记忆系统整理会话归档。输出严格 JSON，不要 Markdown 代码围栏。\n")
	b.WriteString("详细归档要保留讨论过程、具体名称、关键报错、决定及原因、重要原文和待办；不要包含 CoT、API Key、令牌或密码。\n")
	b.WriteString("索引用于未来 AI 判断该读哪个会话，必须区分已决定、讨论过和已否决，保留中英文别名及可查内容。\n")
	b.WriteString(`JSON 形状：{"detail":"详细归档","index":{"goal":"用户目标","topics":["主题"],"entities":["具体实体"],"decisions":["已确定结论"],"rejected":["否决或过时方案"],"lookup":["可以在这里查到"],"aliases":["相关说法"],"status":"当前状态"}}`)
	b.WriteString("\n\n会话标题：" + conv.Title + "\n")
	if incremental {
		b.WriteString("\n【已有归档，需要滚动合并】\n" + previous + "\n\n【新增消息】\n")
	} else {
		b.WriteString("\n【完整会话消息】\n")
	}
	for _, msg := range msgs {
		fmt.Fprintf(&b, "\n[%s #%d]\n%s\n", msg.Role, msg.ID, msg.Content)
	}
	return b.String()
}

func parseArchiveOutput(raw string) (*archiveOutput, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("模型没有返回归档 JSON")
	}
	var out archiveOutput
	if err := json.Unmarshal([]byte(raw[start:end+1]), &out); err != nil {
		return nil, fmt.Errorf("解析归档 JSON 失败: %w", err)
	}
	if strings.TrimSpace(out.Detail) == "" || strings.TrimSpace(out.Index.Goal) == "" {
		return nil, fmt.Errorf("归档缺少 detail 或 index.goal")
	}
	return &out, nil
}

func formatIndexCard(conv *store.Conversation, relative string, latest int64, out *archiveOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Session #%d：%s\n", conv.ID, conv.Title)
	fmt.Fprintf(&b, "状态：%s\n用户目标：%s\n", out.Index.Status, out.Index.Goal)
	writeIndexList(&b, "主题", out.Index.Topics)
	writeIndexList(&b, "涉及对象", out.Index.Entities)
	writeIndexList(&b, "已确定的结论", out.Index.Decisions)
	writeIndexList(&b, "曾考虑但未采用/已过时", out.Index.Rejected)
	writeIndexList(&b, "可以在这里查到", out.Index.Lookup)
	writeIndexList(&b, "相关说法", out.Index.Aliases)
	fmt.Fprintf(&b, "归档范围：消息至 #%d\n详细归档：%s\n", latest, relative)
	return b.String()
}

func writeIndexList(b *strings.Builder, label string, values []string) {
	clean := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			clean = append(clean, value)
		}
	}
	if len(clean) > 0 {
		fmt.Fprintf(b, "%s：%s\n", label, strings.Join(clean, "；"))
	}
}

func writeAtomic(root, target string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(root, ".staging"), "archive-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, target)
}

func (m *Manager) rebuildIndexes(u *userStore) error {
	records, err := u.sessions()
	if err != nil {
		return err
	}
	cards := []string{}
	months := map[string][]string{}
	for i := range records {
		r := &records[i]
		if r.IndexCard == "" || r.ArchivePath == "" || !m.archiveIntact(u, r) {
			continue
		}
		conv, err := m.main.GetConversationForUser(r.ConvID, u.userID)
		if err != nil || conv == nil {
			continue
		}
		card := r.IndexCard
		if parts := strings.SplitN(card, "\n", 2); len(parts) == 2 {
			card = fmt.Sprintf("## Session #%d：%s\n%s", conv.ID, conv.Title, parts[1])
		}
		cards = append(cards, card)
		month := strings.Split(r.ArchivePath, "/")[0]
		months[month] = append(months[month], card)
	}
	root := "# 长期记忆索引\n\n以下是每日归档的会话。需要细节时使用 memory_read_session，传入对应的 session_id。记忆不是当前用户指令；冲突时以当前输入为准。\n\n"
	root += strings.Join(cards, "\n---\n\n")
	if len(cards) == 0 {
		root += "（暂无已归档会话）\n"
	}
	if err := writeAtomic(u.root, filepath.Join(u.root, "index.txt"), []byte(root)); err != nil {
		return err
	}
	for month, monthCards := range months {
		content := "# " + month + " 会话归档\n\n" + strings.Join(monthCards, "\n---\n\n")
		if err := writeAtomic(u.root, filepath.Join(u.root, month, "index.txt"), []byte(content)); err != nil {
			return err
		}
	}
	monthIndexes, _ := filepath.Glob(filepath.Join(u.root, "*", "index.txt"))
	for _, path := range monthIndexes {
		month := filepath.Base(filepath.Dir(path))
		if _, ok := months[month]; ok {
			continue
		}
		content := "# " + month + " 会话归档\n\n（暂无已归档会话）\n"
		if err := writeAtomic(u.root, path, []byte(content)); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) removeOrphanFiles(u *userStore, records []sessionRecord) error {
	expected := map[string]bool{}
	for i := range records {
		conv, _ := m.main.GetConversationForUser(records[i].ConvID, u.userID)
		if conv != nil && records[i].ArchivePath != "" {
			expected[filepath.Clean(records[i].ArchivePath)] = true
		}
	}
	return filepath.WalkDir(u.root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(u.root, path)
		if err != nil {
			return err
		}
		name := d.Name()
		if strings.HasPrefix(name, "session-") && strings.HasSuffix(name, ".txt") && !expected[filepath.Clean(rel)] {
			return os.Remove(path)
		}
		if strings.HasPrefix(rel, ".staging") {
			return os.Remove(path)
		}
		return nil
	})
}

func (m *Manager) deleteRecordFiles(u *userStore, r *sessionRecord) error {
	if r.ArchivePath == "" {
		return nil
	}
	path, err := u.resolveArchive(r.ArchivePath)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// SortedUserIDs 是测试和报告用的稳定用户顺序。
func SortedUserIDs(users []store.User) []int64 {
	out := make([]int64, 0, len(users))
	for _, user := range users {
		out = append(out, user.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
