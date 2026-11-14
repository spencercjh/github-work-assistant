package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

const adaptedBatches = "adapted-batches"

type ForkBatchInput struct {
	WorkInput
	After  string `json:"after"`
	Count  int    `json:"count"`
	Reason string `json:"reason"`
}

func validForkProofHead(p ForkProgress) bool {
	if !shaRE.MatchString(p.ValidatedHead) {
		return false
	}
	if p.SyncMode == adaptedBatches {
		return shaRE.MatchString(p.BatchAfter) && len(p.BatchCommits) > 0 && p.BatchCommits[len(p.BatchCommits)-1] == p.Upstream && p.ValidatedHead != p.InitialTarget
	}
	return p.SyncMode == "" && p.ValidatedHead == p.Upstream
}

// Select the oldest next mainline changes. First-parent order preserves merge
// dependencies; timestamps are not an ordering or evidence of incorporation.
func nextForkBatch(ctx context.Context, repo *forkGitRepository, after, latest string, count int) ([]string, error) {
	if !shaRE.MatchString(after) || !shaRE.MatchString(latest) || count < 1 || count > 50 {
		return nil, errors.New("batch requires a full baseline SHA and count in 1..50")
	}
	text, err := repo.run(ctx, "rev-list", "--first-parent", latest)
	if err != nil {
		return nil, errors.New("cannot read upstream mainline history")
	}
	pending := []string{}
	found := false
	for _, sha := range strings.Fields(text) {
		if sha == after {
			found = true
			break
		}
		pending = append(pending, sha)
	}
	if !found {
		return nil, errors.New("baseline is not on the current upstream first-parent history; reconcile rewritten or divergent history")
	}
	if len(pending) == 0 {
		return nil, errors.New("no upstream mainline changes after this cursor")
	}
	if count > len(pending) {
		count = len(pending)
	}
	batch := make([]string, count)
	for i := range count {
		batch[i] = pending[len(pending)-1-i]
	}
	return batch, nil
}

// An explicit executor operation starts a bounded adaptation batch. The first
// baseline is documented scope, never a fabricated successful synchronization.
// A published unfinished round cannot be silently replaced.
func (s *Service) StartForkBatch(ctx context.Context, in ForkBatchInput) (ForkProgress, error) {
	if strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 4000 || in.Count < 1 || in.Count > 50 {
		return ForkProgress{}, errors.New("a batch requires a rationale (1..4000 bytes) and count in 1..50")
	}
	if _, err := s.RefreshWorkContext(ctx, in.EventID); err != nil {
		return ForkProgress{}, err
	}
	w, err := s.checkWork(ctx, in.WorkInput, "coordinate")
	if err != nil {
		return ForkProgress{}, err
	}
	f, ok := s.Config.Fork(w.Event.Key)
	if !ok || w.Fork == nil || w.Guide.ForkSyncMode != adaptedBatches || w.Guide.Mode != "local-edit" || f.LocalRepository == "" || w.Task == nil || w.Task.TaskID != in.TaskID || w.Task.GuideVersion != in.GuideVersion || w.Task.EvidenceVersion != in.EvidenceVersion {
		return ForkProgress{}, ErrWorkDenied
	}
	if f.Queue != nil {
		return s.startQueuedForkBatch(ctx, in, w, f)
	}
	p := *w.Fork
	var snapshot Snapshot
	if json.Unmarshal(w.Snapshot, &snapshot) != nil {
		return p, ErrStaleWork
	}
	facts, pull := snapshot.Objects["fork"], snapshot.Objects["pull"]
	retired := p.Phase == "restart" && pull["state"] == "closed" && pull["merged"] == false
	if p.Phase != "synced" && !retired && (p.PR != 0 || str(facts["remote_head"]) != "") {
		return p, errors.New("finish or explicitly retire the published round before selecting another batch")
	}
	after := p.LastSuccess
	if after == "" && p.SyncMode == adaptedBatches {
		after = p.BatchAfter // Retrying an unpublished batch must not skip it.
	}
	if after == "" {
		after = in.After
	} else if in.After != "" && in.After != after {
		return p, errors.New("batch must resume after the verified cursor, or retry the same unfinished baseline")
	}
	if p.Phase == "synced" && p.SyncMode == adaptedBatches && (pull["merged"] != true || facts["merge_verified"] != true || !validForkProofHead(p)) {
		return p, errors.New("previous batch merge no longer matches its proof")
	}
	repo, err := openForkGitRepository(ctx, f)
	if err != nil {
		return p, err
	}
	defer repo.close()
	if err = repo.ensureCommit(ctx, f.Upstream.Repo, p.ObservedUpstream); err != nil {
		return p, err
	}
	batch, err := nextForkBatch(ctx, repo, after, p.ObservedUpstream, in.Count)
	if err != nil {
		return p, err
	}
	target := str(facts["target_sha"])
	if !shaRE.MatchString(target) {
		return p, ErrStaleWork
	}
	if err = repo.ensureCommit(ctx, f.Target.Repo, target); err != nil {
		return p, err
	}
	end := batch[len(batch)-1]
	round := hash([]byte(fmt.Sprintf("%s:%s:%s:%s:%d", f.Key(), after, end, target, p.Revision+1)))
	baseline := p.AdaptationBaseline
	if baseline == "" {
		baseline = after
	}
	next := ForkProgress{AdaptationBaseline: baseline, PrivateRepos: p.PrivateRepos, SpecVersion: p.SpecVersion, SyncMode: adaptedBatches,
		Round: round, Phase: "working", BatchAfter: after, BatchCommits: batch, BatchReason: in.Reason,
		Upstream: end, InitialTarget: target, ObservedUpstream: p.ObservedUpstream,
		Branch: "ghraft/sync/" + f.ID + "/" + round[:12], LastSuccess: p.LastSuccess,
		NextAttempt: time.Now().Add(4 * time.Hour).Unix()}
	contains, err := repo.contains(ctx, end, target)
	if err != nil {
		return p, err
	}
	o := ForkObservation{Classification: snapshot.Classification, Upstream: p.ObservedUpstream, Target: target, ContainsPinned: contains, Objects: Objects{}}
	// Recheck current ownership, policy and the input snapshot after local work.
	fresh, err := s.checkWork(ctx, in.WorkInput, "coordinate")
	if err != nil {
		return p, err
	}
	if fresh.Fork == nil || fresh.Fork.Revision != p.Revision {
		return p, ErrStaleWork
	}
	if err = s.Store.StartForkBatch(ctx, f, p, next, o, in.EvidenceVersion); err != nil {
		return p, err
	}
	return s.Store.ForkProgress(ctx, f.Key())
}

// The round transition and new event commit atomically; discussion identity,
// owner, old workspace and report history are deliberately preserved.
func (s *Store) StartForkBatch(ctx context.Context, f ForkSpec, expected, next ForkProgress, o ForkObservation, version string) error {
	tx, err := s.db.BeginWrite(ctx, "resource:"+f.Key())
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := readForkProgress(ctx, tx, f.Key())
	if err != nil {
		return err
	}
	var current string
	if err = tx.QueryRowContext(ctx, `SELECT version_hash FROM ghraft_snapshots WHERE resource_key=?`, f.Key()).Scan(&current); err != nil {
		return err
	}
	if p.Revision != expected.Revision || p.Round != expected.Round || p.SpecVersion != next.SpecVersion || (version != "" && version != current) {
		return ErrStaleWork
	}
	if p.Queue != nil {
		for _, b := range p.Queue.Batches {
			i := queueIndex(next, b.Progress.BatchID)
			if i >= 0 && next.Queue.Batches[i].Progress.Round == b.Progress.Round {
				continue
			}
			// Archive retired generations only, atomically with replacement.
			if err = tx.Upsert(ctx, &metaRecord{queueArchiveName(f.Key(), b.Progress.Round), string(queueArchive(b))}, []string{"name"}, []string{"value"}); err != nil {
				return err
			}
		}
	}
	if next.Queue != nil {
		if i := queueIndex(next, next.BatchID); i >= 0 {
			next.Queue.Batches[i].Observation = o
		}
		syncQueueProjection(&next)
		for _, b := range next.Queue.Batches {
			if b.Observation.PR > 0 {
				if err = associateForkPR(ctx, tx, f, b.Observation.PR); err != nil {
					return err
				}
			}
		}
	}
	// Collector refreshes must not create semantic revisions on every poll or
	// overwrite a newer lease heartbeat.
	if p.Validation != nil && next.Validation != nil && p.Validation.ID == next.Validation.ID && p.Validation.HeartbeatAt > next.Validation.HeartbeatAt {
		next.Validation.HeartbeatAt = p.Validation.HeartbeatAt
	}
	syncQueueProjection(&next)
	next.Revision = p.Revision
	if !reflect.DeepEqual(p, next) {
		next.Revision++
	}
	if err = writeForkProgress(ctx, tx, f.Key(), next); err != nil {
		return err
	}
	if _, err = s.observeTx(ctx, tx, f.Key(), forkSnapshot(f, next, o)); err != nil {
		return err
	}
	return tx.Commit()
}
