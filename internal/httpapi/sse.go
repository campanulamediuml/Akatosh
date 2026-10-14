package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// sseWriter 负责把事件按 SSE 线格式写给浏览器。
// data 用 JSON 编码，天然规避「SSE 的 data 字段不能包含裸换行」这个坑。
type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func newSSEWriter(w http.ResponseWriter) (*sseWriter, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("当前服务器不支持流式响应")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// 万一前面挂了 nginx 之类的反代，这行能阻止它缓冲整个响应
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &sseWriter{w: w, f: f}, nil
}

// Event 写一个命名事件并立刻 flush。
// 不 flush 的话，内容会攒在 Go 的写缓冲里，用户看到的就是「一大段一大段」而不是逐字。
func (s *sseWriter) Event(name string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	s.f.Flush()
	return nil
}

// Comment 写一个 SSE 注释（心跳），客户端会忽略它，但能让中间层知道连接还活着。
func (s *sseWriter) Comment(text string) {
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return
	}
	s.f.Flush()
}
