package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

func (s *Store) RequestWork(ctx context.Context, key, version, requestID, reason string) (Event, error) {
	id := hash([]byte("work-request:" + key + ":" + requestID))
	tx, err := s.db.BeginWrite(ctx, "resource:"+key)
	if err != nil {
		return Event{}, err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT event_id FROM ghraft_events WHERE event_id=?`, id).Scan(&existing)
	if err == nil {
		if err = tx.Commit(); err != nil {
			return Event{}, err
		}
		return s.Event(ctx, id)
	}
	if !errors.Is(err, ErrNotFound) {
		return Event{}, err
	}
	var snap snapshotRecord
	if err = tx.QueryRowContext(ctx, `SELECT version_hash,body FROM ghraft_snapshots WHERE resource_key=?`, key).Scan(&snap.VersionHash, &snap.Body); err != nil {
		return Event{}, err
	}
	if snap.VersionHash != version {
		return Event{}, ErrStaleWork
	}
	var snapshot Snapshot
	if json.Unmarshal([]byte(snap.Body), &snapshot) != nil {
		return Event{}, errors.New("invalid work snapshot")
	}
	classification := snapshot.Classification
	var private string
	err = tx.QueryRowContext(ctx, `SELECT value FROM ghraft_meta WHERE name=?`, "private-watch:"+key).Scan(&private)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Event{}, err
	}
	if private == "true" {
		classification = "private"
	}
	payload := canonical(map[string]any{"event_id": id, "resource_key": key, "version": version, "data_classification": classification, "subject": subjectOf(snapshot), "kind": "work.requested", "request_id": requestID, "reason": reason, "changes": []Change{}})
	row := eventRecord{EventID: id, ResourceKey: key, VersionHash: version, Payload: string(payload), State: "pending", CreatedAt: time.Now().Unix()}
	if err = tx.Session(ctx).Create(&row).Error; err != nil {
		return Event{}, err
	}
	if err = tx.Upsert(ctx, &metaRecord{"current-event:" + key, id}, []string{"name"}, []string{"value"}); err != nil {
		return Event{}, err
	}
	if err = tx.Commit(); err != nil {
		return Event{}, err
	}
	return s.Event(ctx, id)
}
