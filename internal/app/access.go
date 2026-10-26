package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// Reconcile operator configuration before collection, including removed explicit watches.
func (s *Service) reconcileWatches(ctx context.Context) error {
	for _, f := range s.Config.Forks {
		if err := s.Store.RegisterAccess(ctx, f.Key(), "explicit"); err != nil {
			return err
		}
	}
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
	if _, ok := s.Config.Fork(key); ok {
		return true
	}
	if w, err := resourceWatch(key); err == nil {
		owner, err := s.Store.Meta(ctx, forkPRName(w), "")
		if err != nil || owner != "" {
			return false
		}
	}
	key = strings.ToLower(key)
	private, err := s.Store.Meta(ctx, "private-watch:"+key, "false")
	repo, _, _ := strings.Cut(key, "#")
	if err != nil || (private == "true" && !privateRepositoryAllowed(s.Config.PrivateRepositories, repo)) {
		return false
	}
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
	if f, ok := s.Config.Fork(key); ok {
		return s.forkAllowed(ctx, f)
	}
	if !s.watchConfigured(ctx, key) {
		return false
	}
	blocked, err := s.Store.WatchBlocked(ctx, strings.ToLower(key))
	if !errors.Is(err, ErrNotFound) && (err != nil || blocked) {
		return false
	}
	private, err := s.Store.Meta(ctx, "private-watch:"+strings.ToLower(key), "false")
	if err != nil {
		return false
	}
	if private == "true" {
		repo, _, _ := strings.Cut(key, "#")
		if !privateRepositoryAllowed(s.Config.PrivateRepositories, repo) {
			return false
		}
		// Private cached context requires current GitHub access as well as the
		// configured scope, including on standalone CLI reads and delivery retries.
		g, ok := s.GH.(interface {
			RepositoryClassification(context.Context, string) (string, error)
		})
		if !ok || s.GH.CheckIdentity(ctx) != nil {
			return false
		}
		if _, err := g.RepositoryClassification(ctx, repo); err != nil {
			return false
		}
	}
	return true
}

func (s *Service) collectWatch(ctx context.Context, w Watch) error {
	b, err := s.GH.Collect(ctx, w)
	if errors.Is(err, ErrRepositoryNotAllowed) || errors.Is(err, ErrGitHubAccessDenied) {
		dbErr := s.Store.SetBlocked(ctx, w.Key(), true)
		return errors.Join(err, dbErr)
	}
	if err != nil {
		return err
	}
	var snapshot Snapshot
	if json.Unmarshal(b, &snapshot) == nil && s.Config.managedPR(w, snapshot) {
		return nil
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

func (s *Service) Events(ctx context.Context) ([]Event, error) {
	out := []Event{}
	for offset := 0; len(out) < 100; offset += 100 {
		page, err := s.Store.EventsPage(ctx, false, offset)
		if err != nil {
			return nil, err
		}
		for _, event := range page {
			if s.allowed(ctx, event.Key) {
				out = append(out, event)
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
