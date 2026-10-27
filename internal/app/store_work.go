package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

func (s *Store) ReadWork(ctx context.Context, id string) (WorkContext, error) {
	var w WorkContext
	var payload, body, receipt string
	err := s.db.QueryRowContext(ctx, `SELECT e.event_id,e.resource_key,e.version_hash,e.payload,e.state,COALESCE(e.receipt,''),s.body,s.version_hash,s.observed_at FROM ghraft_events e JOIN ghraft_snapshots s ON s.resource_key=e.resource_key WHERE e.event_id=?`, id).Scan(&w.Event.ID, &w.Event.Key, &w.Event.Version, &payload, &w.Event.State, &receipt, &body, &w.CurrentVersion, &w.ObservedAt)
	if err != nil {
		return w, err
	}
	w.Event.Payload = json.RawMessage(payload)
	if receipt != "" {
		w.Event.Receipt = json.RawMessage(receipt)
	}
	w.Snapshot = json.RawMessage(body)
	w.CurrentEventID, err = s.CurrentEvent(ctx, w.Event.Key, w.CurrentVersion)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return w, err
	}
	link := &TaskLink{}
	err = s.db.QueryRowContext(ctx, `SELECT task_id,target,event_id,evidence_version,guide_version FROM ghraft_work_links WHERE resource_key=?`, w.Event.Key).Scan(&link.TaskID, &link.Target, &link.EventID, &link.EvidenceVersion, &link.GuideVersion)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return w, err
	}
	if err == nil {
		w.Task = link
	}
	return w, nil
}
func (s *Store) BindTask(ctx context.Context, key string, in WorkInput, target string) (TaskLink, error) {
	tx, err := s.db.BeginWrite(ctx, "resource:"+key)
	if err != nil {
		return TaskLink{}, err
	}
	defer tx.Rollback()
	var version string
	if err = tx.QueryRowContext(ctx, `SELECT version_hash FROM ghraft_snapshots WHERE resource_key=?`, key).Scan(&version); err != nil {
		return TaskLink{}, err
	}
	if version != in.EvidenceVersion {
		return TaskLink{}, ErrStaleWork
	}
	var task, existingTarget string
	err = tx.QueryRowContext(ctx, `SELECT task_id,target FROM ghraft_work_links WHERE resource_key=?`, key).Scan(&task, &existingTarget)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return TaskLink{}, err
	}
	if err == nil && (task != in.TaskID || existingTarget != target) {
		return TaskLink{}, errors.New("resource already belongs to a different Raft task or destination")
	}
	err = tx.Upsert(ctx, &workLinkRecord{key, in.TaskID, target, in.EventID, in.EvidenceVersion, in.GuideVersion, time.Now().Unix()}, []string{"resource_key"}, []string{"event_id", "evidence_version", "guide_version", "updated_at"})
	if err != nil {
		return TaskLink{}, err
	}
	if err = tx.Commit(); err != nil {
		return TaskLink{}, err
	}
	return TaskLink{in.TaskID, target, in.EventID, in.EvidenceVersion, in.GuideVersion}, nil
}
func (s *Store) SaveWorkResult(ctx context.Context, key string, in WorkResult) error {
	tx, err := s.db.BeginWrite(ctx, "resource:"+key)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current string
	if err = tx.QueryRowContext(ctx, `SELECT version_hash FROM ghraft_snapshots WHERE resource_key=?`, key).Scan(&current); err != nil {
		return err
	}
	if current != in.EvidenceVersion {
		return ErrStaleWork
	}
	if err = saveReport(ctx, tx, in.EventID, in.ReportID, canonical(in)); err != nil {
		return err
	}
	return tx.Commit()
}

// CurrentEvent resolves the latest event for a snapshot, including snapshots
// persisted before current-event indexes were introduced.
func (s *Store) CurrentEvent(ctx context.Context, key, version string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT e.event_id FROM ghraft_events e JOIN ghraft_meta m ON m.value=e.event_id WHERE m.name=? AND e.resource_key=? AND e.version_hash=?`, "current-event:"+key, key, version).Scan(&id)
	if errors.Is(err, ErrNotFound) {
		// Existing snapshots created before current-event tracking can still be read.
		err = s.db.QueryRowContext(ctx, `SELECT event_id FROM ghraft_events WHERE resource_key=? AND version_hash=? ORDER BY created_at DESC,event_id DESC LIMIT 1`, key, version).Scan(&id)
	}
	return id, err
}
