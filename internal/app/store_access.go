package app

import (
	"context"
	"errors"
)

func (s *Store) Close() error                   { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func (s *Store) SchemaReady(ctx context.Context) error {
	for _, table := range []string{"ghraft_locks", "ghraft_watch_access", "ghraft_work_links", "ghraft_watches", "ghraft_meta", "ghraft_legacy_history", "ghraft_triage", "ghraft_snapshots", "ghraft_events", "ghraft_private", "ghraft_results"} {
		if !s.db.Session(ctx).Migrator().HasTable(table) {
			return errors.New("database schema is incomplete; run migrate")
		}
	}
	return nil
}
func (s *Store) ResetDelivery(ctx context.Context, key string) error {
	return s.db.Session(ctx).Model(&eventRecord{}).Where("state = ? AND resource_key = ?", "pending", key).UpdateColumn("next_attempt", 0).Error
}
func (s *Store) InsertMeta(ctx context.Context, key, value string) error {
	err := s.db.Upsert(ctx, &metaRecord{key, value}, []string{"name"}, nil)
	return err
}
func (s *Store) RegisterAccess(ctx context.Context, key, source string) error {
	var updates []string
	if source == "explicit" {
		updates = []string{"source"}
	} else if source != "discovered" {
		return errors.New("invalid watch source")
	}
	err := s.db.Upsert(ctx, &accessRecord{key, source, false}, []string{"resource_key"}, updates)
	return err
}
func (s *Store) SeedAccess(ctx context.Context) error {
	watches, err := s.Watches(ctx)
	if err != nil {
		return err
	}
	for _, w := range watches {
		if err = s.RegisterAccess(ctx, w.Key(), "discovered"); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) WatchSource(ctx context.Context, key string) (string, error) {
	var source string
	err := s.db.QueryRowContext(ctx, "SELECT source FROM ghraft_watch_access WHERE resource_key=?", key).Scan(&source)
	return source, err
}
func (s *Store) WatchBlocked(ctx context.Context, key string) (bool, error) {
	var blocked bool
	err := s.db.QueryRowContext(ctx, "SELECT blocked FROM ghraft_watch_access WHERE resource_key=?", key).Scan(&blocked)
	return blocked, err
}
func (s *Store) SetBlocked(ctx context.Context, key string, blocked bool) error {
	return s.db.Session(ctx).Model(&accessRecord{}).Where("resource_key = ?", key).UpdateColumn("blocked", blocked).Error
}
func (s *Store) ObservedAt(ctx context.Context, key string) (int64, error) {
	var observed int64
	err := s.db.QueryRowContext(ctx, "SELECT observed_at FROM ghraft_snapshots WHERE resource_key=?", key).Scan(&observed)
	return observed, err
}
func (s *Store) Event(ctx context.Context, id string) (Event, error) {
	var event Event
	var payload string
	err := s.db.QueryRowContext(ctx, "SELECT event_id,resource_key,version_hash,payload,state FROM ghraft_events WHERE event_id=?", id).Scan(&event.ID, &event.Key, &event.Version, &payload, &event.State)
	event.Payload = []byte(payload)
	return event, err
}
func (s *Store) TaskLinked(ctx context.Context, key string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ghraft_work_links WHERE resource_key=?", key).Scan(&count)
	return count > 0, err
}
