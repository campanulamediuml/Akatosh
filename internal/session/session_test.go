package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bytes"
	"image"
	"image/color"
	"image/png"

	"aiassistant/internal/llm"
	"aiassistant/internal/media"
	"aiassistant/internal/store"
)

func decodeBody(r *http.Request, dst any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(dst)
}

// newUpstream 起一个慢速的假上游，用于观察生成途中的状态。
func newUpstream(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		flush := func() {
			if f != nil {
				f.Flush()
			}
		}
		for i := 0; i < 8; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":%q}}]}\n\n", fmt.Sprintf("思考%d。", i))
			flush()
			time.Sleep(delay)
		}
		for i := 0; i < 4; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", fmt.Sprintf("正文%d。", i))
			flush()
			time.Sleep(delay)
		}
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30,\"completion_tokens_details\":{\"reasoning_tokens\":12}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush()
	})
	return httptest.NewServer(mux)
}

func newTestManager(t *testing.T, upstream string) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	p, err := st.CreateProvider(1, "test", upstream+"/v1", "k")
	if err != nil {
		t.Fatalf("保存供应商失败: %v", err)
	}
	if _, err := st.UpsertModels(p.ID, []store.Model{
		{ModelID: "m-reason", Reasoning: true, Efforts: []string{"low", "medium", "high"}},
		{ModelID: "m-plain", Reasoning: false, Efforts: []string{}},
	}); err != nil {
		t.Fatalf("写入模型失败: %v", err)
	}
	_ = st.SetSetting(1, store.SettingDefaultModel, "m-reason")

	mgr := NewManager(st, NewBus(), func(userID, providerID int64) (*llm.Client, error) {
		pp, err := st.GetProviderByID(providerID)
		if err != nil {
			return nil, err
		}
		return llm.New(pp.BaseURL, pp.APIKey), nil
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		mgr.Shutdown(ctx)
	})
	return mgr, st
}

func waitIdle(t *testing.T, mgr *Manager, convID int64) *State {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := mgr.State(context.Background(), 1, convID)
		if err != nil {
			t.Fatalf("取状态失败: %v", err)
		}
		if st.Generating == nil {
			return st
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("等待生成结束超时")
	return nil
}

// TestBusyDiscardsExtraMessage 验证「会话没等到回复就再发消息，这条要被丢弃」。
func TestBusyDiscardsExtraMessage(t *testing.T) {
	up := newUpstream(t, 25*time.Millisecond)
	defer up.Close()
	mgr, st := newTestManager(t, up.URL)

	conv, err := st.CreateConversation(1, "t", 0, "m-reason", "high")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if _, _, err := mgr.SendMessage(ctx, 1, conv.ID, "第一条", 0, "", nil, nil, nil); err != nil {
		t.Fatalf("第一条应当被接受: %v", err)
	}

	// 生成途中再发一条：必须被丢弃
	_, _, err = mgr.SendMessage(ctx, 1, conv.ID, "这条应被丢弃", 0, "", nil, nil, nil)
	var se *Error
	if !errors.As(err, &se) || se.Code != "busy" {
		t.Fatalf("期望 busy 错误，实际 %v", err)
	}

	fin := waitIdle(t, mgr, conv.ID)
	for _, m := range fin.Messages {
		if m.Content == "这条应被丢弃" {
			t.Fatal("被丢弃的消息竟然落库了")
		}
	}
	if len(fin.Messages) != 2 {
		t.Fatalf("期望 2 条消息（一问一答），实际 %d", len(fin.Messages))
	}
	if fin.Messages[1].Status != store.StatusDone {
		t.Fatalf("期望 done，实际 %s", fin.Messages[1].Status)
	}
	if fin.Messages[1].Usage.ReasoningTokens != 12 {
		t.Fatalf("用量没有落库: %+v", fin.Messages[1].Usage)
	}
	if fin.Messages[1].Reasoning == "" || fin.Messages[1].Content == "" {
		t.Fatal("CoT 或正文为空")
	}
	// state.conv 必须是库里的最新值，不能是会话缓存里的旧快照
	if fin.Conv == nil || fin.Conv.MessageCount != 2 {
		t.Fatalf("state.conv.message_count 应为 2，实际 %+v", fin.Conv)
	}
	if fin.Conv.Generating {
		t.Fatal("生成已结束，conv.Generating 应为 false")
	}
}

// TestStateReturnsLiveContent 验证生成途中 state 返回的是内存里的实时值，
// 而不是滞后 250ms 的落库副本 —— 这是「刷新页面不丢 CoT」的关键。
func TestStateReturnsLiveContent(t *testing.T) {
	up := newUpstream(t, 60*time.Millisecond)
	defer up.Close()
	mgr, st := newTestManager(t, up.URL)

	conv, err := st.CreateConversation(1, "t", 0, "m-reason", "high")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := mgr.SendMessage(ctx, 1, conv.ID, "你好", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	// 等到已经有内容吐出来，但还远没到第一次落库（250ms）
	time.Sleep(150 * time.Millisecond)

	live, err := mgr.State(ctx, 1, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if live.Generating == nil {
		t.Fatal("此刻应当正在生成")
	}
	var liveMsg *store.Message
	for i := range live.Messages {
		if live.Messages[i].ID == live.Generating.MessageID {
			liveMsg = &live.Messages[i]
		}
	}
	if liveMsg == nil {
		t.Fatal("快照里没有正在生成的那条消息")
	}
	if liveMsg.Reasoning == "" {
		t.Fatal("快照里的 CoT 是空的，说明没有补上内存实时值")
	}
	dbMsg, err := st.GetMessage(live.Generating.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(liveMsg.Reasoning) < len(dbMsg.Reasoning) {
		t.Fatalf("内存值(%d) 比落库值(%d) 还短", len(liveMsg.Reasoning), len(dbMsg.Reasoning))
	}
	t.Logf("内存 CoT=%d 字节，同期落库 CoT=%d 字节", len(liveMsg.Reasoning), len(dbMsg.Reasoning))

	waitIdle(t, mgr, conv.ID)
}

// TestShutdownFlushesInFlight 验证进程退出时会把还没落库的内容补上。
func TestShutdownFlushesInFlight(t *testing.T) {
	up := newUpstream(t, 60*time.Millisecond)
	defer up.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p, _ := st.CreateProvider(1, "test", up.URL+"/v1", "k")
	_, _ = st.UpsertModels(p.ID, []store.Model{{ModelID: "m-reason", Reasoning: true, Efforts: []string{"high"}}})
	mgr := NewManager(st, NewBus(), func(userID, providerID int64) (*llm.Client, error) {
		return llm.New(up.URL+"/v1", "k"), nil
	})

	conv, _ := st.CreateConversation(1, "t", 0, "m-reason", "high")
	ctx := context.Background()
	if _, _, err := mgr.SendMessage(ctx, 1, conv.ID, "你好", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // 有内容了，但还没到第一次落库

	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	mgr.Shutdown(sctx)

	msgs, err := st.ListMessages(conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	var asst *store.Message
	for i := range msgs {
		if msgs[i].Role == store.RoleAssistant {
			asst = &msgs[i]
		}
	}
	if asst == nil {
		t.Fatal("没有助手消息")
	}
	if asst.Status != store.StatusAborted {
		t.Fatalf("期望 aborted，实际 %s", asst.Status)
	}
	if asst.Reasoning == "" {
		t.Fatal("退出前没有把已生成的 CoT 落盘")
	}
	t.Logf("退出前落盘 CoT=%d 字节", len(asst.Reasoning))
}

// countingUpstream 是一个会数「收到几次流式请求」的假上游。
func countingUpstream(t *testing.T, counter *int32, delay time.Duration) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = decodeBody(r, &body)
		if !body.Stream { // 起标题之类的非流式请求不算
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{"content": "标题"}}},
			})
			return
		}
		atomic.AddInt32(counter, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		for i := 0; i < 6; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"片段%d\"}}]}\n\n", i)
			if f != nil {
				f.Flush()
			}
			time.Sleep(delay)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f != nil {
			f.Flush()
		}
	})
	return httptest.NewServer(mux)
}

// TestSwitchProviderMidStreamDoesNotReroute 锁住一个关键不变量：
// 生成途中切换供应商，**正在跑的这一轮绝不能被改道**；改动只对下一轮生效。
//
// 实现上是靠「发起生成时就把 client 和 provider/model 钉进 generation 结构」，
// 但这是必须靠测试守住的约定，不能只靠推理。
func TestSwitchProviderMidStreamDoesNotReroute(t *testing.T) {
	var aReqs, bReqs int32
	upA := countingUpstream(t, &aReqs, 80*time.Millisecond)
	defer upA.Close()
	upB := countingUpstream(t, &bReqs, 80*time.Millisecond)
	defer upB.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	pa, err := st.CreateProvider(1, "A", upA.URL+"/v1", "k")
	if err != nil {
		t.Fatal(err)
	}
	pb, err := st.CreateProvider(1, "B", upB.URL+"/v1", "k")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.UpsertModels(pa.ID, []store.Model{{ModelID: "model-a", Reasoning: true, Efforts: []string{"low"}}})
	_, _ = st.UpsertModels(pb.ID, []store.Model{{ModelID: "model-b", Reasoning: false, Efforts: []string{}}})

	mgr := NewManager(st, NewBus(), func(userID, providerID int64) (*llm.Client, error) {
		p, err := st.GetProviderByID(providerID)
		if err != nil {
			return nil, err
		}
		return llm.New(p.BaseURL, p.APIKey), nil
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		mgr.Shutdown(ctx)
	})

	conv, err := st.CreateConversation(1, "切供应商测试", pa.ID, "model-a", "low")
	if err != nil {
		t.Fatal(err)
	}
	if conv.ProviderID != pa.ID {
		t.Fatalf("会话应绑在 A 上，实际 %d", conv.ProviderID)
	}

	ctx := context.Background()
	if _, _, err := mgr.SendMessage(ctx, 1, conv.ID, "第一轮（走 A）", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	// 等它确实开始流式了
	time.Sleep(250 * time.Millisecond)
	if got := atomic.LoadInt32(&aReqs); got != 1 {
		t.Fatalf("A 应当已经收到 1 次流式请求，实际 %d", got)
	}

	// 生成途中把会话切到 B
	if _, err := mgr.PatchConversation(ctx, 1, conv.ID, store.ConversationPatch{ProviderID: &pb.ID}); err != nil {
		t.Fatalf("切换供应商失败: %v", err)
	}

	fin := waitIdle(t, mgr, conv.ID)

	// 这一轮必须仍然是 A 完成的
	if got := atomic.LoadInt32(&aReqs); got != 1 {
		t.Fatalf("A 应当只收到 1 次请求，实际 %d", got)
	}
	if got := atomic.LoadInt32(&bReqs); got != 0 {
		t.Fatalf("正在跑的这一轮被改道到 B 了（B 收到 %d 次请求）", got)
	}
	var asst *store.Message
	for i := range fin.Messages {
		if fin.Messages[i].Role == store.RoleAssistant {
			asst = &fin.Messages[i]
		}
	}
	if asst == nil {
		t.Fatal("没有助手消息")
	}
	if asst.ModelID != "model-a" {
		t.Fatalf("这条消息应当记 model-a（实际用的是谁就记谁），实际 %q", asst.ModelID)
	}
	if asst.Status != store.StatusDone {
		t.Fatalf("这一轮应当正常完成，实际 %s", asst.Status)
	}
	// 会话本身已经切到 B 了（下一轮生效）
	if fin.Conv.ProviderID != pb.ID {
		t.Fatalf("会话应当已经切到 B，实际 %d", fin.Conv.ProviderID)
	}

	// 下一轮必须走 B
	if _, _, err := mgr.SendMessage(ctx, 1, conv.ID, "第二轮（应当走 B）", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, mgr, conv.ID)
	if got := atomic.LoadInt32(&bReqs); got != 1 {
		t.Fatalf("下一轮应当走 B，B 收到 %d 次请求", got)
	}
	if got := atomic.LoadInt32(&aReqs); got != 1 {
		t.Fatalf("A 不该再收到请求，实际 %d", got)
	}
}

// TestSendWithImageReachesUpstreamAsVisionContent 验证图片真的以 vision
// 多段 content（text + image_url data URL）发到上游，而不是被当成普通文本。
func TestSendWithImageReachesUpstreamAsVisionContent(t *testing.T) {
	var gotImages int32
	var gotText atomic.Value
	gotText.Store("")

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = decodeBody(r, &body)
		if !body.Stream {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{"content": "标题"}}},
			})
			return
		}
		for _, m := range body.Messages {
			if m.Role != "user" {
				continue
			}
			var parts []llm.ContentPart
			if err := json.Unmarshal(m.Content, &parts); err != nil {
				t.Errorf("带图的用户消息 content 应当是数组，实际 %s", string(m.Content))
				continue
			}
			imgs := 0
			for _, p := range parts {
				switch p.Type {
				case "text":
					gotText.Store(p.Text)
				case "image_url":
					if p.ImageURL == nil || !strings.HasPrefix(p.ImageURL.URL, "data:image/") {
						t.Errorf("image_url 不是 data URL: %+v", p.ImageURL)
						continue
					}
					imgs++
				}
			}
			if imgs > 0 {
				atomic.StoreInt32(&gotImages, int32(imgs))
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"我看到图了\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	mediaStore, err := media.NewStore(filepath.Join(t.TempDir(), "misc"))
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 900, 700))
	for y := 0; y < 700; y++ {
		for x := 0; x < 900; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 90, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	saved, err := mediaStore.Save(bytes.NewReader(buf.Bytes()), "shot.png")
	if err != nil {
		t.Fatal(err)
	}
	att, err := st.CreateAttachment(store.Attachment{
		UserID: 1,
		Hash:   saved.Hash, Kind: "image", MIME: saved.MIME, Ext: saved.Ext,
		Name: "shot.png", Size: saved.Size, Width: saved.Width, Height: saved.Height,
	})
	if err != nil {
		t.Fatal(err)
	}

	pa, err := st.CreateProvider(1, "vision", up.URL+"/v1", "k")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.UpsertModels(pa.ID, []store.Model{{ModelID: "m-vision", Reasoning: false, Efforts: []string{}}})

	mgr := NewManagerWithMedia(st, NewBus(), func(userID, providerID int64) (*llm.Client, error) {
		p, err := st.GetProviderByID(providerID)
		if err != nil {
			return nil, err
		}
		return llm.New(p.BaseURL, p.APIKey), nil
	}, mediaStore)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		mgr.Shutdown(ctx)
	})

	conv, err := st.CreateConversation(1, "带图测试", pa.ID, "m-vision", "medium")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := mgr.SendMessage(ctx, 1, conv.ID, "这张图里是什么？", 0, "", nil, []int64{att.ID}, nil); err != nil {
		t.Fatal(err)
	}
	fin := waitIdle(t, mgr, conv.ID)

	if n := atomic.LoadInt32(&gotImages); n != 1 {
		t.Fatalf("上游应当收到 1 张图，实际 %d", n)
	}
	if got := gotText.Load().(string); got != "这张图里是什么？" {
		t.Fatalf("文本段不对: %q", got)
	}
	var userMsg *store.Message
	for i := range fin.Messages {
		if fin.Messages[i].Role == store.RoleUser {
			userMsg = &fin.Messages[i]
		}
	}
	if userMsg == nil || len(userMsg.Attachments) != 1 {
		t.Fatalf("用户消息应当带 1 个附件，实际 %+v", userMsg)
	}
	if userMsg.Attachments[0].ID != att.ID {
		t.Fatalf("附件 id 不对: %d", userMsg.Attachments[0].ID)
	}
}

// TestSendWithOnlyImageIsAllowed 验证「只发图不打字」。
func TestSendWithOnlyImageIsAllowed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = decodeBody(r, &body)
		if !body.Stream {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{"content": "标题"}}},
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"嗯\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	st, _ := store.Open(filepath.Join(t.TempDir(), "test.db"))
	defer st.Close()
	mediaStore, _ := media.NewStore(filepath.Join(t.TempDir(), "misc"))

	img := image.NewRGBA(image.Rect(0, 0, 60, 40))
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	saved, _ := mediaStore.Save(bytes.NewReader(buf.Bytes()), "tiny.png")
	att, _ := st.CreateAttachment(store.Attachment{
		UserID: 1,
		Hash:   saved.Hash, Kind: "image", MIME: saved.MIME, Ext: saved.Ext,
		Name: "tiny.png", Size: saved.Size, Width: saved.Width, Height: saved.Height,
	})

	pa, _ := st.CreateProvider(1, "p", up.URL+"/v1", "k")
	_, _ = st.UpsertModels(pa.ID, []store.Model{{ModelID: "m", Reasoning: false}})
	mgr := NewManagerWithMedia(st, NewBus(), func(userID, providerID int64) (*llm.Client, error) {
		return llm.New(up.URL+"/v1", "k"), nil
	}, mediaStore)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		mgr.Shutdown(ctx)
	})

	conv, _ := st.CreateConversation(1, "只发图", pa.ID, "m", "medium")
	ctx := context.Background()
	if _, _, err := mgr.SendMessage(ctx, 1, conv.ID, "", 0, "", nil, []int64{att.ID}, nil); err != nil {
		t.Fatalf("只发图应当被允许，实际报错: %v", err)
	}
	fin := waitIdle(t, mgr, conv.ID)
	var userMsg *store.Message
	for i := range fin.Messages {
		if fin.Messages[i].Role == store.RoleUser {
			userMsg = &fin.Messages[i]
		}
	}
	if userMsg == nil || len(userMsg.Attachments) != 1 {
		t.Fatalf("应当有一条只带图的用户消息，实际 %+v", userMsg)
	}

	// 内容和图都给空则应当被拒
	if _, _, err := mgr.SendMessage(ctx, 1, conv.ID, "   ", 0, "", nil, nil, nil); err == nil {
		t.Fatal("内容和图都为空时应当报错")
	}
}

// 侧边栏在「回答落地」那一刻把会话顶到最上面，靠的就是会话的 updated_at。
// 这条锁住时机：不是「发出去了」就更新就算完，而是「答完了」还要再更新一次。
func TestFinishBumpsConversationTime(t *testing.T) {
	up := newUpstream(t, 40*time.Millisecond)
	defer up.Close()
	mgr, st := newTestManager(t, up.URL)

	conv, err := st.CreateConversation(1, "置顶时机", 0, "m-reason", "high")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := mgr.SendMessage(ctx, 1, conv.ID, "第一轮", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	// 刚发出去（还在生成）时的时间戳
	sent, err := st.GetConversation(conv.ID)
	if err != nil {
		t.Fatal(err)
	}

	fin := waitIdle(t, mgr, conv.ID)

	if fin.Conv.UpdatedAt <= sent.UpdatedAt {
		t.Fatalf("回答落地后 updated_at 应当变大（发出时=%d，答完=%d）——"+
			"侧边栏按它倒序排，不更新的话会话不会冒到最上面",
			sent.UpdatedAt, fin.Conv.UpdatedAt)
	}
}

// TestNoEffortForNonReasoningModel 验证不支持思考的模型不会下发 reasoning_effort。
func TestNoEffortForNonReasoningModel(t *testing.T) {
	up := newUpstream(t, 5*time.Millisecond)
	defer up.Close()
	mgr, st := newTestManager(t, up.URL)

	conv, _ := st.CreateConversation(1, "t", 0, "m-plain", "high")
	ctx := context.Background()
	if _, _, err := mgr.SendMessage(ctx, 1, conv.ID, "你好", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	fin := waitIdle(t, mgr, conv.ID)
	for _, m := range fin.Messages {
		if m.Role == store.RoleAssistant && m.Effort != "" {
			t.Fatalf("不支持思考的模型不应带 effort，实际 %q", m.Effort)
		}
	}
}

// TestEditChangesHistory 验证编辑后的正文会进入下一轮历史。
func TestEditChangesHistory(t *testing.T) {
	var lastUser string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = decodeBody(r, &body)
		for _, m := range body.Messages {
			if m.Role == "user" {
				lastUser = m.Content
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	mgr, st := newTestManager(t, up.URL)
	conv, _ := st.CreateConversation(1, "t", 0, "m-reason", "high")
	ctx := context.Background()

	if _, _, err := mgr.SendMessage(ctx, 1, conv.ID, "原始提问", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	fin := waitIdle(t, mgr, conv.ID)

	var userMsgID int64
	for _, m := range fin.Messages {
		if m.Role == store.RoleUser {
			userMsgID = m.ID
		}
	}
	if _, err := mgr.EditMessage(ctx, 1, userMsgID, "改过的提问"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.SendMessage(ctx, 1, conv.ID, "接着聊", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, mgr, conv.ID)

	if lastUser != "接着聊" {
		t.Fatalf("最后一轮的用户消息应当是「接着聊」，实际 %q", lastUser)
	}
	msgs, _ := st.ListMessages(conv.ID)
	var edited *store.Message
	for i := range msgs {
		if msgs[i].ID == userMsgID {
			edited = &msgs[i]
		}
	}
	if edited == nil || edited.Content != "改过的提问" || !edited.Edited || edited.ContentOriginal != "原始提问" {
		t.Fatalf("编辑状态不对: %+v", edited)
	}
}

// assistant 消息挂图（AI 生成的图）不能发给上游：DeepSeek 会 400
// 「Image in assistant message is not supported」。buildHistory 只让用户消息带图。
func TestAssistantImageNotSentUpstream(t *testing.T) {
	var assistantContentKinds []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = decodeBody(r, &body)
		if !body.Stream {
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "标题"}}}})
			return
		}
		for _, m := range body.Messages {
			if m.Role != "assistant" {
				continue
			}
			var s string
			if err := json.Unmarshal(m.Content, &s); err == nil {
				assistantContentKinds = append(assistantContentKinds, "string")
			} else {
				assistantContentKinds = append(assistantContentKinds, "array")
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mediaStore, err := media.NewStore(filepath.Join(t.TempDir(), "misc"))
	if err != nil {
		t.Fatal(err)
	}

	// 造一张图，挂到一条历史 assistant 消息上（模拟「生图」功能留下的历史）
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	saved, err := mediaStore.Save(bytes.NewReader(buf.Bytes()), "gen.png")
	if err != nil {
		t.Fatal(err)
	}
	att, err := st.CreateAttachment(store.Attachment{
		UserID: 1,
		Hash:   saved.Hash, Kind: "image", MIME: saved.MIME, Ext: saved.Ext,
		Name: "gen.png", Size: saved.Size, Width: saved.Width, Height: saved.Height,
	})
	if err != nil {
		t.Fatal(err)
	}

	pa, err := st.CreateProvider(1, "p", up.URL+"/v1", "k")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.UpsertModels(pa.ID, []store.Model{{ModelID: "m", Reasoning: false, Efforts: []string{}}})

	mgr := NewManagerWithMedia(st, NewBus(), func(userID, providerID int64) (*llm.Client, error) {
		p, err := st.GetProviderByID(providerID)
		if err != nil {
			return nil, err
		}
		return llm.New(p.BaseURL, p.APIKey), nil
	}, mediaStore)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		mgr.Shutdown(ctx)
	})

	conv, err := st.CreateConversation(1, "t", pa.ID, "m", "")
	if err != nil {
		t.Fatal(err)
	}
	// 历史：一条用户消息 + 一条带图的 assistant 消息
	if _, err := st.CreateMessage(store.Message{ConvID: conv.ID, Role: store.RoleUser, Status: store.StatusDone, Content: "画个图"}); err != nil {
		t.Fatal(err)
	}
	asst, err := st.CreateMessage(store.Message{ConvID: conv.ID, Role: store.RoleAssistant, Status: store.StatusDone, Content: "画好了"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AttachToMessage(asst.ID, []int64{att.ID}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := mgr.SendMessage(context.Background(), 1, conv.ID, "再来一张", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, mgr, conv.ID)

	if len(assistantContentKinds) == 0 {
		t.Fatal("上游没收到 assistant 历史消息")
	}
	for _, k := range assistantContentKinds {
		if k != "string" {
			t.Fatalf("assistant 消息的 content 应是纯文本（不带图），实际是数组")
		}
	}
}
