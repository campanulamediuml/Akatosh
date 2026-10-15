package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// 建一个「老版本」的库：v2 schema（conversations 还没有 provider_id），塞进数据，
// 再用当前代码打开它。升级上来的用户不该因为多供应商这次改动丢任何东西。
func TestMigrateV2ToV3BackfillsProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}

	old := []string{
		`CREATE TABLE settings (k TEXT PRIMARY KEY, v TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE providers (
			id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL DEFAULT '默认',
			base_url TEXT NOT NULL DEFAULT '', api_key TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE models (
			id INTEGER PRIMARY KEY AUTOINCREMENT, provider_id INTEGER NOT NULL,
			model_id TEXT NOT NULL, display_name TEXT NOT NULL DEFAULT '',
			reasoning INTEGER NOT NULL DEFAULT 0, efforts TEXT NOT NULL DEFAULT '[]',
			context_len INTEGER NOT NULL DEFAULT 0, raw TEXT NOT NULL DEFAULT '',
			fetched_at INTEGER NOT NULL, UNIQUE(provider_id, model_id))`,
		// 注意：老版本的 conversations **没有** provider_id
		`CREATE TABLE conversations (
			id INTEGER PRIMARY KEY AUTOINCREMENT, title TEXT NOT NULL DEFAULT '新对话',
			model_id TEXT NOT NULL DEFAULT '', effort TEXT NOT NULL DEFAULT 'medium',
			system_prompt TEXT NOT NULL DEFAULT '', pinned INTEGER NOT NULL DEFAULT 0,
			archived INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT, conv_id INTEGER NOT NULL, role TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'done', content TEXT NOT NULL DEFAULT '',
			reasoning TEXT NOT NULL DEFAULT '', content_original TEXT NOT NULL DEFAULT '',
			edited INTEGER NOT NULL DEFAULT 0, model_id TEXT NOT NULL DEFAULT '',
			effort TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
			prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0,
			cache_hit_tokens INTEGER NOT NULL DEFAULT 0, cache_miss_tokens INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL, finished_at INTEGER NOT NULL DEFAULT 0)`,
	}
	for _, q := range old {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("造老库失败: %v\nSQL: %s", err, q)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO providers(id,name,base_url,api_key,created_at,updated_at)
		 VALUES(1,'老供应商','https://old.example.com/v1','sk-old',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO conversations(id,title,model_id,effort,created_at,updated_at)
		 VALUES(1,'老对话','gpt-4o','high',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO models(provider_id,model_id,display_name,reasoning,efforts,context_len,fetched_at)
		 VALUES(1,'gpt-4o','gpt-4o',1,'["low","high"]',128000,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO messages(conv_id,role,status,content,created_at) VALUES(1,'user','done','你好',1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// 用当前代码打开 → 触发 v2 → v3 迁移
	st, err := Open(path)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	defer st.Close()

	// 真实升级路径：迁移后做用户引导，存量数据全部划给首个管理员（id=1）
	if _, _, err := st.BootstrapUsers(""); err != nil {
		t.Fatalf("用户引导失败: %v", err)
	}

	conv, err := st.GetConversation(1)
	if err != nil || conv == nil {
		t.Fatalf("老会话丢了: %v", err)
	}
	if conv.ProviderID != 1 {
		t.Fatalf("provider_id 没有回填，实际 %d", conv.ProviderID)
	}
	if conv.ProviderName != "老供应商" {
		t.Fatalf("provider_name 不对: %q", conv.ProviderName)
	}
	if conv.ModelID != "gpt-4o" || conv.Title != "老对话" || conv.Effort != "high" {
		t.Fatalf("老会话的字段被改坏了: %+v", conv)
	}

	msgs, err := st.ListMessages(1)
	if err != nil || len(msgs) != 1 || msgs[0].Content != "你好" {
		t.Fatalf("老消息丢了: %v %+v", err, msgs)
	}

	models, err := st.ListModels(1, 1)
	if err != nil || len(models) != 1 || !models[0].Reasoning || len(models[0].Efforts) != 2 {
		t.Fatalf("老模型缓存丢了或能力标志坏了: %v %+v", err, models)
	}

	// 默认供应商也该被顺手记住，否则新建会话会没着落
	if v, _ := st.GetSetting(1, SettingDefaultProvider); v != "1" {
		t.Fatalf("默认供应商没写上，实际 %q", v)
	}
	// 版本号要落到 3，否则每次启动都重跑迁移
	var ver int
	if err := st.db.QueryRow("PRAGMA user_version").Scan(&ver); err != nil || ver != schemaVersion {
		t.Fatalf("user_version 应为 %d，实际 %d (err=%v)", schemaVersion, ver, err)
	}
}

// 两个网关都有 gpt-4o：必须是两条独立记录，能力互不串台。
func TestSameModelNameUnderTwoProviders(t *testing.T) {
	st := newTestStore(t)

	a, err := st.CreateProvider(1, "网关A", "https://a.example.com/v1", "ka")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateProvider(1, "网关B", "https://b.example.com/v1", "kb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModels(a.ID, []Model{
		{ModelID: "gpt-4o", Reasoning: true, Efforts: []string{"low", "high"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModels(b.ID, []Model{
		{ModelID: "gpt-4o", Reasoning: false, Efforts: []string{}},
	}); err != nil {
		t.Fatal(err)
	}

	ma, err := st.GetModelFor(a.ID, "gpt-4o")
	if err != nil || ma == nil {
		t.Fatalf("A 家的 gpt-4o 取不到: %v", err)
	}
	mb, err := st.GetModelFor(b.ID, "gpt-4o")
	if err != nil || mb == nil {
		t.Fatalf("B 家的 gpt-4o 取不到: %v", err)
	}
	if ma.ID == mb.ID {
		t.Fatal("两家的 gpt-4o 应当是两条不同记录")
	}
	if !ma.Reasoning || mb.Reasoning {
		t.Fatalf("能力串台了: A.reasoning=%v B.reasoning=%v", ma.Reasoning, mb.Reasoning)
	}

	all, err := st.ListModels(1, 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("总模型数应为 2，实际 %d (err=%v)", len(all), err)
	}
	onlyA, err := st.ListModels(1, a.ID)
	if err != nil || len(onlyA) != 1 || onlyA[0].ProviderID != a.ID {
		t.Fatalf("按供应商过滤不对: %v %+v", err, onlyA)
	}
}

// 默认模型必须属于目标供应商，否则会被静默发到错的地方。
func TestResolveDefaultsKeepsModelInItsProvider(t *testing.T) {
	st := newTestStore(t)

	a, _ := st.CreateProvider(1, "A", "https://a.example.com/v1", "k")
	b, _ := st.CreateProvider(1, "B", "https://b.example.com/v1", "k")
	_, _ = st.UpsertModels(a.ID, []Model{{ModelID: "model-a"}})
	_, _ = st.UpsertModels(b.ID, []Model{{ModelID: "model-b"}})
	_ = st.SetSetting(1, SettingDefaultModel, "model-a") // 默认模型属于 A

	// 会话指到 B：不能把 model-a 用过去，应当落到 B 自己的 model-b
	pid, mid, _ := st.ResolveDefaults(1, b.ID, "", "")
	if pid != b.ID || mid != "model-b" {
		t.Fatalf("应当解析成 B/model-b，实际 %d/%s", pid, mid)
	}

	// 显式给了模型就照用
	pid2, mid2, _ := st.ResolveDefaults(1, b.ID, "model-b", "high")
	if pid2 != b.ID || mid2 != "model-b" {
		t.Fatalf("显式指定不该被改写，实际 %d/%s", pid2, mid2)
	}
}

// 供应商被删掉后，引用它的会话要能回退到默认供应商，而不是直接发不出去。
func TestDeleteProviderFallsBackToDefault(t *testing.T) {
	st := newTestStore(t)

	a, _ := st.CreateProvider(1, "A", "https://a.example.com/v1", "k") // 第一个建的 = 默认
	b, _ := st.CreateProvider(1, "B", "https://b.example.com/v1", "k")
	_, _ = st.UpsertModels(b.ID, []Model{{ModelID: "model-b"}})

	conv, err := st.CreateConversation(1, "挂在 B 上", b.ID, "model-b", "high")
	if err != nil {
		t.Fatal(err)
	}
	if conv.ProviderID != b.ID {
		t.Fatalf("会话没有绑到 B: %d", conv.ProviderID)
	}
	if conv.ProviderName != "B" {
		t.Fatalf("provider_name 不对: %q", conv.ProviderName)
	}

	if err := st.DeleteProvider(b.ID, 1); err != nil {
		t.Fatalf("删供应商失败: %v", err)
	}

	// 会话本身不该被删
	after, err := st.GetConversation(conv.ID)
	if err != nil || after == nil {
		t.Fatalf("会话被连带删了: %v", err)
	}
	if after.ProviderName != "" {
		t.Fatalf("供应商没了，provider_name 应当是空串，实际 %q", after.ProviderName)
	}
	// 但解析时要能回退到 A
	p, err := st.ResolveProvider(1, after.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || p.ID != a.ID {
		t.Fatalf("应当回退到 A，实际 %+v", p)
	}

	// B 的模型缓存应当被级联删掉
	models, _ := st.ListModels(1, b.ID)
	if len(models) != 0 {
		t.Fatalf("B 的模型缓存没有级联删除: %+v", models)
	}
}

// 未读小红点的语义：**只认已经答完的**，正在生成时不该亮；看过了就灭。
func TestUnreadFlag(t *testing.T) {
	st := newTestStore(t)
	conv, err := st.CreateConversation(1, "未读测试", 0, "", "")
	if err != nil {
		t.Fatal(err)
	}

	unread := func() bool {
		t.Helper()
		c, err := st.GetConversation(conv.ID)
		if err != nil || c == nil {
			t.Fatalf("读会话失败: %v", err)
		}
		return c.Unread
	}

	if unread() {
		t.Fatal("刚建的会话不该是未读")
	}

	// 只有用户消息 → 还没回答，不该未读
	if _, err := st.CreateMessage(Message{ConvID: conv.ID, Role: RoleUser, Status: StatusDone, Content: "问"}); err != nil {
		t.Fatal(err)
	}
	if unread() {
		t.Fatal("只有用户消息时不该显示未读")
	}

	// 助手消息还在生成中（finished_at=0）→ 红点应当等答完才亮
	asst, err := st.CreateMessage(Message{ConvID: conv.ID, Role: RoleAssistant, Status: StatusStreaming})
	if err != nil {
		t.Fatal(err)
	}
	if unread() {
		t.Fatal("正在生成时不该显示未读（红点要等答完才亮）")
	}

	// 答完 → 未读
	time.Sleep(3 * time.Millisecond)
	if err := st.FinishMessage(asst.ID, StatusDone, "想", "答", "", Usage{}, "", ""); err != nil {
		t.Fatal(err)
	}
	if !unread() {
		t.Fatal("回答落地后应当变成未读")
	}

	// 看过了 → 熄灭
	time.Sleep(3 * time.Millisecond)
	if err := st.MarkConversationRead(conv.ID); err != nil {
		t.Fatal(err)
	}
	if unread() {
		t.Fatal("标记已读后不该还是未读")
	}

	// 标记已读不该动 updated_at（否则「看一条消息」会把它无故顶到最前）
	before, _ := st.GetConversation(conv.ID)
	time.Sleep(3 * time.Millisecond)
	if err := st.MarkConversationRead(conv.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := st.GetConversation(conv.ID)
	if after.UpdatedAt != before.UpdatedAt {
		t.Fatal("标记已读不该改动 updated_at（那会破坏按活跃度排序）")
	}

	// 再来一轮答完 → 又变未读
	asst2, err := st.CreateMessage(Message{ConvID: conv.ID, Role: RoleAssistant, Status: StatusStreaming})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond)
	if err := st.FinishMessage(asst2.ID, StatusDone, "", "又答", "", Usage{}, "", ""); err != nil {
		t.Fatal(err)
	}
	if !unread() {
		t.Fatal("新一轮回答落地应当再次变未读")
	}
}

// 侧边栏靠 updated_at 倒序排，所以「谁更新谁在前」这条排序规则要锁住。
func TestConversationOrderFollowsUpdatedAt(t *testing.T) {
	st := newTestStore(t)
	a, _ := st.CreateConversation(1, "A", 0, "", "")
	time.Sleep(3 * time.Millisecond)
	b, _ := st.CreateConversation(1, "B", 0, "", "")

	list, err := st.ListConversations(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) < 2 || list[0].ID != b.ID {
		t.Fatalf("最近更新的 B 应当排在最前，实际第一个是 %d", list[0].ID)
	}

	// A 有了新活动 → 顶到最前
	time.Sleep(3 * time.Millisecond)
	if err := st.TouchConversation(a.ID); err != nil {
		t.Fatal(err)
	}
	list, _ = st.ListConversations(1)
	if list[0].ID != a.ID {
		t.Fatalf("A 刚有活动，应当排到最前，实际第一个是 %d", list[0].ID)
	}
}

// 只换供应商不给模型时，model_id 要被清掉，避免出现「provider=甲 但 model 只存在于乙」。
func TestPatchProviderClearsStaleModel(t *testing.T) {
	st := newTestStore(t)

	a, _ := st.CreateProvider(1, "A", "https://a.example.com/v1", "k")
	b, _ := st.CreateProvider(1, "B", "https://b.example.com/v1", "k")
	_, _ = st.UpsertModels(a.ID, []Model{{ModelID: "only-in-a"}})
	_, _ = st.UpsertModels(b.ID, []Model{{ModelID: "only-in-b"}})

	conv, err := st.CreateConversation(1, "t", a.ID, "only-in-a", "medium")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := st.UpdateConversation(conv.ID, ConversationPatch{ProviderID: &b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ProviderID != b.ID {
		t.Fatalf("供应商没换过去: %d", updated.ProviderID)
	}
	if updated.ModelID != "" {
		t.Fatalf("换了供应商却没清掉旧模型，实际 %q", updated.ModelID)
	}
	// 再显式写上 B 的模型
	mid := "only-in-b"
	updated2, _ := st.UpdateConversation(conv.ID, ConversationPatch{ModelID: &mid})
	if updated2.ModelID != "only-in-b" || updated2.ProviderID != b.ID {
		t.Fatalf("写回模型失败: %+v", updated2)
	}
}
