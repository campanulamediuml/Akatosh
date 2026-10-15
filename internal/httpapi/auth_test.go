package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"aiassistant/internal/store"
)

// contextWithUser 把用户注入请求上下文（等价于 requireToken 做的事）。
func contextWithUser(r *http.Request, u *store.User) context.Context {
	return context.WithValue(r.Context(), ctxKeyUser{}, u)
}

// newAuthTestServer 造一个带真实 store + 一个测试用户的 Server。
// 多租户：令牌存在 users 表（哈希），一个令牌一个用户。
func newAuthTestServer(t *testing.T, token string) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if token != "" {
		if _, err := st.CreateUser("tester", store.HashToken(token), true); err != nil {
			t.Fatal(err)
		}
	}
	return &Server{st: st, lastSeen: map[int64]int64{}}
}

// TestRequireToken 覆盖令牌鉴权的所有通路与放行规则。
func TestRequireToken(t *testing.T) {
	const token = "s3cret-token-value"

	cases := []struct {
		name    string
		path    string
		method  string
		cookie  string
		header  string
		auth    string
		query   string
		want    int
		reached bool
	}{
		{name: "没有任何凭证", path: "/api/conversations", method: http.MethodGet, want: 401},
		{name: "cookie 正确", path: "/api/conversations", method: http.MethodGet, cookie: token, want: 200, reached: true},
		{name: "cookie 错误", path: "/api/conversations", method: http.MethodGet, cookie: "wrong", want: 401},
		{name: "Bearer 正确", path: "/api/conversations", method: http.MethodGet, auth: "Bearer " + token, want: 200, reached: true},
		{name: "Bearer 前缀大小写不同", path: "/api/conversations", method: http.MethodGet, auth: "bearer " + token, want: 401},
		{name: "X-Auth-Token 正确", path: "/api/conversations", method: http.MethodGet, header: token, want: 200, reached: true},
		{name: "查询参数正确", path: "/api/conversations", method: http.MethodGet, query: "token=" + token, want: 200, reached: true},
		{name: "查询参数错误", path: "/api/conversations", method: http.MethodGet, query: "token=nope", want: 401},
		{name: "超长令牌直接拒", path: "/api/conversations", method: http.MethodGet, header: strings.Repeat("a", tokenMaxLen+1), want: 401},
		// 静态资源必须放行，否则「输入令牌」那个界面自己就加载不出来（页面是 JS 渲染的）
		{name: "入口页面不守", path: "/", method: http.MethodGet, want: 200, reached: true},
		{name: "前端资源不守", path: "/assets/index-abc123.js", method: http.MethodGet, want: 200, reached: true},
		// 公开探针：不泄露数据，但方便手机先确认「通不通」
		{name: "health 公开", path: "/api/health", method: http.MethodGet, want: 200, reached: true},
		{name: "非 GET 的 health 仍要令牌", path: "/api/health", method: http.MethodPost, want: 401},
		// 前缀判断不能误伤：/apifoo 不是 /api/ 下的接口
		{name: "非 api 前缀不守", path: "/apifoo", method: http.MethodGet, want: 200, reached: true},
	}

	for _, c := range cases {
		s := newAuthTestServer(t, token)
		reached := false
		h := s.requireToken(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached = true
			// 走鉴权的路径（/api/* 且非公开 health）必须把用户注入上下文；
			// 静态资源和公开探针本来就没有用户
			needUser := strings.HasPrefix(r.URL.Path, "/api/") &&
				!(r.URL.Path == "/api/health" && r.Method == http.MethodGet)
			if needUser && userOf(r) == nil {
				t.Errorf("%s: 鉴权通过但上下文里没有用户", c.name)
			}
			w.WriteHeader(http.StatusOK)
		}))
		target := c.path
		if c.query != "" {
			target += "?" + c.query
		}
		req := httptest.NewRequest(c.method, target, nil)
		if c.cookie != "" {
			req.AddCookie(&http.Cookie{Name: tokenCookie, Value: c.cookie})
		}
		if c.header != "" {
			req.Header.Set(tokenHeader, c.header)
		}
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: 状态码 %d，期望 %d", c.name, rec.Code, c.want)
		}
		if reached != c.reached {
			t.Errorf("%s: 是否到达处理函数 = %v，期望 %v", c.name, reached, c.reached)
		}
		if c.want == 401 && !strings.Contains(rec.Body.String(), "unauthorized") {
			t.Errorf("%s: 401 响应体应带 unauthorized 错误码，实际 %s", c.name, rec.Body.String())
		}
	}
}

// TestBootstrapToken 验证「URL 里的令牌换 cookie 并 302 抹掉」这条手机引导路径。
//
// 这条路径存在的理由：令牌如果只留在 URL 上，从主屏幕启动时走的是浏览器存的入口
// 或 manifest 的 start_url，query 会丢，于是每次启动都要重新输入。
// 换成长期 cookie 之后，加桌面、重启手机都还在。
func TestBootstrapToken(t *testing.T) {
	const token = "tok-abc"

	newHandler := func(tok string) (*Server, http.Handler) {
		s := newAuthTestServer(t, tok)
		return s, s.bootstrapToken(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("SPA"))
		}))
	}

	t.Run("令牌正确则下发 cookie 并抹掉 query", func(t *testing.T) {
		_, h := newHandler(token)
		req := httptest.NewRequest(http.MethodGet, "/?token="+token, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusFound {
			t.Fatalf("状态码 %d，期望 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/" {
			t.Errorf("Location = %q，期望 %q（必须把令牌从地址里去掉）", loc, "/")
		}
		cookies := rec.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatalf("应下发 1 个 cookie，实际 %d 个", len(cookies))
		}
		c := cookies[0]
		if c.Name != tokenCookie || c.Value != token {
			t.Errorf("cookie = %s=%s，期望 %s=%s", c.Name, c.Value, tokenCookie, token)
		}
		if c.MaxAge <= 0 {
			t.Errorf("cookie 必须持久化（MaxAge > 0），否则重启手机就丢，实际 %d", c.MaxAge)
		}
		if !c.HttpOnly {
			t.Error("cookie 应为 HttpOnly")
		}
		if c.SameSite != http.SameSiteStrictMode {
			t.Errorf("SameSite = %v，期望 Strict（顺带堵住 CSRF）", c.SameSite)
		}
		if c.Secure {
			t.Error("cookie 不能带 Secure：局域网是明文 http，带了浏览器不会存，手机这条路会直接断")
		}
	})

	t.Run("保留其它查询参数", func(t *testing.T) {
		_, h := newHandler(token)
		req := httptest.NewRequest(http.MethodGet, "/some/path?token="+token+"&keep=1", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if loc := rec.Header().Get("Location"); loc != "/some/path?keep=1" {
			t.Errorf("Location = %q，期望 /some/path?keep=1", loc)
		}
	})

	t.Run("令牌不对时不重定向，照常渲染页面", func(t *testing.T) {
		_, h := newHandler(token)
		req := httptest.NewRequest(http.MethodGet, "/?token=wrong", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 %d，期望 200（不白屏，让前端去弹「输入令牌」）", rec.Code)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Error("令牌不对时不该下发 cookie")
		}
		if rec.Body.String() != "SPA" {
			t.Errorf("应继续走后面的处理链，实际 body = %q", rec.Body.String())
		}
	})

	t.Run("API 路径不重定向，交给 requireToken 判", func(t *testing.T) {
		_, h := newHandler(token)
		req := httptest.NewRequest(http.MethodGet, "/api/conversations?token="+token, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("API 请求不该被 302，状态码 %d", rec.Code)
		}
	})
}

// TestResolveUser 令牌 → 用户解析的边界。
func TestResolveUser(t *testing.T) {
	const token = "abcdef"
	s := newAuthTestServer(t, token)

	if u := s.resolveUser(token); u == nil || u.Name != "tester" || !u.IsAdmin {
		t.Errorf("正确令牌应解析到 tester 管理员，实际 %+v", u)
	}
	if u := s.resolveUser(" abcdef "); u == nil {
		t.Error("应容忍令牌两侧的空白")
	}
	for _, bad := range []string{"", "abcde", "abcdefg", "ABCDEF", strings.Repeat("a", tokenMaxLen+1)} {
		if u := s.resolveUser(bad); u != nil {
			t.Errorf("resolveUser(%q) 应为 nil，实际 %+v", bad, u)
		}
	}
}

// TestRequireAdmin 用户管理接口必须只有管理员能进。
func TestRequireAdmin(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	admin, _ := st.CreateUser("boss", store.HashToken("admin-tok"), true)
	normal, _ := st.CreateUser("guest", store.HashToken("guest-tok"), false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
	req = req.WithContext(contextWithUser(req, admin))
	if !requireAdmin(rec, req) {
		t.Error("管理员应通过 requireAdmin")
	}
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/users", nil)
	req2 = req2.WithContext(contextWithUser(req2, normal))
	if requireAdmin(rec2, req2) {
		t.Error("普通用户不应通过 requireAdmin")
	}
	if rec2.Code != http.StatusForbidden {
		t.Errorf("普通用户应得到 403，实际 %d", rec2.Code)
	}
}

func TestGenerateToken(t *testing.T) {
	a, b := GenerateToken(), GenerateToken()
	if len(a) != 64 {
		t.Errorf("令牌长度 %d，期望 64 位十六进制", len(a))
	}
	if a == b {
		t.Error("两次生成的令牌不应相同")
	}
	for _, c := range a {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("令牌含非十六进制字符: %q", c)
		}
	}
}
