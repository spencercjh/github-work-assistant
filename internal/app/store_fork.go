package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github-raft-app/internal/database"
)

func readForkProgress(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) database.Row
}, key string) (ForkProgress, error) {
	var p ForkProgress
	var raw string
	err := query.QueryRowContext(ctx, `SELECT value FROM ghraft_meta WHERE name=?`, forkProgressName(key)).Scan(&raw)
	if errors.Is(err, ErrNotFound) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	err = json.Unmarshal([]byte(raw), &p)
	return p, err
}
func (s *Store) ForkProgress(ctx context.Context, key string) (ForkProgress, error) {
	return readForkProgress(ctx, s.db, key)
}

func writeForkProgress(ctx context.Context, tx *database.Tx, key string, p ForkProgress) error {
	return tx.Upsert(ctx, &metaRecord{forkProgressName(key), string(canonical(p))}, []string{"name"}, []string{"value"})
}

// Progress, source snapshot, PR association and event outbox commit together.
// A concurrent collector or executor forces a refresh instead of losing its write.
func (s *Store) ObserveFork(ctx context.Context, spec ForkSpec, expected ForkProgress, o ForkObservation) error {
	tx, err := s.db.BeginWrite(ctx, "resource:"+spec.Key())
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := readForkProgress(ctx, tx, spec.Key())
	if err != nil {
		return err
	}
	if p.Revision != expected.Revision {
		return ErrStaleWork
	}
	next := planFork(spec, p, o, time.Now().Unix())
	if !reflect.DeepEqual(next, p) {
		next.Revision++
		if err = writeForkProgress(ctx, tx, spec.Key(), next); err != nil {
			return err
		}
	}
	if o.PR > 0 {
		name := forkPRName(Watch{spec.Target.Repo, o.PR})
		var owner string
		err = tx.QueryRowContext(ctx, `SELECT value FROM ghraft_meta WHERE name=?`, name).Scan(&owner)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if owner != "" && owner != spec.Key() {
			return errors.New("sync PR already belongs to another maintenance rule")
		}
		if err = tx.Upsert(ctx, &metaRecord{name, spec.Key()}, []string{"name"}, nil); err != nil {
			return err
		}
	}
	// An ended round's PR must not be attached to the next round's snapshot.
	if next.Round != p.Round {
		o.PR, o.Objects, o.SyncHead, o.MergeCommit = 0, nil, "", ""
		o.ContainsPinned, o.ContainsValidated, o.MergeVerified = o.ContainsLatest, false, false
	}
	body := forkSnapshot(spec, next, o)
	if p.Phase == "synced" && next.Phase == "synced" && next.Round == p.Round {
		body, err = completedForkSnapshot(ctx, tx, spec.Key(), body)
		if err != nil {
			return err
		}
	}
	if _, err = s.observeTx(ctx, tx, spec.Key(), body); err != nil {
		return err
	}
	return tx.Commit()
}

// Preserve completed evidence and report associations when only the collector's
// verification rules or progress-derived readiness changed. This preserves the
// report that completed the round. Writes remain forbidden; any later merged
// report independently fetches and checks the current remote proof.
func completedForkSnapshot(ctx context.Context, tx *database.Tx, key string, body []byte) ([]byte, error) {
	var previous string
	err := tx.QueryRowContext(ctx, `SELECT body FROM ghraft_snapshots WHERE resource_key=?`, key).Scan(&previous)
	if errors.Is(err, ErrNotFound) {
		return body, nil
	}
	if err != nil {
		return nil, err
	}
	var old, current Snapshot
	if json.Unmarshal([]byte(previous), &old) != nil || json.Unmarshal(body, &current) != nil || old.Objects["fork"] == nil || current.Objects["fork"] == nil {
		return body, nil
	}
	for _, name := range []string{"contains_validated", "merge_verified", "merge_commit_sha", "next_batch_ready"} {
		if value, exists := current.Objects["fork"][name]; exists {
			old.Objects["fork"][name] = value
		}
	}
	if bytes.Equal(canonical(old), body) {
		return []byte(previous), nil
	}
	return body, nil
}

func (s *Store) SaveForkProgress(ctx context.Context, key string, revision int64, next ForkProgress, result *WorkResult) error {
	version := ""
	if result != nil {
		version = result.EvidenceVersion
	}
	return s.saveForkProgress(ctx, key, revision, next, result, version)
}

// Bind the proof to the refreshed snapshot atomically, including changes which
// do not alter progress (for example a new PR comment or a moved target).
func (s *Store) SaveForkTestProof(ctx context.Context, key string, revision int64, next ForkProgress, version string) error {
	if !idRE.MatchString(version) {
		return ErrStaleWork
	}
	return s.saveForkProgress(ctx, key, revision, next, nil, version)
}

func (s *Store) saveForkProgress(ctx context.Context, key string, revision int64, next ForkProgress, result *WorkResult, expectedVersion string) error {
	tx, err := s.db.BeginWrite(ctx, "resource:"+key)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := readForkProgress(ctx, tx, key)
	if err != nil {
		return err
	}
	if p.Revision != revision || p.Round != next.Round || p.SpecVersion != next.SpecVersion {
		return ErrStaleWork
	}
	if result != nil || expectedVersion != "" {
		var version string
		if err = tx.QueryRowContext(ctx, `SELECT version_hash FROM ghraft_snapshots WHERE resource_key=?`, key).Scan(&version); err != nil {
			return err
		}
		if expectedVersion != version {
			return ErrStaleWork
		}
	}
	if result != nil {
		if err = saveReport(ctx, tx, result.EventID, result.ReportID, canonical(result)); err != nil {
			return err
		}
	}
	next.Revision = revision + 1
	if err = writeForkProgress(ctx, tx, key, next); err != nil {
		return err
	}
	return tx.Commit()
}
