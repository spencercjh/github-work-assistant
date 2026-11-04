package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
)

// TickFork drives durable local deadlines without a GitHub poll. It neither
// invents remote observations nor extends evidence freshness or test validity.
func (s *Store) TickFork(ctx context.Context, spec ForkSpec, now int64) (bool, error) {
	tx, err := s.db.BeginWrite(ctx, "resource:"+spec.Key())
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	p, err := readForkProgress(ctx, tx, spec.Key())
	if err != nil {
		return false, err
	}
	if p.Round == "" || p.SpecVersion != spec.Version() || p.Phase == "synced" || p.Phase == "restart" {
		return false, nil
	}
	next, err := upgradeForkReviewWait(ctx, tx, spec.Key(), p)
	if err != nil {
		return false, err
	}
	next = advanceForkTimers(next, now)
	if reflect.DeepEqual(p, next) {
		return false, nil
	}
	var stored snapshotRecord
	if err = tx.Session(ctx).Where("resource_key = ?", spec.Key()).Take(&stored).Error; err != nil {
		return false, err
	}
	var snapshot Snapshot
	if json.Unmarshal([]byte(stored.Body), &snapshot) != nil || snapshot.Objects["fork"] == nil || snapshot.Objects["fork"]["round"] != p.Round {
		return false, errors.New("fork timer requires its current source snapshot")
	}
	next.Revision++
	if err = writeForkProgress(ctx, tx, spec.Key(), next); err != nil {
		return false, err
	}
	snapshot.Objects["fork"]["wake"] = next.Wake
	if _, err = s.observeTxAt(ctx, tx, spec.Key(), canonical(snapshot), stored.ObservedAt); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
