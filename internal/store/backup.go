package store

import (
	"context"
	"fmt"
	"strings"
)

// Backup 通过 SQLite 在线生成包含 WAL 已提交内容的一致性副本。
func (s *Store) Backup(ctx context.Context, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("备份路径不能为空")
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("生成 SQLite 一致性备份失败: %w", err)
	}
	return nil
}
