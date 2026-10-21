package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// 多租户隔离的 store 层单元测试：users CRUD、ForUser 系列、归属计数、引导迁移。
// 这些是多租户的硬边界，破了任何一个都是「看穿别人数据」级别的事故。

func newTenantStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestUsersCRUDAndTokenHash(t *testing.T) {
	st := newTenantStore(t)

	admin, err := st.CreateUser("admin", HashToken("tok-admin"), true)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := st.CreateUser("guest", HashToken("tok-guest"), false)
	if err != nil {
		t.Fatal(err)
	}

	// 令牌按哈希解析：明文正确 → 命中；错一个字节 → nil
	if u, _ := st.UserByTokenHash(HashToken("tok-admin")); u == nil || u.ID != admin.ID {
		t.Fatalf("tok-admin 应解析到 admin，实际 %+v", u)
	}
	if u, _ := st.UserByTokenHash(HashToken("tok-guesx")); u != nil {
		t.Fatalf("错令牌应解析不到人，实际 %+v", u)
	}
	// 令牌哈希唯一约束
	if _, err := st.CreateUser("dup", HashToken("tok-admin"), false); err == nil {
		t.Fatal("相同令牌哈希应被唯一约束拒绝")
	}

	// 轮换：旧哈希失效，新哈希生效
	if ok, _ := st.RotateUserToken(guest.ID, HashToken("tok-new")); !ok {
		t.Fatal("轮换应成功")
	}
	if u, _ := st.UserByTokenHash(HashToken("tok-guest")); u != nil {
		t.Fatal("旧令牌应立即失效")
	}
	if u, _ := st.UserByTokenHash(HashToken("tok-new")); u == nil || u.ID != guest.ID {
		t.Fatal("新令牌应生效")
	}

	list, _ := st.ListUsers()
	if len(list) != 2 {
		t.Fatalf("应有 2 个用户，实际 %d", len(list))
	}
}

func TestDeleteUserCascadesAndProtectsLastAdmin(t *testing.T) {
	st := newTenantStore(t)
	admin, _ := st.CreateUser("admin", HashToken("a"), true)
	guest, _ := st.CreateUser("guest", HashToken("g"), false)

	p, _ := st.CreateProvider(guest.ID, "p", "https://x/v1", "k")
	conv, _ := st.CreateConversation(guest.ID, "c", p.ID, "", "")
	_, _ = st.CreateMessage(Message{ConvID: conv.ID, Role: RoleUser, Status: StatusDone, Content: "hi"})
	_ = st.SetSetting(guest.ID, SettingDefaultModel, "m")
	f, _ := st.CreateFile(File{UserID: guest.ID, Hash: "h1", Name: "a.txt"})

	// 最后一个管理员不可删
	if err := st.DeleteUser(admin.ID); err == nil || !strings.Contains(err.Error(), "最后一个管理员") {
		t.Fatalf("删最后一个管理员应被拒，实际 %v", err)
	}

	if err := st.DeleteUser(guest.ID); err != nil {
		t.Fatalf("删用户失败: %v", err)
	}
	// 名下数据全部清掉
	if c, _ := st.GetConversation(conv.ID); c != nil {
		t.Error("会话没被级联删除")
	}
	if p2, _ := st.GetProviderByID(p.ID); p2 != nil {
		t.Error("供应商没被级联删除")
	}
	if f2, _ := st.GetFile(f.ID); f2 != nil {
		t.Error("文件记录没被级联删除")
	}
	if v, _ := st.GetSetting(guest.ID, SettingDefaultModel); v != "" {
		t.Error("用户设置没被级联删除")
	}
	var msgCount int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conv_id = ?`, conv.ID).Scan(&msgCount)
	if msgCount != 0 {
		t.Error("消息没被级联删除")
	}
	// 管理员不受影响
	if u, _ := st.GetUser(admin.ID); u == nil {
		t.Error("管理员被误删")
	}
}

func TestProviderAndConversationForUser(t *testing.T) {
	st := newTenantStore(t)
	p1, _ := st.CreateProvider(1, "p1", "https://a/v1", "k1")
	_, _ = st.CreateProvider(2, "p2", "https://b/v1", "k2")
	c1, _ := st.CreateConversation(1, "c1", p1.ID, "", "")
	c2, _ := st.CreateConversation(2, "c2", 0, "", "")

	// GetProviderForUser：别人的返回 nil
	if p, _ := st.GetProviderForUser(p1.ID, 2); p != nil {
		t.Error("用户2不应看到用户1的供应商")
	}
	if p, _ := st.GetProviderForUser(p1.ID, 1); p == nil {
		t.Error("用户1应看到自己的供应商")
	}
	// GetConversationForUser：别人的返回 nil
	if c, _ := st.GetConversationForUser(c1.ID, 2); c != nil {
		t.Error("用户2不应看到用户1的会话")
	}
	if c, _ := st.GetConversationForUser(c2.ID, 2); c == nil {
		t.Error("用户2应看到自己的会话")
	}
	// ListConversations / ListProviders 严格按属主过滤
	if l, _ := st.ListConversations(1); len(l) != 1 || l[0].ID != c1.ID {
		t.Errorf("用户1应只看到 c1，实际 %+v", l)
	}
	if l, _ := st.ListProviders(2); len(l) != 1 || l[0].Name != "p2" {
		t.Errorf("用户2应只看到 p2，实际 %+v", l)
	}
	// ResolveDefaults：指定的供应商是别人的 → 回退，绝不绑定过去
	pid, _, _ := st.ResolveDefaults(2, p1.ID, "", "")
	if pid == p1.ID {
		t.Error("用户2指定用户1的供应商应被回退，绝不许绑定")
	}
	// ResolveProvider：别人供应商 id 回退到本人默认
	p, _ := st.ResolveProvider(2, p1.ID)
	if p == nil || p.Name != "p2" {
		t.Errorf("用户2解析用户1的供应商应回退到 p2，实际 %+v", p)
	}
}

func TestAttachmentAndFileOwnershipCounters(t *testing.T) {
	st := newTenantStore(t)
	p1, _ := st.CreateProvider(1, "p", "https://a/v1", "k")
	conv1, _ := st.CreateConversation(1, "c", p1.ID, "", "")
	msg1, _ := st.CreateMessage(Message{ConvID: conv1.ID, Role: RoleUser, Status: StatusDone, Content: "x"})

	// 附件：1 号上传者的 + 哈希去重共享的
	mine, _ := st.CreateAttachment(Attachment{UserID: 1, Hash: "h-mine", MIME: "image/png", Ext: ".png"})
	theirs, _ := st.CreateAttachment(Attachment{UserID: 2, Hash: "h-theirs", MIME: "image/png", Ext: ".png"})
	_ = st.AttachToMessage(msg1.ID, []int64{mine.ID})

	// 归属计数：自己的算，别人的不算
	if n, _ := st.CountAttachmentsOwnedBy([]int64{mine.ID, theirs.ID}, 1); n != 1 {
		t.Errorf("用户1只拥有 1 个附件，实际 %d", n)
	}
	if n, _ := st.CountAttachmentsOwnedBy([]int64{mine.ID, theirs.ID}, 2); n != 1 {
		t.Errorf("用户2只拥有 1 个附件，实际 %d", n)
	}

	// 哈希去重共享：用户2 上传同内容复用用户1 的行，挂到自己会话后自己也「可见」
	p2, _ := st.CreateProvider(2, "p", "https://b/v1", "k")
	conv2, _ := st.CreateConversation(2, "c", p2.ID, "", "")
	msg2, _ := st.CreateMessage(Message{ConvID: conv2.ID, Role: RoleUser, Status: StatusDone, Content: "x"})
	dup, _ := st.CreateAttachment(Attachment{UserID: 2, Hash: "h-mine", MIME: "image/png", Ext: ".png"})
	if dup.ID != mine.ID {
		t.Fatal("同内容应复用同一行（哈希去重）")
	}
	_ = st.AttachToMessage(msg2.ID, []int64{dup.ID})
	if a, _ := st.GetAttachmentForUser(mine.ID, 2); a == nil {
		t.Error("复用共享附件并挂到自己会话后，用户2 应可见")
	}
	if a, _ := st.GetAttachmentForUser(theirs.ID, 1); a != nil {
		t.Error("用户1 不应看到用户2 的附件")
	}

	// 文件同理
	fmine, _ := st.CreateFile(File{UserID: 1, Hash: "f-mine", Name: "a.txt"})
	ftheirs, _ := st.CreateFile(File{UserID: 2, Hash: "f-theirs", Name: "b.txt"})
	if n, _ := st.CountFilesOwnedBy([]int64{fmine.ID, ftheirs.ID}, 1); n != 1 {
		t.Errorf("用户1只拥有 1 个文件，实际 %d", n)
	}
}

func TestBootstrapUsersIdempotent(t *testing.T) {
	st := newTenantStore(t)

	// 空表引导：建 admin，令牌生效
	tok, created, err := st.BootstrapUsers("legacy-token")
	if err != nil || !created || tok != "legacy-token" {
		t.Fatalf("首次引导失败: tok=%q created=%v err=%v", tok, created, err)
	}
	u, _ := st.UserByTokenHash(HashToken("legacy-token"))
	if u == nil || !u.IsAdmin {
		t.Fatal("引导的管理员没生效")
	}

	// 再跑一遍：幂等，不重复建人
	if _, created, err := st.BootstrapUsers("legacy-token"); err != nil || created {
		t.Fatalf("重复引导应幂等，实际 created=%v err=%v", created, err)
	}
	if list, _ := st.ListUsers(); len(list) != 1 {
		t.Fatalf("应仍只有 1 个用户，实际 %d", len(list))
	}

	// 应急通道：带了别人没有的新令牌 → 补建管理员
	if _, created, err := st.BootstrapUsers("rescue-token"); err != nil || !created {
		t.Fatalf("应急通道应补建管理员，实际 created=%v err=%v", created, err)
	}
	u2, _ := st.UserByTokenHash(HashToken("rescue-token"))
	if u2 == nil || !u2.IsAdmin {
		t.Fatal("应急管理员没生效")
	}
}
