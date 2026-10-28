package session

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aiassistant/internal/llm"
	"aiassistant/internal/store"
)

// compactUpstream 是一个只服务摘要请求（非流式）的假上游：
// 每次 Complete 返回 "摘要第N次"，并记下收到的输入。
func compactUpstream(t *testing.T, inputs *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = decodeBody(r, &body)
		calls++
		if mu != nil {
			mu.Lock()
			*inputs = append(*inputs, body.Messages[0].Content)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": fmt.Sprintf("摘要第%d次", calls)}}},
			"usage":   map[string]any{"total_tokens": 123},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func compactManager(t *testing.T, st *store.Store, upURL string) *Manager {
	t.Helper()
	return NewManager(st, NewBus(), func(userID, providerID int64) (*llm.Client, error) {
		return llm.New(upURL+"/v1", "k"), nil
	}).WithCompactThreshold(0.5)
}

func seedConvWithUpstream(t *testing.T, st *store.Store, upURL string, n int, contentLen int) *store.Conversation {
	t.Helper()
	p, err := st.CreateProvider(1, "p", upURL+"/v1", "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModels(p.ID, []store.Model{{ModelID: "m", Reasoning: true, Efforts: []string{"high"}, ContextLen: 10000}}); err != nil {
		t.Fatal(err)
	}
	conv, err := st.CreateConversation(1, "t", p.ID, "m", "")
	if err != nil {
		t.Fatal(err)
	}
	addMessages(t, st, conv.ID, n, contentLen)
	return conv
}

func addMessages(t *testing.T, st *store.Store, convID int64, n, contentLen int) {
	t.Helper()
	content := strings.Repeat("字", contentLen)
	msgs, _ := st.ListMessages(convID)
	base := len(msgs)
	for i := 0; i < n; i++ {
		role := store.RoleUser
		if (base+i)%2 == 1 {
			role = store.RoleAssistant
		}
		if _, err := st.CreateMessage(store.Message{ConvID: convID, Role: role, Status: store.StatusDone, Content: content}); err != nil {
			t.Fatal(err)
		}
	}
}

// 多轮压缩：第二轮只压增量（compacted_before 之后的消息），
// 旧摘要滚动合并进新摘要，冻结边界随之前进。
func TestRunCompactMultiRound(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var mu sync.Mutex
	var inputs []string
	up := compactUpstream(t, &inputs, &mu)
	conv := seedConvWithUpstream(t, st, up.URL, 24, 10)

	mgr := compactManager(t, st, up.URL)
	s, err := mgr.getOrCreate(conv.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 第一轮：old = 前 14 条（24-10），边界 = msgs[13]
	s.runCompact()
	msgs, _ := st.ListMessages(conv.ID)
	conv1, _ := st.GetConversation(conv.ID)
	if conv1.Summary != "摘要第1次" {
		t.Fatalf("第一轮摘要不对: %q", conv1.Summary)
	}
	if conv1.CompactedBefore != msgs[13].ID {
		t.Fatalf("第一轮边界应为 msgs[13].ID=%d，实际 %d", msgs[13].ID, conv1.CompactedBefore)
	}
	if conv1.ContextEstimate <= 0 {
		t.Fatalf("压缩后应保存上下文估算值，实际 %d", conv1.ContextEstimate)
	}

	// 再聊 16 条（共 40 条），第二轮：fresh = (msgs[13], msgs[29]]
	addMessages(t, st, conv.ID, 16, 10)
	s.runCompact()
	msgs, _ = st.ListMessages(conv.ID)
	conv2, _ := st.GetConversation(conv.ID)
	if conv2.Summary != "摘要第2次" {
		t.Fatalf("第二轮摘要不对: %q", conv2.Summary)
	}
	if conv2.CompactedBefore != msgs[29].ID {
		t.Fatalf("第二轮边界应为 msgs[29].ID=%d，实际 %d", msgs[29].ID, conv2.CompactedBefore)
	}

	// 第二轮的摘要输入：带旧摘要（滚动合并），且只含增量消息
	mu.Lock()
	defer mu.Unlock()
	if len(inputs) != 2 {
		t.Fatalf("应有 2 次摘要请求，实际 %d", len(inputs))
	}
	if !strings.Contains(inputs[1], "【之前的摘要】") || !strings.Contains(inputs[1], "摘要第1次") {
		t.Fatalf("第二轮输入应滚动合并旧摘要")
	}
}

// 输入超上限截断时：冻结边界只覆盖**实际进摘要**的消息，
// 没进摘要的消息不能被冻结（否则内容既不在摘要也不在上下文，静默丢失）。
func TestRunCompactTruncationCoverage(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	up := compactUpstream(t, nil, nil)
	// 24 条 × 5000 字：old = 前 14 条（70k 字），超过 60k 输入上限
	conv := seedConvWithUpstream(t, st, up.URL, 24, 5000)

	mgr := compactManager(t, st, up.URL)
	s, err := mgr.getOrCreate(conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.runCompact()

	msgs, _ := st.ListMessages(conv.ID)
	conv1, _ := st.GetConversation(conv.ID)
	// 每条约 5007 runes：11 条 ≈ 55k（放得下），第 12 条会超 60k → 边界 = msgs[10]
	if conv1.CompactedBefore != msgs[10].ID {
		t.Fatalf("截断时边界应只覆盖实际进摘要的 msgs[10].ID=%d，实际 %d", msgs[10].ID, conv1.CompactedBefore)
	}
	if conv1.CompactedBefore >= msgs[13].ID {
		t.Fatalf("没进摘要的消息（至 msgs[13]）不应被冻结")
	}
	if conv1.Summary == "" {
		t.Fatal("截断也应产出摘要（覆盖已包含的部分）")
	}
}

func TestRunCompactUsesConversationEffort(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	efforts := make(chan string, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ReasoningEffort string `json:"reasoning_effort"`
		}
		_ = decodeBody(r, &body)
		efforts <- body.ReasoningEffort
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "摘要"}}},
		})
	}))
	t.Cleanup(up.Close)

	conv := seedConvWithUpstream(t, st, up.URL, 14, 10)
	effort := "high"
	if _, err := st.UpdateConversation(conv.ID, store.ConversationPatch{Effort: &effort}); err != nil {
		t.Fatal(err)
	}
	mgr := compactManager(t, st, up.URL)
	s, err := mgr.getOrCreate(conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.runCompact()

	select {
	case got := <-efforts:
		if got != effort {
			t.Fatalf("压缩请求应沿用会话思考强度 %q，实际 %q", effort, got)
		}
	default:
		t.Fatal("压缩请求没有发出")
	}
}

func TestCompactContextEstimateLifecycle(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	up := compactUpstream(t, nil, nil)
	conv := seedConvWithUpstream(t, st, up.URL, 24, 100)
	msgs, _ := st.ListMessages(conv.ID)
	last := msgs[len(msgs)-1]
	if err := st.FinishMessage(last.ID, last.Status, last.Reasoning, last.Content, last.Error,
		store.Usage{PromptTokens: 10000}, last.SearchLog, last.ToolRounds); err != nil {
		t.Fatal(err)
	}

	mgr := compactManager(t, st, up.URL)
	s, err := mgr.getOrCreate(conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.runCompact()

	compacted, _ := st.GetConversation(conv.ID)
	if compacted.ContextEstimate <= 0 || compacted.ContextEstimate >= 10000 {
		t.Fatalf("压缩后估算值应大于 0 且小于锚点 10000，实际 %d", compacted.ContextEstimate)
	}

	if err := st.FinishMessage(last.ID, last.Status, last.Reasoning, last.Content, last.Error,
		store.Usage{PromptTokens: 123}, last.SearchLog, last.ToolRounds); err != nil {
		t.Fatal(err)
	}
	updated, _ := st.GetConversation(conv.ID)
	if updated.ContextEstimate != 0 {
		t.Fatalf("拿到下一次真实 usage 后估算值应清零，实际 %d", updated.ContextEstimate)
	}
}
