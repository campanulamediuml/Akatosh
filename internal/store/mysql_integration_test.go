package store

import (
	"os"
	"testing"
	"time"
)

// 需要显式提供 MySQL 测试库；测试会创建一个用户并在结束时级联删除其数据。
func TestMySQLStoreCRUD(t *testing.T) {
	dsn := os.Getenv("AKATOSH_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 AKATOSH_MYSQL_TEST_DSN")
	}
	st, err := OpenMySQL(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	user, err := st.CreateUser("mysql-integration", HashToken("mysql-integration-"+time.Now().String()), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.DeleteUser(user.ID); err != nil {
			t.Errorf("清理测试用户失败: %v", err)
		}
	}()
	if err := st.SetSetting(user.ID, SettingDefaultModel, "model-a"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(user.ID, SettingDefaultModel, "model-b"); err != nil {
		t.Fatal(err)
	}
	if value, _ := st.GetSetting(user.ID, SettingDefaultModel); value != "model-b" {
		t.Fatalf("upsert 设置失败: %q", value)
	}

	provider, err := st.CreateProvider(user.ID, "测试网关", "https://example.com/v1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModels(provider.ID, []Model{{ModelID: "model-a", Reasoning: true, Efforts: []string{"low"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModels(provider.ID, []Model{{ModelID: "model-a", Reasoning: true, Efforts: []string{"low", "high"}}}); err != nil {
		t.Fatal(err)
	}
	model, err := st.GetModelFor(provider.ID, "model-a")
	if err != nil || model == nil || len(model.Efforts) != 2 {
		t.Fatalf("模型 upsert 失败: %+v %v", model, err)
	}

	conv, err := st.CreateConversation(user.ID, "MySQL 测试", provider.ID, "model-a", "low")
	if err != nil {
		t.Fatal(err)
	}
	message, err := st.CreateMessage(Message{ConvID: conv.ID, Role: RoleUser, Content: "你好", Reasoning: "思考"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateMessageStream(message.ID, "新思考", "新正文"); err != nil {
		t.Fatal(err)
	}
	attachment, err := st.CreateAttachment(Attachment{UserID: user.ID, Hash: "mysql-attachment-" + time.Now().String(), Name: "a.png"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AttachToMessage(message.ID, []int64{attachment.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.AttachToMessage(message.ID, []int64{attachment.ID}); err != nil {
		t.Fatal(err)
	}
	file, err := st.CreateFile(File{UserID: user.ID, Hash: "mysql-file-" + time.Now().String(), Name: "a.pdf", Ext: ".pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AttachFileToMessage(message.ID, []int64{file.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.AttachFileToMessage(message.ID, []int64{file.ID}); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.GetMessageWithAttachments(message.ID)
	if err != nil || loaded.Content != "新正文" || len(loaded.Attachments) != 1 {
		t.Fatalf("消息或附件读取失败: %+v %v", loaded, err)
	}
	files, err := st.FilesByMessageIDs([]int64{message.ID})
	if err != nil || len(files[message.ID]) != 1 {
		t.Fatalf("文件关联读取失败: %+v %v", files, err)
	}
}
