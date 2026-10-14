package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRequireToken 覆盖令牌鉴权的所有通路与放行规则。
func TestRequireToken(t *testing.T) {
	hit := func() (*Server, *bool, http.Handler) {
		reached := false
		s := &Server{token: "s3cret-token-value"}
		h := s.requireToken(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		}))
		return s, &reached, h
	}

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
		{name: "cookie 正确", path: "/api/conversations", method: http.MethodGet, cookie: "s3cret-token-value", want: 200, reached: true},
		{name: "cookie 错误", path: "/api/conversations", method: http.MethodGet, cookie: "wrong", want: 401},
		{name: "Bearer 正确", path: "/api/conversations", method: http.MethodGet, auth: "Bearer s3cret-token-value", want: 200, reached: true},
		{name: "Bearer 前缀大小写不同", path: "/api/conversations", method: http.MethodGet, auth: "bearer s3cret-token-value", want: 401},
		{name: "X-Auth-Token 正确", path: "/api/conversations", method: http.MethodGet, header: "s3cret-token-value", want: 200, reached: true},
		{name: "查询参数正确", path: "/api/conversations", method: http.MethodGet, query: "token=s3cret-token-value", want: 200, reached: true},
		{name: "查询参数错误", path: "/api/conversations", method: http.MethodGet, query: "token=nope", want: 401},
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
		_, reached, h := hit()
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
		if *reached != c.reached {
			t.Errorf("%s: 是否到达处理函数 = %v，期望 %v", c.name, *reached, c.reached)
		}
		if c.want == 401 && !strings.Contains(rec.Body.String(), "unauthorized") {
			t.Errorf("%s: 401 响应体应带 unauthorized 错误码，实际 %s", c.name, rec.Body.String())
		}
	}
}

// TestTokenDisabledByDefault 令牌为空时必须完全不鉴权：默认行为不能被改变。
func TestTokenDisabledByDefault(t *testing.T) {
	s := &Server{}
	reached := false
	h := s.requireToken(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/conversations", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !reached {
		t.Fatalf("未设令牌时应当直接放行，实际 %d reached=%v", rec.Code, reached)
	}
}

// TestBootstrapToken 验证「URL 里的令牌换 cookie 并 302 抹掉」这条手机引导路径。
//
// 这条路径存在的理由：令牌如果只留在 URL 上，从主屏幕启动时走的是浏览器存的入口
// 或 manifest 的 start_url，query 会丢，于是每次启动都要重新输入。
// 换成长期 cookie 之后，加桌面、重启手机都还在。
func TestBootstrapToken(t *testing.T) {
	newHandler := func(token string) (*Server, http.Handler) {
		s := &Server{token: token}
		return s, s.bootstrapToken(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("SPA"))
		}))
	}

	t.Run("令牌正确则下发 cookie 并抹掉 query", func(t *testing.T) {
		_, h := newHandler("tok-abc")
		req := httptest.NewRequest(http.MethodGet, "/?token=tok-abc", nil)
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
		if c.Name != tokenCookie || c.Value != "tok-abc" {
			t.Errorf("cookie = %s=%s，期望 %s=tok-abc", c.Name, c.Value, tokenCookie)
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
		_, h := newHandler("tok-abc")
		req := httptest.NewRequest(http.MethodGet, "/some/path?token=tok-abc&keep=1", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if loc := rec.Header().Get("Location"); loc != "/some/path?keep=1" {
			t.Errorf("Location = %q，期望 /some/path?keep=1", loc)
		}
	})

	t.Run("令牌不对时不重定向，照常渲染页面", func(t *testing.T) {
		_, h := newHandler("tok-abc")
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

	t.Run("未启用鉴权时整条链都不介入", func(t *testing.T) {
		_, h := newHandler("")
		req := httptest.NewRequest(http.MethodGet, "/?token=whatever", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("未启用鉴权时不该重定向，状态码 %d", rec.Code)
		}
	})

	t.Run("API 路径不重定向，交给 requireToken 判", func(t *testing.T) {
		_, h := newHandler("tok-abc")
		req := httptest.NewRequest(http.MethodGet, "/api/conversations?token=tok-abc", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("API 请求不该被 302，状态码 %d", rec.Code)
		}
	})
}

func TestTokenMatches(t *testing.T) {
	s := &Server{token: "abcdef"}
	cases := []struct {
		v    string
		want bool
	}{
		{"abcdef", true},
		{" abcdef ", true}, // 容忍空白
		{"abcde", false},   // 长度不同
		{"abcdefg", false},
		{"", false},
		{"ABCDEF", false}, // 大小写敏感
		{strings.Repeat("a", tokenMaxLen+1), false},
	}
	for _, c := range cases {
		if got := s.tokenMatches(c.v); got != c.want {
			t.Errorf("tokenMatches(%q) = %v，期望 %v", c.v, got, c.want)
		}
	}
	// 未启用时任何输入都不算通过（避免 "" == "" 把鉴权放过去）
	empty := &Server{}
	if empty.tokenMatches("") || empty.tokenMatches("x") {
		t.Error("未设令牌时 tokenMatches 必须一律返回 false")
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
