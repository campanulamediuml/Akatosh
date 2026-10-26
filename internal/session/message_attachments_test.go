package session

import (
	"context"
	"fmt"
	"testing"
	"time"

	"aiassistant/internal/store"
)

func waitMessagePayload(t *testing.T, events <-chan Event, messageID int64, action string) *MessagePayload {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case ev := <-events:
			if ev.Type != EventMessage {
				continue
			}
			payload, ok := ev.Payload.(MessagePayload)
			if ok && payload.MessageID == messageID && payload.Action == action {
				return &payload
			}
		case <-timer.C:
			t.Fatalf("等待消息 %d 的 %s 事件超时", messageID, action)
		}
	}
}

func requireMessageFile(t *testing.T, msg *store.Message, fileID int64) {
	t.Helper()
	if msg == nil || len(msg.Files) != 1 {
		t.Fatalf("消息应带一个文件: %+v", msg)
	}
	file := msg.Files[0]
	if file.ID != fileID || file.Name != "note.txt" || file.Size != 12 {
		t.Errorf("文件字段不完整: %+v", file)
	}
}

func requireMessageImage(t *testing.T, msg *store.Message, attachmentID int64) {
	t.Helper()
	if msg == nil || len(msg.Attachments) != 1 {
		t.Fatalf("消息应带一张图片: %+v", msg)
	}
	attachment := msg.Attachments[0]
	if attachment.ID != attachmentID {
		t.Fatalf("图片 id 不对: %+v", attachment)
	}
	if attachment.ThumbURL != fmt.Sprintf("/api/attachments/%d/thumb", attachmentID) ||
		attachment.RawURL != fmt.Sprintf("/api/attachments/%d/raw", attachmentID) {
		t.Errorf("图片 URL 不完整: %+v", attachment)
	}
}

func TestSendMessageEventIncludesFiles(t *testing.T) {
	up := newUpstream(t, time.Millisecond)
	defer up.Close()
	mgr, st := newTestManager(t, up.URL)
	conv, err := st.CreateConversation(1, "文件事件", 0, "m-reason", "high")
	if err != nil {
		t.Fatal(err)
	}
	file, err := st.CreateFile(store.File{UserID: 1, Hash: "send-file", Name: "note.txt", Ext: ".txt", Size: 12})
	if err != nil {
		t.Fatal(err)
	}
	_, events, _, cancel := mgr.Bus().Subscribe(1)
	defer cancel()
	messageID, _, err := mgr.SendMessage(context.Background(), 1, conv.ID, "带文件", 0, "", nil, nil, []int64{file.ID})
	if err != nil {
		t.Fatal(err)
	}
	payload := waitMessagePayload(t, events, messageID, ActionCreated)
	waitIdle(t, mgr, conv.ID)
	requireMessageFile(t, payload.Message, file.ID)
}

func TestMessageUpdatesKeepAttachmentsAndFiles(t *testing.T) {
	mgr, st := newTestManager(t, "http://unused.invalid")
	conv, err := st.CreateConversation(1, "编辑附件", 0, "m-reason", "high")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := st.CreateMessage(store.Message{ConvID: conv.ID, Role: store.RoleUser, Status: store.StatusDone, Content: "原始正文"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateMessageStream(msg.ID, "原始思考", msg.Content); err != nil {
		t.Fatal(err)
	}
	file, err := st.CreateFile(store.File{UserID: 1, Hash: "edit-file", Name: "note.txt", Ext: ".txt", Size: 12})
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := st.CreateAttachment(store.Attachment{
		UserID: 1, Hash: "edit-image", Kind: "image", MIME: "image/png", Ext: ".png",
		Name: "shot.png", Size: 100, Width: 10, Height: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AttachFileToMessage(msg.ID, []int64{file.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.AttachToMessage(msg.ID, []int64{attachment.ID}); err != nil {
		t.Fatal(err)
	}
	_, events, _, cancel := mgr.Bus().Subscribe(1)
	defer cancel()

	for _, tc := range []struct {
		name string
		run  func() (*store.Message, error)
	}{
		{name: "编辑正文", run: func() (*store.Message, error) {
			return mgr.EditMessage(context.Background(), 1, msg.ID, "编辑后的正文")
		}},
		{name: "还原正文", run: func() (*store.Message, error) {
			return mgr.RestoreMessage(context.Background(), 1, msg.ID)
		}},
		{name: "编辑思考", run: func() (*store.Message, error) {
			return mgr.EditMessageReasoning(context.Background(), 1, msg.ID, "编辑后的思考")
		}},
		{name: "还原思考", run: func() (*store.Message, error) {
			return mgr.RestoreMessageReasoning(context.Background(), 1, msg.ID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updated, err := tc.run()
			if err != nil {
				t.Fatal(err)
			}
			requireMessageFile(t, updated, file.ID)
			requireMessageImage(t, updated, attachment.ID)
			payload := waitMessagePayload(t, events, msg.ID, ActionUpdated)
			requireMessageFile(t, payload.Message, file.ID)
			requireMessageImage(t, payload.Message, attachment.ID)
		})
	}
}
