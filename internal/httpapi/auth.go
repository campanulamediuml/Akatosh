package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

// 访问令牌的三条通路，按优先级：
//
//  1. Cookie（主路径）——**这是手机/PWA 能用的关键**。
//     令牌如果只放在 URL 里，从主屏幕启动时浏览器走的是 manifest 的 start_url
//     或是它自己存下来的入口，query 上的令牌会丢，于是每次启动都要求重新输入。
//     cookie 是按 origin 存的，重启手机、从图标启动都还在。
//  2. Authorization: Bearer / X-Auth-Token ——给 curl、脚本、以及禁用 cookie 的环境用。
//  3. ?token= ——只做两件事：首次引导（换 cookie 后 302 抹掉），
//     以及给 <img src> 这种加不了 header 的请求兜底。
//
// 注意 cookie **不能带 Secure**：局域网是明文 http，带 Secure 的 cookie 浏览器
// 根本不会存，手机这条路会直接断掉。这是“内网明文 + 长期 cookie”的自觉取舍，
// 所以令牌只应该在内网用，且不要和别的服务复用。
const (
	tokenCookie = "ai_token"
	tokenHeader = "X-Auth-Token"
	// 一年。手机上“添加到主屏幕”之后要长期有效，否则体验还不如不设。
	tokenCookieMaxAge = 365 * 24 * 3600
	// 令牌长度上限，防止有人拿超长字符串来撞 ConstantTimeCompare
	tokenMaxLen = 256
)

// GenerateToken 生成一个 32 字节的随机令牌（64 位十六进制）。
func GenerateToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败在正常系统上不可能发生；真发生了也不能退化成弱随机，
		// 宁可返回空串让调用方把鉴权当关闭处理并告警。
		return ""
	}
	return hex.EncodeToString(b)
}

// tokenEnabled 是否启用了鉴权。令牌为空 = 不鉴权（默认行为，保持向后兼容）。
func (s *Server) tokenEnabled() bool {
	return strings.TrimSpace(s.token) != ""
}

// tokenMatches 常量时间比较，避免用响应时间把令牌一位一位试出来。
func (s *Server) tokenMatches(v string) bool {
	want := strings.TrimSpace(s.token)
	v = strings.TrimSpace(v)
	if want == "" || v == "" {
		return false
	}
	if len(v) > tokenMaxLen || len(want) != len(v) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(v)) == 1
}

// tokenFromRequest 依次尝试 cookie / Authorization / X-Auth-Token / query。
func (s *Server) tokenFromRequest(r *http.Request) (string, bool) {
	if c, err := r.Cookie(tokenCookie); err == nil && s.tokenMatches(c.Value) {
		return c.Value, true
	}
	if h := r.Header.Get("Authorization"); h != "" {
		if v, ok := strings.CutPrefix(h, "Bearer "); ok && s.tokenMatches(strings.TrimSpace(v)) {
			return strings.TrimSpace(v), true
		}
	}
	if v := r.Header.Get(tokenHeader); s.tokenMatches(strings.TrimSpace(v)) {
		return strings.TrimSpace(v), true
	}
	if v := r.URL.Query().Get("token"); s.tokenMatches(v) {
		return v, true
	}
	return "", false
}

// requireToken 只守 /api/*。
//
// 静态资源必须放行，否则连“输入令牌”那个界面自己都加载不出来——页面是 JS 渲染的，
// 把 index.html 和 assets 也拦掉就成了死锁。
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.tokenEnabled() || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		// 探活接口保持公开：不泄露任何数据，但方便手机/脚本先确认“通不通”。
		// 鉴权失败和网络不通在手机上表现完全一样（都是转圈），有个公开探针好排查。
		if r.URL.Path == "/api/health" && r.Method == http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := s.tokenFromRequest(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusUnauthorized, "unauthorized", "需要访问令牌")
	})
}

// bootstrapToken 处理「拿 URL 里的令牌换一个长期 cookie」这个一次性动作。
//
// 手机第一次访问 http://<局域网IP>:28888/?token=xxxx 就走这里：
// 校验通过 → 下发 cookie → 302 到去掉 token 的同一个地址。
// 之后「添加到主屏幕」存下来的入口已经是干净地址，而从图标启动时 cookie 还在，
// 于是不会再出现“转成 app 之后令牌丢了”的问题。
func (s *Server) bootstrapToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 只管文档导航（非 /api/*）；API 请求带 ?token= 直接在 requireToken 里放行
		if !s.tokenEnabled() || r.Method != http.MethodGet || strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		raw := r.URL.Query().Get("token")
		if raw == "" || !s.tokenMatches(raw) {
			// 令牌不对也照常放行：页面会加载出来，API 会 401，
			// 前端据此弹出“输入访问令牌”，比白屏友好得多。
			next.ServeHTTP(w, r)
			return
		}
		s.setTokenCookie(w, raw)

		q := r.URL.Query()
		q.Del("token")
		target := r.URL.Path
		if enc := q.Encode(); enc != "" {
			target += "?" + enc
		}
		http.Redirect(w, r, target, http.StatusFound)
	})
}

// setTokenCookie 下发长期 cookie。
//
// SameSite=Strict：跨站请求一律不带，等于顺手把 CSRF 也堵了，
// 和 guard 里的 Origin 校验是两层独立的防线。
// HttpOnly：JS 读不到，少一条 XSS 泄露路径（前端不需要读它，走 header 那条路即可）。
func (s *Server) setTokenCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     tokenCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   tokenCookieMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}
