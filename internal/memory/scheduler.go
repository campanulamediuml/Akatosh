package memory

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

var archiveLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

// LatestArchiveTime 返回 now 之前最近一次应该执行的 04:00。
func LatestArchiveTime(now time.Time) time.Time {
	local := now.In(archiveLocation)
	latest := time.Date(local.Year(), local.Month(), local.Day(), 4, 0, 0, 0, archiveLocation)
	if local.Before(latest) {
		latest = latest.AddDate(0, 0, -1)
	}
	return latest
}

func (m *Manager) RunAll(ctx context.Context, onlyUser int64) []Report {
	reports := []Report{}
	users := []storeUser{}
	if onlyUser > 0 {
		users = append(users, storeUser{ID: onlyUser})
	} else if list, err := m.main.ListUsers(); err == nil {
		alive := map[int64]bool{}
		for _, user := range list {
			alive[user.ID] = true
			users = append(users, storeUser{ID: user.ID})
		}
		entries, _ := os.ReadDir(filepath.Join(m.root, "users"))
		for _, entry := range entries {
			id, err := strconv.ParseInt(entry.Name(), 10, 64)
			if entry.IsDir() && err == nil && !alive[id] {
				if err := m.DeleteUser(id); err != nil {
					log.Printf("[memory] 清理已删除用户目录 %d 失败: %v", id, err)
				}
			}
		}
	}
	for _, user := range users {
		if ctx.Err() != nil {
			break
		}
		report, err := m.RunMaintenance(ctx, user.ID)
		if err != nil {
			log.Printf("[memory] 用户 %d 维护失败: %v", user.ID, err)
			report.Failed++
			report.UserID = user.ID
		}
		reports = append(reports, report)
		log.Printf("[memory] 每日维护完成：%s", report.String())
	}
	return reports
}

type storeUser struct{ ID int64 }

// RunDue 为每个用户补跑最近错过的一次 04:00，不会把漏掉的每天重复跑很多遍。
func (m *Manager) RunDue(ctx context.Context) {
	users, err := m.main.ListUsers()
	if err != nil {
		log.Printf("[memory] 读取用户列表失败: %v", err)
		return
	}
	latest := LatestArchiveTime(time.Now()).UnixMilli()
	for _, user := range users {
		u, err := m.user(user.ID)
		if err != nil {
			log.Printf("[memory] 打开用户 %d 记忆库失败: %v", user.ID, err)
			continue
		}
		if u.maintenanceValue(maintenanceLastSuccess) >= latest {
			continue
		}
		if _, err := m.RunMaintenance(ctx, user.ID); err != nil {
			log.Printf("[memory] 用户 %d 补跑归档失败: %v", user.ID, err)
		}
	}
}

func (m *Manager) RunScheduler(ctx context.Context) {
	m.RunDue(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.RunDue(ctx)
		}
	}
}
