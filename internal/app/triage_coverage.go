package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github-raft-app/internal/database"
)

// Coverage is an explicit judgment by the current owner, not an inference
// that every older notification is irrelevant when a new report arrives.
func (s *Service) checkTriageCoverage(ctx context.Context, old WorkContext, in Triage) error {
	if !s.Config.SharedTasks() || old.Fork == nil {
		return ErrWorkDenied
	}
	w, err := s.WorkContext(ctx, in.SupersededBy)
	if err != nil {
		return err
	}
	if w.Event.Key != old.Event.Key || w.Fork == nil || w.Task == nil || w.CurrentEventID != in.SupersededBy {
		return ErrStaleWork
	}
	if w.Task.EvidenceVersion != w.CurrentVersion || w.Task.GuideVersion != w.Guide.Version {
		return ErrStaleWork
	}
	_, err = s.CheckWork(ctx, WorkInput{Executor: in.Executor, EventID: in.SupersededBy, EvidenceVersion: w.CurrentVersion, GuideVersion: w.Guide.Version, TaskID: w.Task.TaskID})
	return err
}

// Hold the resource lock through validation and insertion so an intervening
// collector/request cannot make a coverage receipt silently consume new work.
func verifySupersededReport(ctx context.Context, tx *database.Tx, in Triage) error {
	var old, covering eventRecord
	for _, item := range []struct {
		id string
		e  *eventRecord
	}{{in.EventID, &old}, {in.SupersededBy, &covering}} {
		if err := tx.Session(ctx).Where("event_id = ?", item.id).Take(item.e).Error; err != nil {
			return err
		}
	}
	if old.EventID == covering.EventID || old.ResourceKey != covering.ResourceKey || old.State != "accepted" || covering.State != "accepted" || old.CreatedAt > covering.CreatedAt {
		return errors.New("coverage requires a different, already delivered older event of the same maintenance work")
	}
	var payload struct {
		Kind   string
		Pruned bool `json:"body_pruned"`
	}
	if json.Unmarshal([]byte(old.Payload), &payload) != nil || payload.Kind != "" || payload.Pruned {
		return errors.New("explicit work requests or compacted event bodies cannot be newly superseded; handle the event separately")
	}
	var current string
	if err := tx.QueryRowContext(ctx, "SELECT value FROM ghraft_meta WHERE name=?", "current-event:"+old.ResourceKey).Scan(&current); err != nil {
		return err
	}
	if current != covering.EventID {
		return ErrStaleWork
	}
	var snapshot snapshotRecord
	if err := tx.Session(ctx).Where("resource_key = ?", old.ResourceKey).Take(&snapshot).Error; err != nil {
		return err
	}
	var body Snapshot
	if json.Unmarshal([]byte(snapshot.Body), &body) != nil || body.Objects["fork"] == nil {
		return ErrWorkDenied
	}
	if snapshot.VersionHash != covering.VersionHash || snapshot.ObservedAt < time.Now().Add(-10*time.Minute).Unix() {
		return ErrStaleWork
	}
	var link workLinkRecord
	if err := tx.Session(ctx).Where("resource_key = ?", old.ResourceKey).Take(&link).Error; err != nil {
		return err
	}
	var saved resultRecord
	if err := tx.Session(ctx).Where("event_id = ? AND report_id = ?", covering.EventID, in.ReportID).Take(&saved).Error; err != nil {
		return err
	}
	var report WorkResult
	if json.Unmarshal([]byte(saved.Body), &report) != nil || report.EventID != covering.EventID || report.ReportID != in.ReportID || report.Executor != in.Executor || report.EvidenceVersion != snapshot.VersionHash || link.EvidenceVersion != snapshot.VersionHash || report.GuideVersion != link.GuideVersion || report.TaskID != link.TaskID {
		return errors.New("covering report must match the current owner, evidence and linked work")
	}
	if !slices.Contains([]string{"investigated", "changes-ready", "merged"}, report.Outcome) {
		return errors.New("coverage requires a resolved report, not waiting or needs-decision")
	}
	return nil
}
