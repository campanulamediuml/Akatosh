package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestBindIsExposed(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		// 默认配置：只绑回环
		{"127.0.0.1:28888", false},
		{"localhost:28888", false},
		{"[::1]:28888", false},
		// 用户主动对外开放
		{"0.0.0.0:28888", true},
		{":28888", true},
		{"192.168.1.10:28888", true},
		{"[::]:28888", true},
	}
	for _, c := range cases {
		if got := bindIsExposed(c.addr); got != c.want {
			t.Errorf("bindIsExposed(%q) = %v，期望 %v", c.addr, got, c.want)
		}
	}
}

func TestOriginAllowed(t *testing.T) {
	cases := []struct {
		origin string
		host   string
		want   bool
	}{
		// 自己访问自己
		{"http://127.0.0.1:28888", "127.0.0.1:28888", true},
		{"http://localhost:28888", "localhost:28888", true},
		// Vite 开发服务器走代理转发，Origin 是本机的另一个端口
		{"http://localhost:5173", "127.0.0.1:28888", true},
		// 攻击者的页面
		{"https://evil.com", "127.0.0.1:28888", false},
		{"http://evil.com:28888", "evil.com:28888", true},
		{"", "127.0.0.1:28888", false},
	}
	for _, c := range cases {
		if got := originAllowed(c.origin, c.host); got != c.want {
			t.Errorf("originAllowed(%q, %q) = %v，期望 %v", c.origin, c.host, got, c.want)
		}
	}
}

func TestGuardAllowsAnyHostAndBlocksCrossOrigin(t *testing.T) {
	s := &Server{}
	called := false
	h := s.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name    string
		host    string
		origin  string
		want    string
		reached bool
	}{
		{"本机 IP", "127.0.0.1:28888", "http://127.0.0.1:28888", "200", true},
		{"任意 NAT 域名", "nat.example.com", "", "200", true},
		{"任意域名同源 Origin", "nat.example.com", "https://nat.example.com", "200", true},
		{"跨源读取", "nat.example.com", "https://evil.com", "403", false},
	}
	for _, c := range cases {
		called = false
		req := httptest.NewRequest(http.MethodGet, "http://"+c.host+"/api/conversations", nil)
		req.Host = c.host
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := strconv.Itoa(rec.Code); got != c.want {
			t.Errorf("%s: 状态码 %s，期望 %s", c.name, got, c.want)
		}
		if called != c.reached {
			t.Errorf("%s: 是否到达处理函数 = %v，期望 %v", c.name, called, c.reached)
		}
	}
}
