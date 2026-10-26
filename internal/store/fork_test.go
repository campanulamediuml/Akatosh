package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

// 消息级分叉：锚点截断、标题格式、附件/文档关联复制、压缩冻结状态跟随。
func TestForkConversationFromMessage(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	p, _ := st.CreateProvider(1, "p", "https://a/v1", "k")
	// 先往别的会话塞一条，让主会话的消息 id 从 >1 开始（锚点不存在的测试要用）
	dummy, _ := st.CreateConversation(1, "别的会话", p.ID, "", "")
	if _, err := st.CreateMessage(Message{ConvID: dummy.ID, Role: RoleUser, Status: StatusDone, Content: "x"}); err != nil {
		t.Fatal(err)
	}
	conv, _ := st.CreateConversation(1, "原会话", p.ID, "m", "")

	var msgIDs []int64
	for i := 0; i < 5; i++ {
		role := RoleUser
		if i%2 == 1 {
			role = RoleAssistant
		}
		m, err := st.CreateMessage(Message{ConvID: conv.ID, Role: role, Status: StatusDone, Content: fmt.Sprintf("m%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		msgIDs = append(msgIDs, m.ID)
	}

	// m1 带图、m2 带文档
	att, _ := st.CreateAttachment(Attachment{UserID: 1, Hash: "h1", MIME: "image/png", Ext: ".png"})
	if err := st.AttachToMessage(msgIDs[1], []int64{att.ID}); err != nil {
		t.Fatal(err)
	}
	f, _ := st.CreateFile(File{UserID: 1, Hash: "f1", Name: "a.txt"})
	if err := st.AttachFileToMessage(msgIDs[2], []int64{f.ID}); err != nil {
		t.Fatal(err)
	}

	// 压缩冻结到 m2：fork 要把这个状态带走
	if err := st.SetConversationSummary(conv.ID, "前两条的摘要", msgIDs[1]); err != nil {
		t.Fatal(err)
	}

	// —— 从 m3（含）分叉 ——
	fork, err := st.ForkConversation(conv.ID, msgIDs[2], 1)
	if err != nil {
		t.Fatal(err)
	}
	if fork == nil {
		t.Fatal("fork 应成功")
	}
	wantTitle := fmt.Sprintf("原会话-消息%d-fork", msgIDs[2])
	if fork.Title != wantTitle {
		t.Errorf("标题应为 %q，实际 %q", wantTitle, fork.Title)
	}
	if fork.Summary != "前两条的摘要" {
		t.Errorf("摘要没带走: %q", fork.Summary)
	}
	// compacted_before 必须重映射到新 id：冻结的是原前 2 条 → 第 2 条复制后的新 id
	fmsgsEarly, _ := st.ListMessages(fork.ID)
	if len(fmsgsEarly) != 3 {
		t.Fatalf("fork 消息数不对: %d", len(fmsgsEarly))
	}
	if fork.CompactedBefore != fmsgsEarly[1].ID {
		t.Errorf("compacted_before 应重映射为第 2 条的新 id %d，实际 %d", fmsgsEarly[1].ID, fork.CompactedBefore)
	}
	var frozenN int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conv_id = ? AND id <= ?`, fork.ID, fork.CompactedBefore).Scan(&frozenN)
	if frozenN != 2 {
		t.Errorf("fork 里应有 2 条冻结消息，实际 %d", frozenN)
	}
	if fork.UserID != 1 {
		t.Errorf("fork 会话属主不对: %d", fork.UserID)
	}

	fmsgs, err := st.ListMessages(fork.ID)
	if err != nil || len(fmsgs) != 3 {
		t.Fatalf("fork 应只有锚点之前的 3 条消息，实际 %d (err=%v)", len(fmsgs), err)
	}
	for i, m := range fmsgs {
		if m.Content != fmt.Sprintf("m%d", i) {
			t.Errorf("第 %d 条内容不对: %q", i, m.Content)
		}
	}
	// 附件跟过来了
	fatts, _ := st.ListAttachments(fmsgs[1].ID)
	if len(fatts) != 1 || fatts[0].ID != att.ID {
		t.Errorf("fork 后 m1 的附件丢了: %+v", fatts)
	}
	// 文档也跟过来了（新需求）
	ffiles, _ := st.ListFiles(fmsgs[2].ID)
	if len(ffiles) != 1 || ffiles[0].ID != f.ID {
		t.Errorf("fork 后 m2 的文档丢了: %+v", ffiles)
	}
	// 原文档/原附件本体不复制（关联行共享同一 id，内容哈希去重）

	// —— 整会话分叉（beforeMsgID=0）= 锚到最后一条 ——
	whole, err := st.ForkConversation(conv.ID, 0, 1)
	if err != nil || whole == nil {
		t.Fatal("整会话分叉失败")
	}
	wantWhole := fmt.Sprintf("原会话-消息%d-fork", msgIDs[4])
	if whole.Title != wantWhole {
		t.Errorf("整会话分叉标题应为 %q，实际 %q", wantWhole, whole.Title)
	}
	wmsgs, _ := st.ListMessages(whole.ID)
	if len(wmsgs) != 5 {
		t.Errorf("整会话分叉应有全部 5 条，实际 %d", len(wmsgs))
	}

	// —— 锚点在本会话里不存在（id 小于第一条消息）→ nil ——
	tooSmall := msgIDs[0] - 1
	if tooSmall <= 0 {
		t.Fatalf("测试前提不成立：第一条消息 id 应大于 1，实际 %d", msgIDs[0])
	}
	if ghost, err := st.ForkConversation(conv.ID, tooSmall, 1); err != nil || ghost != nil {
		t.Errorf("锚点不存在应返回 nil，实际 %+v err=%v", ghost, err)
	}
}
