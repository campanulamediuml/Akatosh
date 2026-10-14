package session

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"aiassistant/internal/store"
)

// 造一个带若干消息的会话（messages 内容形如 "m1"、"m2"…）
func seedConv(t *testing.T, st *store.Store, n int) *store.Conversation {
	t.Helper()
	p, err := st.CreateProvider("p", "http://127.0.0.1:1/v1", "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModels(p.ID, []store.Model{{ModelID: "m", Reasoning: true, Efforts: []string{"high"}, ContextLen: 10000}}); err != nil {
		t.Fatal(err)
	}
	conv, err := st.CreateConversation("t", p.ID, "m", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		role := store.RoleUser
		if i%2 == 1 {
			role = store.RoleAssistant
		}
		if _, err := st.CreateMessage(store.Message{ConvID: conv.ID, Role: role, Status: store.StatusDone, Content: "msg", Reasoning: "cot"}); err != nil {
			t.Fatal(err)
		}
	}
	return conv
}

// buildHistory：有摘要时历史 = system 摘要 + 近期消息，旧消息被排除。
func TestBuildHistoryWithSummary(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	conv := seedConv(t, st, 20)

	// 压到第 10 条（含），保留后 10 条
	msgs, _ := st.ListMessages(conv.ID)
	if err := st.SetConversationSummary(conv.ID, "前 10 条的摘要", msgs[9].ID); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(st, NewBus(), nil)
	s, err := mgr.getOrCreate(conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist := s.buildHistory("m")

	var systemCount, oldCount int
	for _, m := range hist {
		if m.Role == store.RoleSystem {
			systemCount++
			if s2, ok := m.Content.(string); ok && strings.Contains(s2, "前 10 条的摘要") {
				oldCount++
			}
		}
	}
	if systemCount != 2 || oldCount != 1 {
		t.Fatalf("历史里应有日期+摘要两条 system: system=%d summary=%d", systemCount, oldCount)
	}
	// 用户消息 10 条（后 10 条中的一半）+ 摘要 1 条
	userCount := 0
	for _, m := range hist {
		if m.Role == store.RoleUser {
			userCount++
		}
	}
	if userCount != 5 {
		t.Fatalf("近期消息应保留（后 10 条中的 5 条用户消息），实际 %d", userCount)
	}
}

// 冻结：compacted_before 之前的消息编辑/还原/删除都被拒绝（archived）。
func TestFrozenMessagesRejected(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	conv := seedConv(t, st, 6)
	msgs, _ := st.ListMessages(conv.ID)
	if err := st.SetConversationSummary(conv.ID, "摘要", msgs[2].ID); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(st, NewBus(), nil)
	// 冻结消息（第 1 条）：编辑正文、编辑 CoT、还原、删除都应 archived
	frozenID := msgs[0].ID
	for name, fn := range map[string]error{
		"edit":          func() error { _, e := mgr.EditMessage(context.Background(), frozenID, "x"); return e }(),
		"editReasoning": func() error { _, e := mgr.EditMessageReasoning(context.Background(), frozenID, "x"); return e }(),
		"restore":       func() error { _, e := mgr.RestoreMessage(context.Background(), frozenID); return e }(),
		"restoreCot":    func() error { _, e := mgr.RestoreMessageReasoning(context.Background(), frozenID); return e }(),
		"delete":        func() error { return mgr.DeleteMessage(context.Background(), frozenID) }(),
	} {
		var se *Error
		if fn == nil || !errorAs(fn, &se) || se.Code != "archived" {
			t.Fatalf("%s 冻结消息应返回 archived，实际 %v", name, fn)
		}
	}
	// 未冻结消息（第 5 条）可以正常编辑
	if _, err := mgr.EditMessage(context.Background(), msgs[4].ID, "新内容"); err != nil {
		t.Fatalf("未冻结消息编辑应成功: %v", err)
	}
}

func errorAs(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		return false
	}
	return false
}

// 压缩中发送被拒绝（compacting）。
func TestSendRejectedWhileCompacting(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	conv := seedConv(t, st, 2)
	mgr := NewManager(st, NewBus(), nil)
	mgr.setCompacting(conv.ID, true)
	_, _, err = mgr.SendMessage(context.Background(), conv.ID, "hi", 0, "", nil, nil, nil)
	var se *Error
	if err == nil || !errorAs(err, &se) || se.Code != "compacting" {
		t.Fatalf("压缩中发送应返回 compacting，实际 %v", err)
	}
}
