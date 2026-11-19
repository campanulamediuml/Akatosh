package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"aiassistant/internal/modelcheck"
	"aiassistant/internal/store"
)

func TestModelHealthAPIAndAdminToggle(t *testing.T) {
	const token = "health-admin"
	s := newAuthTestServer(t, token)
	s.modelHealth = modelcheck.New(s.st)
	s.routes()
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	request := func(method, path string, body []byte) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Auth-Token", token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := request(http.MethodGet, "/api/model-health", nil)
	var initial struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&initial); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || initial.Enabled {
		t.Fatalf("默认应关闭: status=%d body=%+v", resp.StatusCode, initial)
	}

	resp = request(http.MethodPut, "/api/users/1/model-health", []byte(`{"enabled":true}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("管理员开启失败: %d", resp.StatusCode)
	}
	if value, _ := s.st.GetSetting(1, store.SettingModelHealthEnabled); value != "1" {
		t.Fatalf("开关没有按用户持久化: %q", value)
	}

	resp = request(http.MethodGet, "/api/users", nil)
	var users []store.User
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(users) != 1 || !users[0].ModelHealthEnabled {
		t.Fatalf("用户列表没有返回检测开关: %+v", users)
	}

	resp = request(http.MethodGet, "/api/model-health", nil)
	var current struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&current); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !current.Enabled {
		t.Fatal("当前用户读取不到已开启状态")
	}

	resp = request(http.MethodGet, "/api/model-health/concurrency", nil)
	var config struct {
		Concurrency int `json:"concurrency"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || config.Concurrency != 16 {
		t.Fatalf("默认全局并发不对: status=%d body=%+v", resp.StatusCode, config)
	}
	resp = request(http.MethodPut, "/api/model-health/concurrency", []byte(`{"concurrency":24}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || s.modelHealth.Concurrency() != 24 {
		t.Fatalf("管理员设置全局并发失败: status=%d concurrency=%d", resp.StatusCode, s.modelHealth.Concurrency())
	}
	if value, _ := s.st.GetSetting(0, store.SettingModelHealthConcurrency); value != "24" {
		t.Fatalf("全局并发没有持久化: %q", value)
	}
}

func TestModelHealthToggleUserPermissions(t *testing.T) {
	s := newAuthTestServer(t, "admin-token")
	guest, err := s.st.CreateUser("guest", store.HashToken("guest-token"), false)
	if err != nil {
		t.Fatal(err)
	}
	s.modelHealth = modelcheck.New(s.st)
	s.routes()
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	request := func(path string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut, srv.URL+path, bytes.NewBufferString(`{"enabled":true}`))
		req.Header.Set("X-Auth-Token", "guest-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := request("/api/users/1/model-health")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("普通用户不应修改他人的开关: %d", resp.StatusCode)
	}

	resp = request(fmt.Sprintf("/api/users/%d/model-health", guest.ID))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("普通用户应能修改自己的开关: %d", resp.StatusCode)
	}
	if value, _ := s.st.GetSetting(guest.ID, store.SettingModelHealthEnabled); value != "1" {
		t.Fatalf("自己的开关没有持久化: %q", value)
	}

	resp = request("/api/model-health/concurrency")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("普通用户不应修改全局并发: %d", resp.StatusCode)
	}
}
