package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github-raft-app/internal/database"
)

// Every writer of an event's completion state uses the same resource lock as
// compaction, so a fresh report cannot race past the retention check.
func (s *Store) eventWrite(ctx context.Context, id string) (*database.Tx, string, error) {
	var key string
	if err := s.db.QueryRowContext(ctx, "SELECT resource_key FROM ghraft_events WHERE event_id=?", id).Scan(&key); err != nil {
		return nil, "", err
	}
	tx, err := s.db.BeginWrite(ctx, "resource:"+key)
	return tx, key, err
}

// PrepareDelivery freezes a request only while delivery is pending. Once an
// acceptance receipt is durable, subsequent callers reuse it without sending.
func (s *Store) PrepareDelivery(ctx context.Context, id, envelope string) (string, json.RawMessage, error) {
	tx, _, err := s.eventWrite(ctx, id)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback()
	var event eventRecord
	if err = tx.Session(ctx).Where("event_id = ?", id).Take(&event).Error; err != nil {
		return "", nil, err
	}
	if event.State == "accepted" {
		if event.Receipt == nil || !json.Valid([]byte(*event.Receipt)) {
			return "", nil, errors.New("accepted event has no valid receipt")
		}
		return "", json.RawMessage(*event.Receipt), nil
	}
	name := "external-delivery:" + id
	if err = tx.Upsert(ctx, &metaRecord{name, envelope}, []string{"name"}, nil); err != nil {
		return "", nil, err
	}
	var saved string
	if err = tx.QueryRowContext(ctx, "SELECT value FROM ghraft_meta WHERE name=?", name).Scan(&saved); err != nil {
		return "", nil, err
	}
	return saved, nil, tx.Commit()
}

// The guide version is already part of WorkResult. Keeping the entire guide in
// every report duplicates operator-owned files. Normalize older records when
// comparing or compacting them, without changing their report identity.
func reportBody(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, errors.New("invalid report body")
	}
	delete(fields, "guide")
	return canonical(fields), nil
}

func saveReport(ctx context.Context, tx *database.Tx, id, report string, body []byte) error {
	b, err := reportBody(body)
	if err != nil {
		return err
	}
	fingerprint := hash(b)
	if err = tx.Upsert(ctx, &resultRecord{EventID: id, ReportID: report, Body: string(b), BodyHash: fingerprint, CreatedAt: time.Now().Unix()}, []string{"event_id", "report_id"}, nil); err != nil {
		return err
	}
	var saved resultRecord
	if err = tx.Session(ctx).Where("event_id = ? AND report_id = ?", id, report).Take(&saved).Error; err != nil {
		return err
	}
	previous := saved.BodyHash
	if previous == "" {
		b, err = reportBody([]byte(saved.Body))
		if err != nil {
			return err
		}
		previous = hash(b)
	}
	if previous != fingerprint {
		return errors.New("report already recorded with different content")
	}
	return nil
}

type CleanupStats struct {
	DryRun           bool  `json:"dry_run"`
	RetentionDays    int   `json:"history_retention_days"`
	Events           int64 `json:"event_bodies"`
	Reports          int64 `json:"report_bodies"`
	Triages          int64 `json:"triage_bodies"`
	LegacyEntries    int64 `json:"legacy_bodies"`
	DeliveryRequests int64 `json:"delivery_requests"`
	ExpiredSecrets   int64 `json:"expired_secrets"`
}

// Cleanup removes optional bodies, never event identities, processing markers,
// task links, current snapshots, discovery cursors or unconfirmed deliveries.
func (s *Store) Cleanup(ctx context.Context, days int, dryRun bool) (CleanupStats, error) {
	if days == 0 {
		days = 30
	}
	stats := CleanupStats{DryRun: dryRun, RetentionDays: days}
	if days < 1 || days > 3650 {
		return stats, errors.New("history_retention_days must be 1..3650")
	}
	now := time.Now().Unix()
	cutoff := now - int64(days)*86400
	for _, item := range []struct {
		model any
		where string
		args  []any
		count *int64
	}{
		{&secretRecord{}, "expires_at <= ?", []any{now}, &stats.ExpiredSecrets},
		{&metaRecord{}, "name LIKE ? AND SUBSTR(name, 19) IN (SELECT event_id FROM ghraft_events WHERE state = ?)", []any{"external-delivery:%", "accepted"}, &stats.DeliveryRequests},
	} {
		q := s.db.Session(ctx).Model(item.model).Where(item.where, item.args...)
		if dryRun {
			if err := q.Count(item.count).Error; err != nil {
				return stats, err
			}
		} else {
			r := q.Delete(item.model)
			if r.Error != nil {
				return stats, r.Error
			}
			*item.count = r.RowsAffected
		}
	}
	// Keyset pagination bounds memory and avoids skipping records as bodies are
	// compacted. Each event commits separately; a failed run can safely resume.
	for after := ""; ; {
		var events []eventRecord
		err := s.db.Session(ctx).Select("event_id").Where(`event_id > ? AND state = 'accepted' AND accepted_at > 0 AND accepted_at < ?
			AND (EXISTS (SELECT 1 FROM ghraft_triage t WHERE t.event_id=ghraft_events.event_id) OR EXISTS (SELECT 1 FROM ghraft_results r WHERE r.event_id=ghraft_events.event_id))
			AND (compacted_at=0 OR EXISTS (SELECT 1 FROM ghraft_results r WHERE r.event_id=ghraft_events.event_id AND r.body_pruned=?) OR EXISTS (SELECT 1 FROM ghraft_triage t WHERE t.event_id=ghraft_events.event_id AND t.body_pruned=?))`, after, cutoff, false, false).
			Order("event_id").Limit(100).Find(&events).Error
		if err != nil {
			return stats, err
		}
		for _, event := range events {
			if err := s.compactEvent(ctx, event.EventID, cutoff, now, &stats); err != nil {
				return stats, err
			}
			after = event.EventID
		}
		if len(events) < 100 {
			break
		}
	}
	err := s.compactLegacy(ctx, cutoff, &stats)
	return stats, err
}

func compactBody(body, fingerprint string, keep ...string) (string, error) {
	var original map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &original) != nil || original == nil {
		return "", errors.New("cannot compact malformed history")
	}
	out := map[string]any{"body_pruned": true, "content_hash": fingerprint}
	for _, key := range keep {
		if v, ok := original[key]; ok {
			out[key] = v
		}
	}
	return string(canonical(out)), nil
}

func (s *Store) compactEvent(ctx context.Context, id string, cutoff, now int64, stats *CleanupStats) error {
	tx, _, err := s.eventWrite(ctx, id)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var event eventRecord
	if err = tx.Session(ctx).Where("event_id = ?", id).Take(&event).Error; err != nil {
		return err
	}
	if event.State != "accepted" || event.AcceptedAt == 0 || event.AcceptedAt >= cutoff {
		return nil
	}
	var triages []triageRecord
	var reports []resultRecord
	if err = tx.Session(ctx).Where("event_id = ?", id).Find(&triages).Error; err != nil {
		return err
	}
	if err = tx.Session(ctx).Where("event_id = ?", id).Find(&reports).Error; err != nil {
		return err
	}
	if len(triages)+len(reports) == 0 {
		return nil
	}
	for _, t := range triages {
		if t.CreatedAt >= cutoff {
			return nil
		}
	}
	for _, r := range reports {
		if r.CreatedAt >= cutoff {
			return nil
		}
	}
	var changed CleanupStats
	for _, t := range triages {
		if t.BodyPruned {
			continue
		}
		fingerprint := t.BodyHash
		if fingerprint == "" {
			fingerprint = hash([]byte(t.Body))
		}
		body, err := compactBody(t.Body, fingerprint, "event_id", "outcome", "message_id", "target", "executor", "superseded_by", "report_id")
		if err != nil {
			return err
		}
		if !stats.DryRun {
			if err = tx.Session(ctx).Model(&triageRecord{}).Where("event_id = ?", id).Updates(map[string]any{"body": body, "body_hash": fingerprint, "body_pruned": true}).Error; err != nil {
				return err
			}
		}
		changed.Triages++
	}
	for _, r := range reports {
		if r.BodyPruned {
			continue
		}
		original, err := reportBody([]byte(r.Body))
		if err != nil {
			return err
		}
		fingerprint := r.BodyHash
		if fingerprint == "" {
			fingerprint = hash(original)
		}
		body, err := compactBody(string(original), fingerprint, "event_id", "evidence_version", "guide_version", "raft_task_message_id", "raft_report_message_id", "outcome", "executor")
		if err != nil {
			return err
		}
		if !stats.DryRun {
			if err = tx.Session(ctx).Model(&resultRecord{}).Where("event_id = ? AND report_id = ?", id, r.ReportID).Updates(map[string]any{"body": body, "body_hash": fingerprint, "body_pruned": true}).Error; err != nil {
				return err
			}
		}
		changed.Reports++
	}
	if event.CompactedAt == 0 {
		payload, err := compactBody(event.Payload, hash([]byte(event.Payload)), "event_id", "resource_key", "version", "previous_version", "first_observation", "data_classification")
		if err != nil {
			return err
		}
		if !stats.DryRun {
			if err = tx.Session(ctx).Model(&eventRecord{}).Where("event_id = ?", id).Updates(map[string]any{"payload": payload, "compacted_at": now}).Error; err != nil {
				return err
			}
		}
		changed.Events++
	}
	if !stats.DryRun {
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	stats.Events += changed.Events
	stats.Reports += changed.Reports
	stats.Triages += changed.Triages
	return nil
}

func (s *Store) compactLegacy(ctx context.Context, cutoff int64, stats *CleanupStats) error {
	for after := ""; ; {
		var rows []legacyRecord
		if err := s.db.Session(ctx).Where("marker > ? AND created_at > 0 AND created_at < ? AND body_pruned = ?", after, cutoff, false).Order("marker").Limit(100).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			body, err := compactBody(row.Body, hash([]byte(row.Body)), "marker", "watch", "outcome", "message", "target")
			if err != nil {
				return err
			}
			if !stats.DryRun {
				r := s.db.Session(ctx).Model(&legacyRecord{}).Where("marker = ? AND body_pruned = ?", row.Marker, false).Updates(map[string]any{"body": body, "body_pruned": true})
				if r.Error != nil {
					return r.Error
				}
				stats.LegacyEntries += r.RowsAffected
			} else {
				stats.LegacyEntries++
			}
			after = row.Marker
		}
		if len(rows) < 100 {
			return nil
		}
	}
}
