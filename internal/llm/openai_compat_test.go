package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestImageContentMissingTextCompatibilityRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content []map[string]json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if len(body.Messages) != 1 || len(body.Messages[0].Content) != 2 {
			t.Errorf("请求内容块不对: %+v", body.Messages)
			return
		}
		image := body.Messages[0].Content[1]
		if image["type"] == nil || image["image_url"] == nil {
			t.Errorf("图片块丢失: %+v", image)
			return
		}
		_, hasText := image["text"]
		if calls.Add(1) == 1 {
			if hasText {
				t.Error("首次请求应使用标准 OpenAI 图片格式，不应带 text")
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"error":{"message":"messages[0]: missing field `+"`text`"+`"}}`)
			return
		}
		if !hasText {
			t.Error("兼容重试的图片块应带 text")
		}
		for _, part := range body.Messages[0].Content {
			if _, ok := part["text"]; !ok {
				t.Errorf("兼容重试的每个内容块都应有 text: %+v", part)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	cli := New(srv.URL+"/v1", "k")
	req := ChatRequest{Model: "vision", Messages: []ChatMessage{{
		Role:    "user",
		Content: BuildContent([]ContentPart{TextPart("看图"), ImagePart("data:image/png;base64,eA==")}),
	}}}
	ch, _, err := cli.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	events := collect(ch)
	if len(events) == 0 || events[0].Type != EventContent || events[0].Delta != "ok" {
		t.Fatalf("兼容重试没有返回正文: %+v", events)
	}
	if calls.Load() != 2 {
		t.Fatalf("应只在明确的 422 后重试一次，实际请求 %d 次", calls.Load())
	}
}

func TestUnrelated422DoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprint(w, `{"error":{"message":"other validation error"}}`)
	}))
	t.Cleanup(srv.Close)
	cli := New(srv.URL+"/v1", "k")
	_, _, err := cli.Stream(context.Background(), ChatRequest{Model: "m", Messages: []ChatMessage{{
		Role:    "user",
		Content: BuildContent([]ContentPart{TextPart("x"), ImagePart("data:image/png;base64,eA==")}),
	}}})
	if err == nil {
		t.Fatal("无关 422 不应被吞掉")
	}
	if calls.Load() != 1 {
		t.Fatalf("无关 422 不应重试，实际请求 %d 次", calls.Load())
	}
}
