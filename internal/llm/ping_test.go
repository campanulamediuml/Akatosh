package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPingSendsMinimalChineseProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages  []ChatMessage `json:"messages"`
			MaxTokens int           `json:"max_tokens"`
			Stream    bool          `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Messages) != 1 || body.Messages[0].Content != "测试" || body.MaxTokens != 1 || body.Stream {
			t.Fatalf("检测请求不对: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	t.Cleanup(srv.Close)
	if _, err := New(srv.URL, "").Ping(context.Background(), "test-model"); err != nil {
		t.Fatal(err)
	}
}
