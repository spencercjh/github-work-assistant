package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github-raft-app/internal/database"
)

var ErrWebhookConflict = errors.New("webhook delivery ID already has different content")

type WebhookDelivery struct {
	ID       string   `json:"delivery_id"`
	Hash     string   `json:"body_hash"`
	Event    string   `json:"event"`
	Keys     []string `json:"resources"`
	Attempts int      `json:"attempts"`
}

// A duplicate never resets a retry deadline or reopens a completed delivery.
func (s *Store) AcceptWebhook(ctx context.Context, d WebhookDelivery) (bool, error) {
	tx, err := s.db.BeginWrite(ctx, "webhook-inbox")
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var old webhookRecord
	err = database.RecordError(tx.Session(ctx).Where("delivery_id = ?", d.ID).Take(&old).Error)
	if err == nil {
		if old.BodyHash != d.Hash || old.EventType != d.Event {
			return false, ErrWebhookConflict
		}
		return false, tx.Commit()
	}
	if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	now := time.Now().Unix()
	row := webhookRecord{DeliveryID: d.ID, BodyHash: d.Hash, EventType: d.Event, PendingKeys: string(canonical(d.Keys)), ReceivedAt: now, NextAttempt: now + 2}
	if len(d.Keys) == 0 {
		row.FinishedAt = now
	}
	if err = tx.Session(ctx).Create(&row).Error; err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) PendingWebhooks(ctx context.Context, now int64) ([]WebhookDelivery, error) {
	var rows []webhookRecord
	if err := s.db.Session(ctx).Where("finished_at = 0 AND next_attempt <= ?", now).Order("received_at, delivery_id").Limit(100).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]WebhookDelivery, 0, len(rows))
	for _, row := range rows {
		d := WebhookDelivery{ID: row.DeliveryID, Hash: row.BodyHash, Event: row.EventType, Attempts: row.Attempts}
		if err := json.Unmarshal([]byte(row.PendingKeys), &d.Keys); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (s *Store) FinishWebhook(ctx context.Context, d WebhookDelivery, pending []string, failure string, retryAt int64) error {
	tx, err := s.db.BeginWrite(ctx, "webhook-inbox")
	if err != nil {
		return err
	}
	defer tx.Rollback()
	finished := int64(0)
	if len(pending) == 0 {
		finished, retryAt, failure = time.Now().Unix(), 0, ""
	}
	result := tx.Session(ctx).Model(&webhookRecord{}).
		Where("delivery_id = ? AND body_hash = ? AND attempts = ? AND finished_at = 0", d.ID, d.Hash, d.Attempts).
		Updates(map[string]any{"pending_keys": string(canonical(pending)), "attempts": d.Attempts + 1, "next_attempt": retryAt, "last_error": failure, "finished_at": finished})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrStaleWork
	}
	return tx.Commit()
}
