package app

import (
	"context"
	"encoding/json"
	"errors"
)

// Only RecoverForkProof may call this after validating the operator-pinned
// archive and live merge. The receipt and proof commit together; no result or
// cursor is written here. A replay cannot overwrite its original audit record.
func (s *Store) RestoreHistoricalForkProof(ctx context.Context, receipt ForkProofRecoveryReceipt) (ForkProofRecoveryReceipt, error) {
	var empty ForkProofRecoveryReceipt
	tx, err := s.db.BeginWrite(ctx, "resource:"+receipt.Resource)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	p, err := readForkProgress(ctx, tx, receipt.Resource)
	if err != nil {
		return empty, err
	}
	if p.Revision != receipt.Previous.Revision || p.Round != receipt.Archived.Round || p.SpecVersion != receipt.Archived.SpecVersion || p.Phase == "synced" || p.Phase == "restart" || (p.Validation != nil && p.Validation.State == "running") {
		return empty, ErrStaleWork
	}
	var version string
	if err = tx.QueryRowContext(ctx, `SELECT version_hash FROM ghraft_snapshots WHERE resource_key=?`, receipt.Resource).Scan(&version); err != nil {
		return empty, err
	}
	if version != receipt.Input.EvidenceVersion {
		return empty, ErrStaleWork
	}
	name := "fork-proof-recovery:" + receipt.ID
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT value FROM ghraft_meta WHERE name=?`, name).Scan(&raw)
	if err == nil {
		var saved ForkProofRecoveryReceipt
		if json.Unmarshal([]byte(raw), &saved) != nil || saved.Resource != receipt.Resource || saved.Archived.Round != p.Round || !sameForkProof(p, saved.Archived) {
			return empty, errors.New("recovery was already used; restored proof has since changed")
		}
		return saved, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return empty, err
	}
	next := restoreForkProof(p, receipt.Archived)
	// RecoverForkProof has verified this head's original proof against its actual
	// merge. Clear only its execution hold; dependent batches and the real latest
	// validation attempt remain unchanged. The receipt retains the prior hold.
	next.QueueStale = ""
	next.Revision++
	if err = writeForkProgress(ctx, tx, receipt.Resource, next); err != nil {
		return empty, err
	}
	if err = tx.Session(ctx).Create(&metaRecord{Name: name, Value: string(canonical(receipt))}).Error; err != nil {
		return empty, err
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	return receipt, nil
}
