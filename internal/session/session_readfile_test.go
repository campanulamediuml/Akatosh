package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aiassistant/internal/llm"
	"aiassistant/internal/media"
	"aiassistant/internal/sandbox"
	"aiassistant/internal/store"
)

func TestParseReadFileArgs(t *testing.T) {
	args, err := parseReadFileArgs(`{"id":7,"pages":[1,3],"images":true}`)
	if err != nil || args.ID != 7 || !args.Images || len(args.Pages) != 2 || args.Pages[1] != 3 {
		t.Fatalf("图片读取参数解析不对: %+v err=%v", args, err)
	}
	legacy, err := parseReadFileArgs(`{"id":9}`)
	if err != nil || legacy.ID != 9 || legacy.Images || len(legacy.Pages) != 0 {
		t.Fatalf("旧参数不兼容: %+v err=%v", legacy, err)
	}
	invalid := []string{
		`{"id":7,"images":true}`,
		`{"id":7,"pages":[0],"images":true}`,
		`{"id":7,"pages":[1,1],"images":true}`,
		`{"id":7,"pages":[1,2,3,4,5,6],"images":true}`,
	}
	for _, raw := range invalid {
		if _, err := parseReadFileArgs(raw); err == nil {
			t.Errorf("无效参数应被拒绝: %s", raw)
		}
	}
}

func TestFileExtractScriptPDFPagesAndScanHint(t *testing.T) {
	script := fileExtractScript("/files/scan.pdf", []int{1, 3})
	for _, want := range []string{"selected_pages = [1,3]", "PDF 共", "images=true", "此页未提取到文本"} {
		if !strings.Contains(script, want) {
			t.Errorf("PDF 提取脚本缺少 %q", want)
		}
	}
}

func TestReadPDFPagesSendsImagesUpstream(t *testing.T) {
	var gotImages atomic.Int32
	var renderCalls atomic.Int32
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
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "扫描件"}}}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if renderCalls.Load() == 0 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_pdf\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"id\\\":1,\\\"pages\\\":[1,3],\\\"images\\\":true}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		for _, m := range body.Messages {
			if m.Role != "tool" {
				continue
			}
			var parts []llm.ContentPart
			if err := json.Unmarshal(m.Content, &parts); err != nil {
				t.Errorf("PDF 工具回执应是多段 content: %v", err)
				continue
			}
			for _, p := range parts {
				if p.Type == "image_url" && p.ImageURL != nil && strings.HasPrefix(p.ImageURL.URL, "data:image/jpeg;base64,") {
					gotImages.Add(1)
				}
			}
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"我看到了第 1、3 页\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(up.Close)

	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/render-pdf" {
			t.Errorf("不应调用文本提取接口: %s", r.URL.Path)
		}
		var req struct {
			Path  string `json:"path"`
			Pages []int  `json:"pages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Path != "/files/scan-hash.pdf" || len(req.Pages) != 2 || req.Pages[0] != 1 || req.Pages[1] != 3 {
			t.Errorf("渲染参数不对: %+v", req)
		}
		renderCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"total_pages":3,"images":[{"page":1,"mime":"image/jpeg","data":"aGVsbG8="},{"page":3,"mime":"image/jpeg","data":"d29ybGQ="}]}`)
	}))
	t.Cleanup(box.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mediaStore, err := media.NewStore(filepath.Join(t.TempDir(), "misc"))
	if err != nil {
		t.Fatal(err)
	}
	provider, err := st.CreateProvider(1, "p", up.URL+"/v1", "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModels(provider.ID, []store.Model{{ModelID: "deepseek-v4.1-flash", Reasoning: false, Efforts: []string{}}}); err != nil {
		t.Fatal(err)
	}
	mgr := NewManagerWithMedia(st, NewBus(), func(userID, providerID int64) (*llm.Client, error) {
		p, err := st.GetProviderByID(providerID)
		if err != nil {
			return nil, err
		}
		return llm.New(p.BaseURL, p.APIKey), nil
	}, mediaStore).WithSandbox(sandbox.New(box.URL))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		mgr.Shutdown(ctx)
	})
	conv, err := st.CreateConversation(1, "扫描件", provider.ID, "deepseek-v4.1-flash", "")
	if err != nil {
		t.Fatal(err)
	}
	file, err := st.CreateFile(store.File{UserID: 1, Hash: "scan-hash", Name: "scan.pdf", Ext: ".pdf", Size: 100})
	if err != nil {
		t.Fatal(err)
	}
	user, err := st.CreateMessage(store.Message{ConvID: conv.ID, Role: store.RoleUser, Status: store.StatusDone, Content: "附件"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AttachFileToMessage(user.ID, []int64{file.ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.SendMessage(context.Background(), 1, conv.ID, "看第 1、3 页", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	state := waitIdle(t, mgr, conv.ID)
	if renderCalls.Load() != 1 || gotImages.Load() != 2 {
		t.Fatalf("PDF 图片没有完整回传: render=%d images=%d", renderCalls.Load(), gotImages.Load())
	}
	last := state.Messages[len(state.Messages)-1]
	if !strings.Contains(last.SearchLog, `"images":true`) || !strings.Contains(last.SearchLog, `"pages":[1,3]`) {
		t.Fatalf("工具记录缺少图片模式或页码: %s", last.SearchLog)
	}
}
