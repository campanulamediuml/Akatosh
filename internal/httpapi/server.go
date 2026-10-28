package httpapi

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"aiassistant/internal/llm"
	"aiassistant/internal/media"
	"aiassistant/internal/session"
	"aiassistant/internal/store"
)

type Server struct {
	st      *store.Store
	mgr     *session.Manager
	web     fs.FS
	version string
	mux     *http.ServeMux
	// media 是 misc 目录句柄（图片上传/读取）；可以为 nil
	media *media.Store
	// filesDir 是上传文档的存放目录（misc/files）；空 = 不启用文件上传
	filesDir string

	// exposed 为 true 表示用户把服务绑到了非回环地址（主动对外提供访问）。
	exposed bool

	// lastSeenMu/lastSeen 是 last_seen_at 的节流表：
	// 每个用户每分钟最多写一次库，免得每个 API 请求都 UPDATE users。
	lastSeenMu sync.Mutex
	lastSeen   map[int64]int64
}

// Option 用来扩展 Server 配置。
// 用变参而不是加必填形参，是为了不破坏既有调用点（含测试）。
type Option func(*Server)

// WithFilesDir 设置上传文档的存放目录（misc/files）。
func WithFilesDir(dir string) Option {
	return func(s *Server) { s.filesDir = dir }
}

func New(st *store.Store, mgr *session.Manager, web fs.FS, version, bindAddr string, mediaStore *media.Store, opts ...Option) *Server {
	s := &Server{
		st:       st,
		mgr:      mgr,
		web:      web,
		version:  version,
		media:    mediaStore,
		exposed:  bindIsExposed(bindAddr),
		lastSeen: map[int64]int64{},
	}
	for _, o := range opts {
		if o != nil {
			o(s)
		}
	}
	s.routes()
	return s
}

// Exposed 报告服务是否绑到了非回环地址（main 用来打告警）。
func (s *Server) Exposed() bool { return s.exposed }

// touchUserThrottled 刷新用户的 last_seen_at，每个用户每分钟最多写一次。
func (s *Server) touchUserThrottled(userID int64) {
	now := time.Now().UnixMilli()
	s.lastSeenMu.Lock()
	if now-s.lastSeen[userID] < 60_000 {
		s.lastSeenMu.Unlock()
		return
	}
	s.lastSeen[userID] = now
	s.lastSeenMu.Unlock()
	s.st.TouchUser(userID)
}

func (s *Server) routes() {
	m := http.NewServeMux()

	m.HandleFunc("GET /api/health", s.handleHealth)

	// 全局唯一的下行通道
	m.HandleFunc("GET /api/stream", s.handleStream)

	// 配置 / 供应商 / 模型
	m.HandleFunc("GET /api/settings", s.handleGetSettings)
	m.HandleFunc("PUT /api/settings/defaults", s.handlePutDefaults)

	m.HandleFunc("GET /api/providers", s.handleListProviders)
	m.HandleFunc("POST /api/providers", s.handleCreateProvider)
	m.HandleFunc("PATCH /api/providers/{id}", s.handlePatchProvider)
	m.HandleFunc("DELETE /api/providers/{id}", s.handleDeleteProvider)
	m.HandleFunc("POST /api/providers/{id}/models/fetch", s.handleFetchModels)
	m.HandleFunc("POST /api/providers/{id}/test", s.handleTestProvider)
	m.HandleFunc("GET /api/models", s.handleListModels)

	// 会话内全文搜索（当前用户隔离）
	m.HandleFunc("GET /api/search/messages", s.handleSearchMessages)

	// 会话
	m.HandleFunc("GET /api/conversations", s.handleListConversations)
	m.HandleFunc("POST /api/conversations", s.handleCreateConversation)
	m.HandleFunc("PATCH /api/conversations/{id}", s.handlePatchConversation)
	m.HandleFunc("DELETE /api/conversations/{id}", s.handleDeleteConversation)
	m.HandleFunc("GET /api/conversations/{id}/state", s.handleState)
	m.HandleFunc("POST /api/conversations/{id}/read", s.handleMarkRead)

	// 命令（返回 ack，内容一律走 SSE）
	m.HandleFunc("POST /api/conversations/{id}/messages", s.handleSendMessage)
	m.HandleFunc("POST /api/conversations/{id}/stop", s.handleStop)
	m.HandleFunc("POST /api/messages/{id}/regenerate", s.handleRegenerate)
	m.HandleFunc("PATCH /api/messages/{id}", s.handleEditMessage)
	m.HandleFunc("POST /api/messages/{id}/restore", s.handleRestoreMessage)
	m.HandleFunc("POST /api/messages/{id}/restore-reasoning", s.handleRestoreMessageReasoning)
	m.HandleFunc("POST /api/conversations/{id}/fork", s.handleForkConversation)
	m.HandleFunc("POST /api/conversations/{id}/compact", s.handleCompactConversation)
	m.HandleFunc("GET /api/conversations/{id}/messages", s.handleListMessages)
	m.HandleFunc("DELETE /api/messages/{id}", s.handleDeleteMessage)

	// 当前用户 / 用户管理（管理员）
	m.HandleFunc("GET /api/me", s.handleMe)
	m.HandleFunc("GET /api/users", s.handleListUsers)
	m.HandleFunc("POST /api/users", s.handleCreateUser)
	m.HandleFunc("POST /api/users/{id}/token", s.handleRotateUserToken)
	m.HandleFunc("DELETE /api/users/{id}", s.handleDeleteUser)

	// 附件（图片）
	m.HandleFunc("POST /api/uploads", s.handleUpload)
	m.HandleFunc("POST /api/files", s.handleUploadFile)
	m.HandleFunc("GET /api/attachments/{id}/thumb", s.handleAttachmentThumb)
	m.HandleFunc("GET /api/attachments/{id}/raw", s.handleAttachmentRaw)

	// 未匹配的 /api/* 返回 JSON 404，而不是掉进 SPA 回退里返回 index.html
	m.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "接口不存在: "+r.Method+" "+r.URL.Path)
	})
	m.HandleFunc("/", s.handleSPA)

	s.mux = m
}

func (s *Server) Handler() http.Handler {
	// 顺序：panic 兜底 → gzip → Origin 校验 → 令牌引导 → 令牌鉴权 → 路由。
	return recoverer(gzipMiddleware(s.guard(s.bootstrapToken(s.requireToken(s.mux)))))
}

// gzipMiddleware 对支持 gzip 的客户端压缩响应。
// 长会话的 state 响应是大头（CoT 全量，454KB 级；文本压缩率高，通常能压到 1/4 以下）。
// 跳过两类：/api/stream（SSE 长连接有自己的节奏，不能缓冲）；
// /api/attachments（JPEG/PNG 已是压缩格式，再压纯浪费 CPU）。
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") ||
			strings.HasPrefix(r.URL.Path, "/api/stream") ||
			strings.HasPrefix(r.URL.Path, "/api/attachments") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, w: gz}, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	w io.Writer
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) { return g.w.Write(p) }

// ---------- 中间件 ----------

// guard 拒绝跨源请求。Host 不限制，以便通过任意 NAT 域名访问。
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && !originAllowed(o, r.Host) {
			writeErr(w, http.StatusForbidden, "forbidden", "已拒绝跨源请求")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bindIsExposed 判断监听地址是不是「主动对外」。
func bindIsExposed(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" {
		return true // ":28888" 等价于监听全部地址
	}
	// localhost 也是回环：net.Listen 会把它解到 127.0.0.1 / ::1
	if strings.EqualFold(host, "localhost") {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true // 绑的是主机名（用户就是想用这个名字访问），判断不了，放宽
	}
	return !ip.IsLoopback()
}

func originAllowed(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	// 本机任意端口都放行：Vite 开发服务器（5173）走代理转发时会带上自己的 Origin。
	// 攻击场景里的 Origin 是攻击者的域名，不会命中 loopback 判断。
	if isLoopbackHost(u.Hostname()) {
		return true
	}
	return strings.EqualFold(u.Host, host)
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[http] panic %s %s: %v", r.Method, r.URL.Path, rec)
				writeErr(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------- 静态资源 ----------

// handleSPA 提供嵌入的前端产物，并对未知路径回退到 index.html。
func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	if s.web == nil {
		http.NotFound(w, r)
		return
	}
	p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if p == "" {
		p = "index.html"
	}
	if f, err := s.web.Open(p); err == nil {
		f.Close()
		switch {
		case p == "index.html":
			// 不缓存入口，重新构建后刷新就能看到新版
			w.Header().Set("Cache-Control", "no-cache")
		case p == "sw.js":
			// Service Worker 的更新检查直接拉这个文件，长缓存会让新版迟迟不生效
			w.Header().Set("Cache-Control", "no-cache")
		case strings.HasSuffix(p, ".webmanifest"):
			// 精简容器里没有 /etc/mime.types，Go 内置表不认 .webmanifest，
			// 不设会被当 text/plain（浏览器虽宽容，但按规范给）
			w.Header().Set("Content-Type", "application/manifest+json")
			w.Header().Set("Cache-Control", "no-cache")
		case strings.HasPrefix(p, "assets/"):
			// vite 产物是 hash 命名（内容变则文件名变），可以放心长缓存
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		http.FileServer(http.FS(s.web)).ServeHTTP(w, r)
		return
	}
	index, err := fs.ReadFile(s.web, "index.html")
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "前端资源未构建（缺少 web/dist/index.html）")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(index)
}

// ---------- 通用助手 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[http] 写响应失败: %v", err)
	}
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": msg},
	})
}

// writeDomainErr 把会话层返回的错误码映射成 HTTP 状态码。
func writeDomainErr(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return // 客户端已经断开，没必要回写
	}
	var se *session.Error
	if errors.As(err, &se) {
		status := http.StatusInternalServerError
		switch se.Code {
		case "busy":
			status = http.StatusConflict
		case "not_found":
			status = http.StatusNotFound
		case "bad_request", "no_model", "no_provider":
			status = http.StatusBadRequest
		}
		writeErr(w, status, se.Code, se.Msg)
		return
	}
	writeErr(w, http.StatusInternalServerError, "internal", err.Error())
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return true // 允许空 body
		}
		writeErr(w, http.StatusBadRequest, "bad_request", "请求体解析失败: "+err.Error())
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || n <= 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "非法的 "+name)
		return 0, false
	}
	return n, true
}

// clientFor 按供应商构造上游客户端（工具/诊断接口用；多租户：按用户隔离）。
// providerID <= 0 时回退到该用户的默认供应商。
func (s *Server) clientFor(userID, providerID int64) (*llm.Client, error) {
	p, err := s.st.ResolveProvider(userID, providerID)
	if err != nil {
		return nil, err
	}
	if p == nil || strings.TrimSpace(p.BaseURL) == "" {
		return nil, session.NewError("no_provider", "还没有配置任何供应商，请先在设置里添加一个")
	}
	return llm.New(p.BaseURL, p.APIKey), nil
}

// maskKey 只显示头尾，避免明文 key 出现在任何响应里。
func maskKey(k string) string {
	if k == "" {
		return ""
	}
	r := []rune(k)
	if len(r) <= 10 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:5]) + "****" + string(r[len(r)-4:])
}
