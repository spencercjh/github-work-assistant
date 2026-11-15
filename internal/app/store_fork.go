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
	syncQueueProjection(&p)
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
	next, err := upgradeForkReviewWait(ctx, tx, spec.Key(), p)
	if err != nil {
		return err
	}
	next = planFork(spec, next, o, time.Now().Unix())
	if !reflect.DeepEqual(next, p) {
		next.Revision++
		if err = writeForkProgress(ctx, tx, spec.Key(), next); err != nil {
			return err
		}
	}
	if o.PR > 0 {
		if err = associateForkPR(ctx, tx, spec, o.PR); err != nil {
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

// Earlier changes-ready reports scheduled hourly retries just like unfinished
// work. Recover only an unambiguous, current report and its original deadline.
// This changes scheduling, never the evidence, test proof or completion cursor.
func upgradeForkReviewWait(ctx context.Context, tx *database.Tx, key string, p ForkProgress) (ForkProgress, error) {
	if p.Phase != "waiting" || p.NextAttempt == 0 || p.PR == 0 {
		return p, nil
	}
	var latest []resultRecord
	err := tx.Session(ctx).Table("ghraft_results AS r").Select("r.*").
		Joins("JOIN ghraft_events e ON e.event_id=r.event_id").Where("e.resource_key=?", key).
		Order("r.created_at DESC").Limit(2).Find(&latest).Error
	if err != nil || len(latest) == 0 {
		return p, err
	}
	r := latest[0]
	// Timestamps have second precision. Do not guess which simultaneous report
	// is authoritative, or reinterpret a later explicit waiting deadline.
	delay := r.CreatedAt + int64(time.Hour.Seconds()) - p.NextAttempt
	if (len(latest) > 1 && latest[1].CreatedAt == r.CreatedAt) || delay < 0 || delay > 1 {
		return p, nil
	}
	var result WorkResult
	if json.Unmarshal([]byte(r.Body), &result) != nil || result.Outcome != "changes-ready" || result.EventID != r.EventID || result.ReportID != r.ReportID || result.GuideVersion != p.ValidatedGuide {
		return p, nil
	}
	var link workLinkRecord
	err = tx.QueryRowContext(ctx, `SELECT task_id,event_id,evidence_version,guide_version FROM ghraft_work_links WHERE resource_key=?`, key).Scan(&link.TaskID, &link.EventID, &link.EvidenceVersion, &link.GuideVersion)
	if errors.Is(err, ErrNotFound) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if link.EventID != result.EventID || link.TaskID != result.TaskID || link.GuideVersion != result.GuideVersion || link.EvidenceVersion != result.EvidenceVersion {
		return p, nil
	}
	var body string
	err = tx.QueryRowContext(ctx, `SELECT s.body FROM ghraft_snapshots s JOIN ghraft_events e ON e.resource_key=s.resource_key AND e.version_hash=s.version_hash WHERE s.resource_key=? AND e.event_id=? AND e.version_hash=? AND e.state='accepted'`, key, result.EventID, result.EvidenceVersion).Scan(&body)
	if errors.Is(err, ErrNotFound) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	var snapshot Snapshot
	if json.Unmarshal([]byte(body), &snapshot) != nil || snapshot.Objects["fork"]["round"] != p.Round || snapshot.Objects["fork"]["target_sha"] != p.ValidatedBase || snapshot.Objects["pull"]["state"] != "open" || snapshot.Objects["pull"]["head"] != p.ValidatedHead || !validForkProofHead(p) || !shaRE.MatchString(p.ValidatedTree) {
		return p, nil
	}
	p.Phase, p.NextAttempt = "awaiting-review", 0
	return p, nil
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
	// Heartbeats intentionally do not advance the semantic revision. Preserve
	// the latest heartbeat when saving a progress copy read before that tick.
	if p.Validation != nil && next.Validation != nil && p.Validation.ID == next.Validation.ID && p.Validation.HeartbeatAt > next.Validation.HeartbeatAt {
		v := *next.Validation
		v.HeartbeatAt = p.Validation.HeartbeatAt
		next.Validation = &v
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
		if err = saveProjectWorkStatus(ctx, tx, key, next, *result); err != nil {
			return err
		}
		if next.Validation != nil && next.Validation.State != "running" && next.Validation.HandledAt == 0 {
			v := *next.Validation
			v.HandledAt = time.Now().Unix()
			next.Validation = &v
		}
	} else if expectedVersion != "" {
		// Only this still-live attempt can complete; heartbeat expiration and
		// replacement runs fence late writers independently of snapshot checks.
		v := p.Validation
		if v == nil || v.State != "running" || next.Validation == nil || next.Validation.ID != v.ID || v.Round != next.Round || v.Head != next.ValidatedHead || v.Base != next.ValidatedBase || v.Guide != next.ValidatedGuide || v.Log != next.TestLog || time.Now().Unix() >= v.Deadline || time.Now().Unix() >= v.HeartbeatAt+int64(forkTestLease.Seconds()) {
			return ErrStaleWork
		}
		completed := *v
		completed.State, completed.FinishedAt = "passed", time.Now().Unix()
		next.Validation = &completed
		next.NextAttempt = completed.FinishedAt + int64(forkTestFollowup.Seconds())
	}
	next.Revision = revision + 1
	if err = writeForkProgress(ctx, tx, key, next); err != nil {
		return err
	}
	return tx.Commit()
}

func associateForkPR(ctx context.Context, tx *database.Tx, spec ForkSpec, pr int) error {
	name := forkPRName(Watch{spec.Target.Repo, pr})
	var owner string
	err := tx.QueryRowContext(ctx, `SELECT value FROM ghraft_meta WHERE name=?`, name).Scan(&owner)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if owner != "" && owner != spec.Key() {
		return errors.New("sync PR already belongs to another maintenance rule")
	}
	return tx.Upsert(ctx, &metaRecord{name, spec.Key()}, []string{"name"}, nil)
}
