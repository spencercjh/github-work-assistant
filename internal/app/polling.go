package app

import (
	"context"
	"encoding/json"
	"errors"
)

type notificationMarker struct {
	UpdatedAt int64  `json:"updated_at"`
	Version   string `json:"version"`
}

func notificationKey(key string) string        { return "poll-notification:" + hash([]byte(key)) }
func appliedNotificationKey(key string) string { return "applied-notification:" + hash([]byte(key)) }

func (s *Service) readNotification(ctx context.Context, key string) (notificationMarker, error) {
	raw, err := s.Store.Meta(ctx, notificationKey(key), "{}")
	var marker notificationMarker
	if err == nil {
		err = json.Unmarshal([]byte(raw), &marker)
	}
	return marker, err
}

// Persist the trigger before advancing the discovery cursor. A failed refresh
// retries it even when the next Notifications response no longer contains it.
func (s *Service) rememberNotification(ctx context.Context, n Notification) error {
	old, err := s.readNotification(ctx, n.Key())
	if err != nil {
		return err
	}
	if n.Version == old.Version || n.UpdatedAt < old.UpdatedAt {
		return nil
	}
	return s.Store.PutMeta(ctx, notificationKey(n.Key()), string(canonical(notificationMarker{n.UpdatedAt, n.Version})))
}

// Terminal work has no periodic refresh, including after an upgrade/restart.
// New notifications trigger one full refresh; active work is still polled.
// Explicit refreshes and operation/completion checks bypass this schedule.
func (s *Service) pollWatch(ctx context.Context, w Watch) (bool, error) {
	body, _, err := s.Store.Snapshot(ctx, w.Key())
	if err != nil && !errors.Is(err, ErrNotFound) {
		return false, err
	}
	notification, err := s.readNotification(ctx, w.Key())
	if err != nil {
		return false, err
	}
	applied, err := s.Store.Meta(ctx, appliedNotificationKey(w.Key()), "")
	if err != nil {
		return false, err
	}
	if githubEnded(body) && notification.Version == applied {
		return false, nil
	}
	if err = s.collectWatch(ctx, w); err != nil {
		return true, err
	}
	return true, s.Store.PutMeta(ctx, appliedNotificationKey(w.Key()), notification.Version)
}
