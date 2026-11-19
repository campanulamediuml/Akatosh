// Package modelcheck 按用户定时检查其全部供应商和模型。
package modelcheck

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"aiassistant/internal/llm"
	"aiassistant/internal/store"
)

const (
	interval           = time.Minute
	requestTimeout     = 20 * time.Second
	defaultConcurrency = 16
	minConcurrency     = 1
	maxConcurrency     = 64
)

// Store 是检测器需要的用户、设置和模型读取能力。
type Store interface {
	ListUsers() ([]store.User, error)
	GetSetting(userID int64, key string) (string, error)
	SetSetting(userID int64, key, value string) error
	ListProviders(userID int64) ([]store.Provider, error)
	ListModels(userID int64, providerID int64) ([]store.Model, error)
}

type pinger interface {
	Ping(ctx context.Context, model string) (time.Duration, error)
}

// Status 是一个模型最近一次检测的结果。
type Status struct {
	OK        bool   `json:"ok"`
	CheckedAt int64  `json:"checked_at"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// Snapshot 是某个用户当前看到的检测开关和全部已知结果。
type Snapshot struct {
	Enabled bool              `json:"enabled"`
	Models  map[string]Status `json:"models"`
}

// Manager 保存内存态结果；开关和全局并发由 Store 持久化。
type Manager struct {
	st        Store
	newClient func(baseURL, apiKey string) pinger
	mu        sync.RWMutex
	results   map[int64]map[string]Status
	overrides map[int64]bool
	epochs    map[int64]uint64
	wake      chan struct{}
}

func New(st Store) *Manager {
	return &Manager{
		st: st,
		newClient: func(baseURL, apiKey string) pinger {
			return llm.New(baseURL, apiKey)
		},
		results:   map[int64]map[string]Status{},
		overrides: map[int64]bool{},
		epochs:    map[int64]uint64{},
		wake:      make(chan struct{}, 1),
	}
}

func (m *Manager) Enabled(userID int64) bool {
	m.mu.RLock()
	enabled, ok := m.overrides[userID]
	m.mu.RUnlock()
	if ok {
		return enabled
	}
	value, err := m.st.GetSetting(userID, store.SettingModelHealthEnabled)
	return err == nil && value == "1"
}

// SetEnabled 持久化开关，并立即触发一轮检测；关闭时清掉旧颜色。
func (m *Manager) SetEnabled(userID int64, enabled bool) error {
	value := "0"
	if enabled {
		value = "1"
	}
	if err := m.st.SetSetting(userID, store.SettingModelHealthEnabled, value); err != nil {
		return err
	}
	m.mu.Lock()
	m.overrides[userID] = enabled
	m.epochs[userID]++
	delete(m.results, userID)
	m.mu.Unlock()
	if enabled {
		m.Trigger()
	}
	return nil
}

func (m *Manager) Concurrency() int {
	value, err := m.st.GetSetting(0, store.SettingModelHealthConcurrency)
	if err != nil {
		return defaultConcurrency
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < minConcurrency || n > maxConcurrency {
		return defaultConcurrency
	}
	return n
}

func (m *Manager) SetConcurrency(n int) error {
	if n < minConcurrency || n > maxConcurrency {
		return fmt.Errorf("全局并发数应在 %d–%d 之间", minConcurrency, maxConcurrency)
	}
	return m.st.SetSetting(0, store.SettingModelHealthConcurrency, strconv.Itoa(n))
}

func (m *Manager) ClearUser(userID int64) {
	m.mu.Lock()
	delete(m.results, userID)
	delete(m.overrides, userID)
	delete(m.epochs, userID)
	m.mu.Unlock()
}

func (m *Manager) Trigger() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) Snapshot(userID int64) Snapshot {
	enabled := m.Enabled(userID)
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := Snapshot{Enabled: enabled, Models: map[string]Status{}}
	for key, status := range m.results[userID] {
		out.Models[key] = status
	}
	return out
}

// Run 启动后立即检查，此后每分钟一轮；同一轮未结束时不重叠开新一轮。
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	m.checkAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkAll(ctx)
		case <-m.wake:
			m.checkAll(ctx)
		}
	}
}

type checkJob struct {
	userID     int64
	providerID int64
	modelID    string
	epoch      uint64
	client     pinger
}

func (m *Manager) checkAll(ctx context.Context) {
	users, err := m.st.ListUsers()
	if err != nil {
		log.Printf("[modelcheck] 读取用户失败: %v", err)
		return
	}

	jobs := make(chan checkJob)
	concurrency := m.Concurrency()
	var workers sync.WaitGroup
	workers.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer workers.Done()
			m.worker(ctx, jobs)
		}()
	}

schedule:
	for _, user := range users {
		if !m.Enabled(user.ID) {
			continue
		}
		providers, err := m.st.ListProviders(user.ID)
		if err != nil {
			log.Printf("[modelcheck] 用户 %d 读取供应商失败: %v", user.ID, err)
			continue
		}
		for _, provider := range providers {
			if ctx.Err() != nil || !m.Enabled(user.ID) {
				continue
			}
			models, err := m.st.ListModels(user.ID, provider.ID)
			if err != nil {
				log.Printf("[modelcheck] 用户 %d 供应商 %d 读取模型失败: %v", user.ID, provider.ID, err)
				continue
			}
			m.mu.RLock()
			epoch := m.epochs[user.ID]
			m.mu.RUnlock()
			client := m.newClient(provider.BaseURL, provider.APIKey)
			for _, model := range models {
				if ctx.Err() != nil || !m.Enabled(user.ID) {
					break
				}
				job := checkJob{userID: user.ID, providerID: provider.ID, modelID: model.ModelID, epoch: epoch, client: client}
				select {
				case jobs <- job:
				case <-ctx.Done():
					break schedule
				}
			}
		}
	}
	close(jobs)
	workers.Wait()
}

func (m *Manager) worker(ctx context.Context, jobs <-chan checkJob) {
	for job := range jobs {
		if ctx.Err() != nil || !m.Enabled(job.userID) {
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		started := time.Now()
		latency, err := job.client.Ping(requestCtx, job.modelID)
		cancel()
		status := Status{CheckedAt: time.Now().UnixMilli(), LatencyMS: latency.Milliseconds()}
		if err != nil {
			status.Error = err.Error()
		} else {
			status.OK = true
			if status.LatencyMS == 0 {
				status.LatencyMS = time.Since(started).Milliseconds()
			}
		}
		m.mu.Lock()
		if m.epochs[job.userID] == job.epoch {
			if m.results[job.userID] == nil {
				m.results[job.userID] = map[string]Status{}
			}
			m.results[job.userID][strconv.FormatInt(job.providerID, 10)+":"+job.modelID] = status
		}
		m.mu.Unlock()
	}
}
