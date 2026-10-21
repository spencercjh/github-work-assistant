package app

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Reconcile operator configuration before collection, including removed explicit watches.
func (s *Service) reconcileWatches(ctx context.Context) error {
	for _, w := range s.Config.Watches {
		if err := s.Store.RegisterWatch(ctx, w); err != nil {
			return err
		}
		_, err := s.Store.DB.ExecContext(ctx, `INSERT INTO ghraft_watch_access(resource_key,source,blocked) VALUES(?,'explicit',FALSE) ON DUPLICATE KEY UPDATE source='explicit'`, w.Key())
		if err != nil {
			return err
		}
	}
	// Imported legacy watches were user-authorized discovery. Current discovery still gates access.
	_, err := s.Store.DB.ExecContext(ctx, `INSERT IGNORE INTO ghraft_watch_access(resource_key,source,blocked) SELECT resource_key,'discovered',FALSE FROM ghraft_watches`)
	return err
}

func (s *Service) watchConfigured(ctx context.Context, key string) bool {
	key = strings.ToLower(key)
	if _, ok := s.Config.Watch(key); ok {
		return true
	}
	if !s.Config.Discover {
		return false
	}
	var source string
	err := s.Store.DB.QueryRowContext(ctx, `SELECT source FROM ghraft_watch_access WHERE resource_key=?`, key).Scan(&source)
	return err == nil && source == "discovered"
}

func (s *Service) allowed(ctx context.Context, key string) bool {
	if !s.watchConfigured(ctx, key) {
		return false
	}
	var blocked bool
	err := s.Store.DB.QueryRowContext(ctx, `SELECT blocked FROM ghraft_watch_access WHERE resource_key=?`, strings.ToLower(key)).Scan(&blocked)
	return errors.Is(err, sql.ErrNoRows) || (err == nil && !blocked)
}

func (s *Service) collectWatch(ctx context.Context, w Watch) error {
	b, err := s.GH.Collect(ctx, w)
	if errors.Is(err, ErrNotPublic) {
		_, dbErr := s.Store.DB.ExecContext(ctx, `UPDATE ghraft_watch_access SET blocked=TRUE WHERE resource_key=?`, w.Key())
		return errors.Join(err, dbErr)
	}
	if err != nil {
		return err
	}
	if _, err = s.Store.Observe(ctx, w.Key(), b); err != nil {
		return err
	}
	_, err = s.Store.DB.ExecContext(ctx, `UPDATE ghraft_watch_access SET blocked=FALSE WHERE resource_key=?`, w.Key())
	return err
}

// Revoked history cannot occupy the first page and starve current authorized work.
func (s *Service) Pending(ctx context.Context) ([]map[string]any, error) {
	out := []map[string]any{}
	for offset := 0; len(out) < 100; offset += 100 {
		page, err := s.Store.unprocessedPage(ctx, offset)
		if err != nil {
			return nil, err
		}
		for _, item := range page {
			if s.allowed(ctx, str(item["resource_key"])) {
				out = append(out, item)
				if len(out) == 100 {
					break
				}
			}
		}
		if len(page) < 100 {
			break
		}
	}
	return out, nil
}
