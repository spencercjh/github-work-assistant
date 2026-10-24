package app

import (
	"context"
	"errors"
	"strings"
)

// Reconcile operator configuration before collection, including removed explicit watches.
func (s *Service) reconcileWatches(ctx context.Context) error {
	for _, w := range s.Config.Watches {
		if err := s.Store.RegisterWatch(ctx, w); err != nil {
			return err
		}
		err := s.Store.RegisterAccess(ctx, w.Key(), "explicit")
		if err != nil {
			return err
		}
	}
	// Imported legacy watches were user-authorized discovery. Current discovery still gates access.
	return s.Store.SeedAccess(ctx)
}

func (s *Service) watchConfigured(ctx context.Context, key string) bool {
	key = strings.ToLower(key)
	if _, ok := s.Config.Watch(key); ok {
		return true
	}
	if !s.Config.Discover {
		return false
	}
	source, err := s.Store.WatchSource(ctx, key)
	return err == nil && source == "discovered"
}

func (s *Service) allowed(ctx context.Context, key string) bool {
	if !s.watchConfigured(ctx, key) {
		return false
	}
	blocked, err := s.Store.WatchBlocked(ctx, strings.ToLower(key))
	return errors.Is(err, ErrNotFound) || (err == nil && !blocked)
}

func (s *Service) collectWatch(ctx context.Context, w Watch) error {
	b, err := s.GH.Collect(ctx, w)
	if errors.Is(err, ErrNotPublic) {
		dbErr := s.Store.SetBlocked(ctx, w.Key(), true)
		return errors.Join(err, dbErr)
	}
	if err != nil {
		return err
	}
	if _, err = s.Store.Observe(ctx, w.Key(), b); err != nil {
		return err
	}
	err = s.Store.SetBlocked(ctx, w.Key(), false)
	return err
}

// Revoked history cannot occupy the first page and starve current authorized work.
func (s *Service) Pending(ctx context.Context) ([]map[string]any, error) {
	out := []map[string]any{}
	for offset := 0; len(out) < 100; offset += 100 {
		page, err := s.Store.UnprocessedPage(ctx, offset)
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
