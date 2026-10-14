package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestHostIsLiteral(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1:28888", true},
		{"127.0.0.1", true},
		{"localhost:28888", true},
		{"LOCALHOST:28888", true},
		{"[::1]:28888", true},
		{"192.168.1.10:28888", true},
		// DNS rebinding 必然长这样：一个域名
		{"evil.com:28888", false},
		{"evil.com", false},
		{"localhost.evil.com:28888", false},
		{"127.0.0.1.evil.com:28888", false},
		{"", false},
		{"   ", false},
	}
	for _, c := range cases {
		if got := hostIsLiteral(c.host); got != c.want {
			t.Errorf("hostIsLiteral(%q) = %v，期望 %v", c.host, got, c.want)
		}
	}
}

func TestBindIsExposed(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		// 默认配置：只绑回环 → 不对外 → 启用 Host 校验
		{"127.0.0.1:28888", false},
		{"localhost:28888", false},
		{"[::1]:28888", false},
		// 用户主动对外开放 → 放宽 Host 校验
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
		{"http://evil.com:28888", "evil.com:28888", true}, // 同源，交给 Host 校验去挡
		{"", "127.0.0.1:28888", false},
	}
	for _, c := range cases {
		if got := originAllowed(c.origin, c.host); got != c.want {
			t.Errorf("originAllowed(%q, %q) = %v，期望 %v", c.origin, c.host, got, c.want)
		}
	}
}

// TestGuardBlocksRebinding 端到端验证中间件：
// 带攻击者域名的 Host 必须被拒，正常访问必须放行。
func TestGuardBlocksRebinding(t *testing.T) {
	s := &Server{exposed: false}
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
		// 正常访问：放行
		{"本机 IP 访问", "127.0.0.1:28888", "http://127.0.0.1:28888", "200", true},
		{"localhost 访问", "localhost:28888", "", "200", true},
		// DNS rebinding：Host 是攻击者域名 → 拒
		{"rebinding GET（无 Origin）", "evil.com:28888", "", "403", false},
		{"rebinding POST", "evil.com:28888", "http://evil.com:28888", "403", false},
		// 跨源：Host 正常但 Origin 是外站 → 拒
		{"跨源读取", "127.0.0.1:28888", "https://evil.com", "403", false},
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

// TestGuardStillBlocksRebindingWhenExposed 是回归测试。
//
// 曾经的实现：只要绑了非回环地址就整体跳过 Host 校验 —— 那正是用手机访问的唯一方式。
// 实测 Host=evil.com:28888 + Origin=http://evil.com:28888 能拿到 200，rebinding 完全可打。
// 现在 Host 校验与绑定地址无关，只看「是不是 IP 字面量 / 在白名单里」。
func TestGuardStillBlocksRebindingWhenExposed(t *testing.T) {
	s := &Server{exposed: true}
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
		// 手机走的就是这两条
		{"局域网 IP", "192.168.1.10:28888", "", "200", true},
		{"本机回环", "127.0.0.1:28888", "", "200", true},
		// DNS rebinding：页面与 Host 同为攻击者域名，Origin 校验拦不住，必须靠 Host 拦
		{"rebinding 经典打法", "evil.com:28888", "http://evil.com:28888", "403", false},
		{"rebinding 无 Origin", "evil.com:28888", "", "403", false},
		// 机器名未加白名单 → 也拒（要放行请用 -allow-host）
		{"机器名未加白名单", "my-pc:28888", "", "403", false},
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

// TestHostInAllowList 白名单匹配：支持精确、带端口、前缀通配，端口一律忽略。
func TestHostInAllowList(t *testing.T) {
	s := &Server{allowHosts: []string{"my-pc", "*.local", "*.ts.net", " "}}
	cases := []struct {
		host string
		want bool
	}{
		// 精确：写 my-pc，实际 Host 带端口，不该要求用户写端口
		{"my-pc", true},
		{"my-pc:28888", true},
		{"MY-PC:28888", true},
		// 通配
		{"nas.local", true},
		{"nas.local:28888", true},
		{"pc.tail1234.ts.net", true},
		// 不该命中
		{"evil-my-pc.com", false},
		{"my-pc.evil.com", false},
		{"notlocal", false},
		// 通配不匹配空后缀（"*" 只当后缀用，不做全放行）
		{".local", true},
		{"", false},
	}
	for _, c := range cases {
		if got := s.hostInAllowList(c.host); got != c.want {
			t.Errorf("hostInAllowList(%q) = %v，期望 %v", c.host, got, c.want)
		}
	}

	// 空白名单 → 一律不通过
	none := &Server{}
	if none.hostInAllowList("my-pc") {
		t.Error("没有白名单时不该放行任何主机名")
	}
}

// TestHostAllowedWithAllowList 走完整判断：白名单里的主机名要能过，其它域名仍拒。
func TestHostAllowedWithAllowList(t *testing.T) {
	s := &Server{exposed: true, allowHosts: []string{"my-pc", "*.local"}}
	yes := []string{"127.0.0.1:28888", "192.168.1.10:28888", "my-pc:28888", "nas.local:28888"}
	no := []string{"evil.com:28888", "evil.local.evil.com", "localhost.evil.com:28888"}
	for _, h := range yes {
		if !s.hostAllowed(h) {
			t.Errorf("hostAllowed(%q) = false，期望 true", h)
		}
	}
	for _, h := range no {
		if s.hostAllowed(h) {
			t.Errorf("hostAllowed(%q) = true，期望 false", h)
		}
	}
}
