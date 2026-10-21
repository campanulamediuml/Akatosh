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
	// 只用来打启动告警 —— **不再用它放宽 Host 校验**，见 hostAllowed。
	exposed bool

	// allowHosts 是额外放行的主机名（非 IP 字面量）。
	// 正常用 IP 访问不需要它；用机器名 / MagicDNS 名字访问时才要配。
	allowHosts []string

	// lastSeenMu/lastSeen 是 last_seen_at 的节流表：
	// 每个用户每分钟最多写一次库，免得每个 API 请求都 UPDATE users。
	lastSeenMu sync.Mutex
	lastSeen   map[int64]int64
}

// Option 用来扩展 Server 配置。
// 用变参而不是加必填形参，是为了不破坏既有调用点（含测试）。
type Option func(*Server)

// WithAllowHosts 设置额外放行的主机名白名单（逗号分隔或直接给切片）。
func WithAllowHosts(hosts []string) Option {
	return func(s *Server) { s.allowHosts = hosts }
}

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
	// 顺序：panic 兜底 → gzip → Host/Origin 校验 → 令牌引导 → 令牌鉴权 → 路由。
	// 把 guard 放最前面：rebinding 请求连令牌都不用看就直接拒。
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

// guard 挡两种东西：DNS rebinding 和跨源请求。
//
// 为什么本地服务也需要这个：浏览器里的 JS 能访问 127.0.0.1，防火墙和「只绑回环」
// 拦的都是外部主机。攻击链是——你打开 evil.com:28888（攻击者的服务器），页面里的 JS
// 在攻击者把 evil.com 的 DNS 改指向 127.0.0.1 之后请求 /api/conversations，于是这个
// 请求落到你本机的服务上；而浏览器认为页面和请求同源（都是 evil.com:28888），
// 不会有任何 CORS 拦截，JS 能把你的对话和 CoT 全读走。
//
// 拦截点选在 Host 而不是 Origin：**同源 GET 请求浏览器不发 Origin**，只查 Origin
// 挡不住最主要的读数据路径。而 Host 一定存在，且正常访问它只会是 IP 字面量或
// localhost，DNS rebinding 却必然带一个攻击者域名过来。
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			writeErr(w, http.StatusForbidden, "forbidden",
				"已拒绝该 Host 的请求（疑似 DNS rebinding）。请用 127.0.0.1 或局域网 IP 访问；"+
					"确实要用主机名，请启动时加 -allow-host。")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !originAllowed(o, r.Host) {
			writeErr(w, http.StatusForbidden, "forbidden", "已拒绝跨源请求")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed 判断请求的 Host 是否可以接受。
//
// 这里曾经有个洞：只要用户绑了非回环地址（手机访问必须这么做），就整体返回 true，
// 等于把 DNS rebinding 防线整个关掉了。实测 Host=evil.com:28888 配
// Origin=http://evil.com:28888 能拿到 200 —— 而“绑 0.0.0.0”正是用手机的唯一方式。
//
// 现在的规则与绑定地址无关，一律：
//   - IP 字面量 / localhost        → 放行（手机用局域网 IP 访问走的就是这条）
//   - 在 -allow-host 白名单里      → 放行（用机器名 / MagicDNS 名字访问时）
//   - 其它（必然是某个域名）        → 拒绝
//
// 域名访问被拒不会影响手机：启动日志里打的就是 IP 形式的地址。
func (s *Server) hostAllowed(host string) bool {
	if hostIsLiteral(host) {
		return true
	}
	return s.hostInAllowList(host)
}

// hostInAllowList 匹配 -allow-host 里的主机名。
// 支持精确匹配（my-pc、my-pc:28888）和前缀通配（*.local、*.ts.net）。
// 端口一律忽略后再比：用户写 my-pc，实际 Host 是 my-pc:28888，不该要求他写端口。
func (s *Server) hostInAllowList(host string) bool {
	if len(s.allowHosts) == 0 {
		return false
	}
	full := strings.ToLower(strings.TrimSpace(host))
	name := full
	if h, _, err := net.SplitHostPort(full); err == nil {
		name = h
	}
	name = strings.Trim(name, "[]")
	if name == "" {
		return false
	}
	for _, pat := range s.allowHosts {
		p := strings.ToLower(strings.TrimSpace(pat))
		if p == "" {
			continue
		}
		if p == full || p == name {
			return true
		}
		if suffix, ok := strings.CutPrefix(p, "*"); ok && strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// hostIsLiteral 判断 Host 是不是 IP 字面量或 localhost。
// 用 127.0.0.1 / ::1 / localhost 访问都合法；带域名的请求一律拒绝，
// 因为正常使用不会出现域名，而 DNS rebinding 必须有域名才能成立。
func hostIsLiteral(host string) bool {
	h := host
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		h = hostOnly
	}
	h = strings.Trim(strings.TrimSpace(h), "[]")
	if h == "" {
		return false
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	return net.ParseIP(h) != nil
}

// bindIsExposed 判断监听地址是不是「主动对外」。
// 只用于启动时打告警，不再影响 Host 校验（那曾是个洞，见 hostAllowed）。
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
