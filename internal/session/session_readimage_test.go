package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"aiassistant/internal/llm"
	"aiassistant/internal/media"
	"aiassistant/internal/store"
)

// TestReadAttachmentSendsImageUpstream 验证 read_attachment 循环：
// 模型要求回看 → 后端把图作为 tool 消息的多段 content（text + image_url）回传。
// 协议依据是实测：DeepSeek 接受 tool 消息带图。
func TestReadAttachmentSendsImageUpstream(t *testing.T) {
	var toolMsgHasImage bool
	round := 0
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
		round++
		w.Header().Set("Content-Type", "text/event-stream")
		if round == 1 {
			// 第一轮：要求读附件 #1（先用一个假 id，后面换成真 id 再发一次）
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_r\",\"type\":\"function\",\"function\":{\"name\":\"read_attachment\",\"arguments\":\"{\\\"id\\\": 1}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		// 第二轮：检查 tool 消息是不是带了图
		for _, m := range body.Messages {
			if m.Role != "tool" {
				continue
			}
			var parts []llm.ContentPart
			if err := json.Unmarshal(m.Content, &parts); err == nil {
				for _, p := range parts {
					if p.Type == "image_url" && p.ImageURL != nil {
						toolMsgHasImage = true
					}
				}
			}
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"图里是一只鲸鱼\"}}]}\n\n")
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

	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	saved, err := mediaStore.Save(bytes.NewReader(buf.Bytes()), "g.png")
	if err != nil {
		t.Fatal(err)
	}
	att, err := st.CreateAttachment(store.Attachment{
		Hash: saved.Hash, Kind: "image", MIME: saved.MIME, Ext: saved.Ext,
		Name: "g.png", Size: saved.Size, Width: saved.Width, Height: saved.Height,
	})
	if err != nil {
		t.Fatal(err)
	}

	pa, err := st.CreateProvider("p", up.URL+"/v1", "k")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.UpsertModels(pa.ID, []store.Model{{ModelID: "m", Reasoning: false, Efforts: []string{}}})

	mgr := NewManagerWithMedia(st, NewBus(), func(providerID int64) (*llm.Client, error) {
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

	conv, err := st.CreateConversation("t", pa.ID, "m", "")
	if err != nil {
		t.Fatal(err)
	}
	asst, err := st.CreateMessage(store.Message{ConvID: conv.ID, Role: store.RoleAssistant, Status: store.StatusDone, Content: "画好了"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AttachToMessage(asst.ID, []int64{att.ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.SendMessage(context.Background(), conv.ID, "回看一下刚画的图", 0, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, mgr, conv.ID)

	if !toolMsgHasImage {
		t.Fatal("tool 消息没带图（image_url 段）")
	}
}
