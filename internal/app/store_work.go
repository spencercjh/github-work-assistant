package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

func (s *Store) ReadWork(ctx context.Context, id string) (WorkContext, error) {
	var w WorkContext
	var payload, body string
	err := s.db.QueryRowContext(ctx, `SELECT e.event_id,e.resource_key,e.version_hash,e.payload,e.state,s.body,s.version_hash,s.observed_at FROM ghraft_events e JOIN ghraft_snapshots s ON s.resource_key=e.resource_key WHERE e.event_id=?`, id).Scan(&w.Event.ID, &w.Event.Key, &w.Event.Version, &payload, &w.Event.State, &body, &w.CurrentVersion, &w.ObservedAt)
	if err != nil {
		return w, err
	}
	w.Event.Payload = json.RawMessage(payload)
	w.Snapshot = json.RawMessage(body)
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
func (s *Store) SaveWorkResult(ctx context.Context, key string, in WorkResult, guide RepositoryGuide) error {
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
	body := string(canonical(struct {
		WorkResult
		Guide RepositoryGuide `json:"guide"`
	}{in, guide}))
	err = tx.Upsert(ctx, &resultRecord{in.EventID, in.ReportID, body, time.Now().Unix()}, []string{"event_id", "report_id"}, nil)
	if err != nil {
		return err
	}
	var saved string
	if err = tx.QueryRowContext(ctx, `SELECT body FROM ghraft_results WHERE event_id=? AND report_id=?`, in.EventID, in.ReportID).Scan(&saved); err != nil {
		return err
	}
	if saved != body {
		return errors.New("report already recorded with different content")
	}
	return tx.Commit()
}
