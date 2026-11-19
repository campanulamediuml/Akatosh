package modelcheck

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"aiassistant/internal/store"
)

type fakeStore struct {
	mu        sync.RWMutex
	users     []store.User
	settings  map[int64]string
	providers map[int64][]store.Provider
	models    map[int64][]store.Model
}

func (f *fakeStore) ListUsers() ([]store.User, error) { return f.users, nil }
func (f *fakeStore) GetSetting(userID int64, _ string) (string, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.settings[userID], nil
}
func (f *fakeStore) SetSetting(userID int64, _ string, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings[userID] = value
	return nil
}
func (f *fakeStore) ListProviders(userID int64) ([]store.Provider, error) {
	return f.providers[userID], nil
}
func (f *fakeStore) ListModels(_ int64, providerID int64) ([]store.Model, error) {
	return f.models[providerID], nil
}

type pingRecorder struct {
	mu    sync.Mutex
	calls []string
}

type fakePinger struct{ recorder *pingRecorder }

func (p fakePinger) Ping(_ context.Context, model string) (time.Duration, error) {
	p.recorder.mu.Lock()
	p.recorder.calls = append(p.recorder.calls, model)
	p.recorder.mu.Unlock()
	if model == "bad" {
		return 0, errors.New("upstream down")
	}
	return 25 * time.Millisecond, nil
}

func TestCheckAllIsolatesUsersAndRecordsEveryModel(t *testing.T) {
	st := &fakeStore{
		users:    []store.User{{ID: 1}, {ID: 2}},
		settings: map[int64]string{1: "1", 2: "0"},
		providers: map[int64][]store.Provider{
			1: {{ID: 11, BaseURL: "https://a.example"}, {ID: 12, BaseURL: "https://b.example"}},
			2: {{ID: 21, BaseURL: "https://other.example"}},
		},
		models: map[int64][]store.Model{
			11: {{ModelID: "good"}, {ModelID: "bad"}},
			12: {{ModelID: "good-2"}},
			21: {{ModelID: "other-user-model"}},
		},
	}
	manager := New(st)
	recorder := &pingRecorder{}
	manager.newClient = func(_, _ string) pinger { return fakePinger{recorder: recorder} }
	manager.checkAll(context.Background())

	recorder.mu.Lock()
	calls := append([]string(nil), recorder.calls...)
	recorder.mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("只应检测开启用户的 3 个模型，实际 %v", calls)
	}
	for _, call := range calls {
		if call == "other-user-model" {
			t.Fatal("检测串到了未开启的另一个用户")
		}
	}

	snapshot := manager.Snapshot(1)
	if !snapshot.Enabled || len(snapshot.Models) != 3 {
		t.Fatalf("用户 1 快照不对: %+v", snapshot)
	}
	if !snapshot.Models["11:good"].OK || !snapshot.Models["12:good-2"].OK {
		t.Fatalf("成功模型应标绿: %+v", snapshot.Models)
	}
	bad := snapshot.Models["11:bad"]
	if bad.OK || bad.Error != "upstream down" || bad.CheckedAt == 0 {
		t.Fatalf("失败模型应标红并带错误: %+v", bad)
	}
	if other := manager.Snapshot(2); other.Enabled || len(other.Models) != 0 {
		t.Fatalf("用户 2 不应有检测结果: %+v", other)
	}
}

func TestSetEnabledPersistsAndClearsResults(t *testing.T) {
	st := &fakeStore{settings: map[int64]string{}}
	manager := New(st)
	manager.results[1] = map[string]Status{"1:m": {OK: true}}
	if err := manager.SetEnabled(1, true); err != nil {
		t.Fatal(err)
	}
	if st.settings[1] != "1" || !manager.Enabled(1) || len(manager.Snapshot(1).Models) != 0 {
		t.Fatalf("开启状态和旧结果处理不对: %+v", manager.Snapshot(1))
	}
	select {
	case <-manager.wake:
	default:
		t.Fatal("开启后应立即触发检测")
	}
	if err := manager.SetEnabled(1, false); err != nil {
		t.Fatal(err)
	}
	if st.settings[1] != "0" || manager.Enabled(1) {
		t.Fatal("关闭状态没有持久化")
	}
}

type blockingPinger struct {
	started chan struct{}
	release chan struct{}
}

func (p blockingPinger) Ping(context.Context, string) (time.Duration, error) {
	close(p.started)
	<-p.release
	return time.Millisecond, nil
}

func TestDisableDiscardsInflightResults(t *testing.T) {
	st := &fakeStore{
		users:     []store.User{{ID: 1}},
		settings:  map[int64]string{},
		providers: map[int64][]store.Provider{1: {{ID: 11}}},
		models:    map[int64][]store.Model{11: {{ModelID: "slow"}}},
	}
	manager := New(st)
	probe := blockingPinger{started: make(chan struct{}), release: make(chan struct{})}
	manager.newClient = func(_, _ string) pinger { return probe }
	if err := manager.SetEnabled(1, true); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		manager.checkAll(context.Background())
		close(done)
	}()
	<-probe.started
	if err := manager.SetEnabled(1, false); err != nil {
		t.Fatal(err)
	}
	close(probe.release)
	<-done
	if snapshot := manager.Snapshot(1); snapshot.Enabled || len(snapshot.Models) != 0 {
		t.Fatalf("关闭后在途结果不应写回: %+v", snapshot)
	}
}

type concurrencyPinger struct {
	mu     sync.Mutex
	active int
	max    int
}

func (p *concurrencyPinger) Ping(context.Context, string) (time.Duration, error) {
	p.mu.Lock()
	p.active++
	if p.active > p.max {
		p.max = p.active
	}
	p.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	p.mu.Lock()
	p.active--
	p.mu.Unlock()
	return time.Millisecond, nil
}

func TestGlobalWorkerPoolHonorsConcurrency(t *testing.T) {
	st := &fakeStore{
		users:     []store.User{{ID: 1}},
		settings:  map[int64]string{1: "1"},
		providers: map[int64][]store.Provider{1: {{ID: 11}}},
		models: map[int64][]store.Model{11: {
			{ModelID: "m1"}, {ModelID: "m2"}, {ModelID: "m3"}, {ModelID: "m4"},
			{ModelID: "m5"}, {ModelID: "m6"}, {ModelID: "m7"}, {ModelID: "m8"},
		}},
	}
	manager := New(st)
	probe := &concurrencyPinger{}
	manager.newClient = func(_, _ string) pinger { return probe }
	if manager.Concurrency() != defaultConcurrency {
		t.Fatalf("默认并发应为 %d，实际 %d", defaultConcurrency, manager.Concurrency())
	}
	if err := manager.SetConcurrency(0); err == nil {
		t.Fatal("并发为 0 应被拒绝")
	}
	if err := manager.SetConcurrency(maxConcurrency + 1); err == nil {
		t.Fatal("超过上限的并发应被拒绝")
	}
	if err := manager.SetConcurrency(3); err != nil {
		t.Fatal(err)
	}
	manager.checkAll(context.Background())
	probe.mu.Lock()
	max := probe.max
	probe.mu.Unlock()
	if max != 3 {
		t.Fatalf("全局 worker 池最大并发应为 3，实际 %d", max)
	}
	if got := len(manager.Snapshot(1).Models); got != 8 {
		t.Fatalf("8 个模型都应由同一 worker 池处理，实际 %d", got)
	}
}
