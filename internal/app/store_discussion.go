package app

import (
	"context"
	"encoding/json"
	"errors"

	"github-raft-app/internal/database"
)

// Discussion fixes the root for a resource. Ownership lives only in Raft: the
// collector never clears or assigns it, including when the task is completed.
type Discussion struct {
	Identity string        `json:"identity"`
	Server   string        `json:"server"`
	Channel  string        `json:"channel"`
	EventID  string        `json:"event_id"`
	Task     *TaskDispatch `json:"task,omitempty"`
}

func discussionKey(resource string) string { return "discussion:" + hash([]byte(resource)) }

func (s *Store) Discussion(ctx context.Context, resource string) (Discussion, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM ghraft_meta WHERE name=?", discussionKey(resource)).Scan(&value)
	var d Discussion
	if err == nil && json.Unmarshal([]byte(value), &d) != nil {
		err = errors.New("invalid stored discussion")
	}
	return d, err
}

func discussionCandidates(ctx context.Context, tx *database.Tx, resource, channel string) ([]TaskDispatch, error) {
	var events []eventRecord
	if err := tx.Session(ctx).Select("receipt").Where("resource_key = ? AND state = ?", resource, "accepted").Find(&events).Error; err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []TaskDispatch
	for _, event := range events {
		var task TaskDispatch
		if event.Receipt == nil || json.Unmarshal([]byte(*event.Receipt), &task) != nil || !task.valid() || task.Channel != channel {
			continue // Older App and DM receipts do not identify a shared task.
		}
		task.NotificationMessageID = ""
		if !seen[task.MessageID] {
			seen[task.MessageID] = true
			out = append(out, task)
		}
	}
	return out, nil
}

func (s *Store) DiscussionCandidates(ctx context.Context, resource, channel string) ([]TaskDispatch, error) {
	tx, err := s.db.BeginWrite(ctx, "resource:"+resource)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return discussionCandidates(ctx, tx, resource, channel)
}

// Preparing the root under the resource lock makes different concurrent events
// share ONE initial event and its frozen, idempotent task-creation request. No
// database transaction is held during a network call.
func (s *Store) PrepareDiscussion(ctx context.Context, resource string, proposed Discussion, adopt string) (Discussion, error) {
	tx, err := s.db.BeginWrite(ctx, "resource:"+resource)
	if err != nil {
		return Discussion{}, err
	}
	defer tx.Rollback()
	var record metaRecord
	err = database.RecordError(tx.Session(ctx).Where("name = ?", discussionKey(resource)).Take(&record).Error)
	if err == nil {
		var saved Discussion
		if json.Unmarshal([]byte(record.Value), &saved) != nil || saved.Identity != proposed.Identity || saved.Server != proposed.Server || saved.Channel != proposed.Channel {
			return Discussion{}, errors.New("discussion belongs to a different sender, server or channel; restore its configuration")
		}
		if adopt != "" && (saved.Task == nil || saved.Task.MessageID != adopt) {
			return Discussion{}, errors.New("resource already has a fixed discussion; do not replace its root")
		}
		return saved, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Discussion{}, err
	}
	var unresolved int64
	if err := tx.Session(ctx).Raw(`SELECT COUNT(*) FROM ghraft_events e JOIN ghraft_meta m ON SUBSTR(m.name, 19) = e.event_id WHERE m.name LIKE ? AND e.resource_key = ? AND e.state = ?`, "external-delivery:%", resource, "pending").Scan(&unresolved).Error; err != nil {
		return Discussion{}, err
	}
	if unresolved > 0 {
		return Discussion{}, errors.New("unresolved legacy delivery; reconcile it with the previous version before adopting a discussion")
	}
	candidates, err := discussionCandidates(ctx, tx, resource, proposed.Channel)
	if err != nil {
		return Discussion{}, err
	}
	if adopt != "" {
		for _, task := range candidates {
			if task.MessageID == adopt {
				proposed.Task = &task
			}
		}
		if proposed.Task == nil {
			return Discussion{}, errors.New("selected task is not an accepted delivery for this resource and channel")
		}
	} else if len(candidates) == 1 {
		proposed.Task = &candidates[0]
	} else if len(candidates) > 1 {
		return Discussion{}, errors.New("multiple existing tasks for this resource; an operator must choose one with adopt-discussion")
	}
	// An adopted root already contains an older event, not this new event.
	if proposed.Task != nil {
		proposed.EventID = ""
	}
	if err = tx.Upsert(ctx, &metaRecord{discussionKey(resource), string(canonical(proposed))}, []string{"name"}, nil); err != nil {
		return Discussion{}, err
	}
	return proposed, tx.Commit()
}

func (s *Store) ConfirmDiscussion(ctx context.Context, resource string, expected Discussion, task TaskDispatch) error {
	if !task.valid() || task.Channel != expected.Channel {
		return errors.New("invalid discussion receipt")
	}
	tx, err := s.db.BeginWrite(ctx, "resource:"+resource)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var record metaRecord
	if err = tx.Session(ctx).Where("name = ?", discussionKey(resource)).Take(&record).Error; err != nil {
		return err
	}
	var saved Discussion
	if json.Unmarshal([]byte(record.Value), &saved) != nil || saved.Identity != expected.Identity || saved.Server != expected.Server || saved.Channel != expected.Channel || saved.EventID != expected.EventID {
		return errors.New("discussion changed during creation")
	}
	if saved.Task != nil && *saved.Task != task {
		return errors.New("resource already has a different discussion root")
	}
	saved.Task = &task
	if err = tx.Upsert(ctx, &metaRecord{discussionKey(resource), string(canonical(saved))}, []string{"name"}, []string{"value"}); err != nil {
		return err
	}
	return tx.Commit()
}
