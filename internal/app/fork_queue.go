package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// A nil queue keeps the serial protocol. Queue depth and test concurrency are
// separate: the original owner still operates one selected workspace at a time.
type ForkQueueConfig struct {
	MaxOpenPRs int `json:"max_open_prs,omitempty"`
}

func (f ForkSpec) queueLimit() int {
	if f.Queue == nil {
		return 0
	}
	if f.Queue.MaxOpenPRs == 0 {
		return 10
	}
	return f.Queue.MaxOpenPRs
}

type ForkQueueState struct {
	PreparedThrough string            `json:"prepared_through,omitempty"`
	MergedThrough   string            `json:"merged_through,omitempty"`
	MergedTarget    string            `json:"merged_target,omitempty"`
	Batches         []ForkQueuedBatch `json:"batches"`
}
type ForkQueuedBatch struct {
	// Progress never has a nested Queue. The top-level progress is a selected
	// projection; each record retains its own proof and latest remote evidence.
	Progress    ForkProgress    `json:"progress"`
	Observation ForkObservation `json:"observation"`
}
type ForkQueueInput struct {
	WorkInput
	Batch string `json:"batch"`
}

const forkQueueInstruction = `A bounded stacked PR queue is enabled. Keep the original maintenance task and owner. The queue's prepared_through is not merged progress. After every batch passes required tests and is reported changes-ready (awaiting-review), continue start-sync-batch while capacity remains; for a human merge choice alone, report needs-decision with review_only=true after asking the decision maker (all changes-ready gates still apply); never mark a correctness, conflict or test failure review-only; select-sync-batch --batch <batch_id> resumes an earlier batch. Inspect all queue entries before choosing work. Failed tests, unresolved correctness issues, conflicts, closed PRs and stale dependencies stop the affected chain. Only the first unfinished batch may merge, only into the configured integration branch; NEVER merge a descendant PR into its predecessor branch. After predecessor or integration changes, select affected batches oldest-first and run rebuild-sync-batch, then prepare-worktree in its new generation. Replay the SAME source range, preserve upstream authors, sign commits according to repository policy, refresh the mapping and run the complete validation. Re-read old work and the returned rebuild source before replay; do not reset existing workspaces. A rebuild may replace only its captured remote head with an explicit lease and may retarget only the exact current PR through the separately delegated retarget-pr action. Refresh CI and obtain a new independent review for the tested head AND target; old approval does not survive a changed base. Close/rejection never advances or skips a source range. Queue mode grants no merge, publication, deployment, workflow or credential permission. Keep test-only CI separate from publishing, and route enterprise fixes into the integration branch through explicit deduplicated PRs; no automatic reverse synchronization.`

func cloneForkProgress(p ForkProgress) ForkProgress {
	var result ForkProgress
	_ = json.Unmarshal(canonical(p), &result)
	return result
}
func unqueuedProgress(p ForkProgress) ForkProgress { p.Queue = nil; p.Revision = 0; return p }
func queueIndex(p ForkProgress, id string) int {
	if p.Queue == nil {
		return -1
	}
	return slices.IndexFunc(p.Queue.Batches, func(b ForkQueuedBatch) bool { return b.Progress.BatchID == id })
}
func syncQueueProjection(p *ForkProgress) {
	if i := queueIndex(*p, p.BatchID); i >= 0 {
		p.Queue.Batches[i].Progress = unqueuedProgress(*p)
	}
}
func forkTargetBranch(f ForkSpec, p ForkProgress) string {
	if p.BatchID != "" && p.TargetBranch != "" {
		return p.TargetBranch
	}
	return f.Target.Branch
}
func queueHead(p ForkProgress) string {
	if p.Queue == nil {
		return ""
	}
	for _, b := range p.Queue.Batches {
		if b.Progress.Phase != "synced" {
			return b.Progress.BatchID
		}
	}
	return ""
}
func queueUnfinished(p ForkProgress) int {
	n := 0
	if p.Queue != nil {
		for _, b := range p.Queue.Batches {
			if b.Progress.Phase != "synced" {
				n++
			}
		}
	}
	return n
}
func queueValidationRunning(p ForkProgress) bool {
	if p.Validation != nil && p.Validation.State == "running" {
		return true
	}
	if p.Queue != nil {
		for _, b := range p.Queue.Batches {
			if b.Progress.Validation != nil && b.Progress.Validation.State == "running" {
				return true
			}
		}
	}
	return false
}

func queueRemoteFailure(objects Objects) bool {
	failed := func(row map[string]any) bool {
		for _, name := range []string{"conclusion", "status", "state"} {
			if slices.Contains([]string{"failure", "error", "cancelled", "timed_out", "action_required", "startup_failure", "stale"}, str(row[name])) {
				return true
			}
		}
		return false
	}
	type status struct {
		created time.Time
		id      uint64
		row     map[string]any
	}
	latest := map[string]status{}
	for key, row := range objects {
		if strings.HasPrefix(key, "check:") && failed(row) {
			return true
		}
		if !strings.HasPrefix(key, "status:") {
			continue
		}
		// The statuses API returns history. Snapshots retain it for audit, but
		// readiness follows only the newest status per context. Numeric IDs break
		// ties when multiple updates arrive within the same timestamp second.
		context := strings.ToLower(str(row["context"]))
		created, timeErr := time.Parse(time.RFC3339, str(row["created_at"]))
		id, idErr := strconv.ParseUint(strings.TrimPrefix(key, "status:"), 10, 64)
		if context == "" || timeErr != nil || idErr != nil {
			if failed(row) {
				return true // Unorderable evidence cannot conceal a failure.
			}
			continue
		}
		old, exists := latest[context]
		if !exists || created.After(old.created) || (created.Equal(old.created) && id > old.id) {
			latest[context] = status{created, id, row}
		}
	}
	for _, s := range latest {
		if failed(s.row) {
			return true
		}
	}
	return false
}
func queueReviewReady(b ForkQueuedBatch, guide string) bool {
	p, o := b.Progress, b.Observation
	pull := o.Objects["pull"]
	return p.QueueStale == "" && p.Phase == "awaiting-review" && p.Validation != nil && p.Validation.State == "passed" &&
		validForkProofHead(p) && shaRE.MatchString(p.ValidatedTree) && p.ValidatedGuide == guide && p.ValidatedBase == o.Target &&
		p.PR > 0 && pull["state"] == "open" && pull["merged"] == false && pull["head"] == p.ValidatedHead &&
		o.SyncHead == p.ValidatedHead && pull["base"] == p.TargetBranch && !queueRemoteFailure(o.Objects)
}

// Gates also run after selection, so selecting a later entry cannot evade an
// earlier conflict or failed validation. Completed predecessors remain auditable.
func queueWorkAllowed(p ForkProgress, guide string) error {
	if p.Queue == nil {
		return nil
	}
	if p.QueueStale != "" {
		return fmt.Errorf("batch requires rebuild: %s", p.QueueStale)
	}
	for _, b := range p.Queue.Batches {
		if b.Progress.BatchID == p.BatchID {
			return nil
		}
		if b.Progress.Phase != "synced" && !queueReviewReady(b, guide) {
			return errors.New("predecessor is not a tested review-ready batch; repair it before continuing this dependency chain")
		}
	}
	return ErrStaleWork
}

func queueSnapshot(p ForkProgress, objects Objects) {
	if p.Queue == nil {
		return
	}
	objects["fork-queue"] = map[string]any{"selected_batch": p.BatchID, "head_batch": queueHead(p), "prepared_through": p.Queue.PreparedThrough, "merged_through": p.Queue.MergedThrough, "unfinished": queueUnfinished(p)}
	for _, b := range p.Queue.Batches {
		x := b.Progress
		objects["queue:"+x.BatchID] = map[string]any{"batch_id": x.BatchID, "round": x.Round, "phase": x.Phase, "parent_batch": x.ParentBatch, "parent_head": x.ParentHead, "target_branch": x.TargetBranch, "source_after": x.BatchAfter, "source_end": x.Upstream, "head": b.Observation.SyncHead, "pr": x.PR, "stale": x.QueueStale, "created_at": x.CreatedAt, "ready_at": x.ReadyAt, "merged_at": x.MergedAt, "evidence": hash(canonical(b.Observation))}
	}
}

// Read all pending batches before a write. A missing response is an error, never
// evidence that a PR was deleted or permission to create a replacement.
func (s *Service) collectForkQueue(ctx context.Context, f ForkSpec, expected ForkProgress, source ForkSource) error {
	if f.Queue == nil {
		if queueUnfinished(expected) > 0 {
			return errors.New("queue has unfinished batches; restore its policy and finish them before disabling queue mode")
		}
		o, err := source.CollectFork(ctx, f, expected)
		if err != nil {
			return err
		}
		if !o.MergeVerified {
			return errors.New("verify the completed queue merge before disabling queue mode")
		}
		next := cloneForkProgress(expected)
		next.Queue = nil
		next.BatchID, next.ParentBatch, next.ParentHead, next.TargetBranch = "", "", "", ""
		next.IntegrationBase, next.QueueStale = "", ""
		return s.Store.StartForkBatch(ctx, f, expected, next, o, "")
	}
	next := cloneForkProgress(expected)
	syncQueueProjection(&next)
	var integration, upstream string
	var privacy ForkObservation
	observePrivacy := func(o ForkObservation) {
		if o.Classification == "private" {
			privacy.Classification = "private"
		}
		for _, repo := range o.PrivateRepos {
			if !slices.Contains(privacy.PrivateRepos, repo) {
				privacy.PrivateRepos = append(privacy.PrivateRepos, repo)
			}
		}
	}
	for i := range next.Queue.Batches {
		b := &next.Queue.Batches[i]
		// Completion was accepted only after exact merge/proof verification.
		// Keep that PR's evidence, rather than polling its comments and CI forever.
		// The live integration frontier is still checked below on every refresh.
		if completedQueueBatch(*b) {
			continue
		}
		requested := b.Progress
		// A merged/closed or replaced predecessor may have lost its old branch.
		// The child's declared target still identifies that retired generation
		// after the parent is rebuilt. Only observation falls back to integration;
		// the child remains stale until explicitly rebuilt and retargeted.
		if i > 0 && requested.ParentBatch != "" {
			parent := next.Queue.Batches[i-1]
			if parent.Progress.Phase == "synced" || parent.Observation.Objects["pull"]["state"] == "closed" || requested.TargetBranch != parent.Progress.Branch {
				requested.TargetBranch = f.Target.Branch
			}
		}
		o, err := source.CollectFork(ctx, f, requested)
		if err != nil {
			return err
		}
		observePrivacy(o)
		main := o.IntegrationTarget
		if main == "" {
			main = o.Target
		}
		if integration != "" && (integration != main || upstream != o.Upstream) {
			return ErrStaleWork
		}
		integration, upstream = main, o.Upstream
		p := planFork(f, b.Progress, o, time.Now().Unix())
		if p.Phase != "synced" {
			reason := ""
			if p.InitialTarget != o.Target && !o.MergeVerified {
				reason = "tested preparation base changed"
			}
			if p.IntegrationBase != main && !o.MergeVerified {
				reason = "integration branch changed; rebuild dependency chain"
			}
			if i > 0 && p.ParentBatch != "" {
				parent := next.Queue.Batches[i-1]
				if parent.Progress.BatchID != p.ParentBatch {
					return errors.New("queue dependency order is corrupt")
				}
				if parent.Progress.Phase == "synced" {
					reason = "predecessor merged; rebuild onto integration branch"
				} else if parent.Progress.QueueStale != "" || parent.Observation.SyncHead != p.ParentHead || p.TargetBranch != parent.Progress.Branch {
					reason = "predecessor revision changed"
				}
			}
			pull := o.Objects["pull"]
			if pull != nil && pull["base"] != p.TargetBranch && (p.RetargetFrom == "" || pull["base"] != p.RetargetFrom) {
				reason = "PR target changed"
			}
			if pull["merged"] == true && (p.BatchID != queueHead(next) || pull["base"] != f.Target.Branch) {
				reason = "PR merged outside queue order or integration branch"
			}
			// Validation pins the old remote head while preparing its next push.
			// This only permits the publication gap; force updates still require
			// the separate exact replacement lease in checkForkAction.
			if p.ValidatedHead != "" && o.SyncHead != "" && o.SyncHead != p.ValidatedHead && o.SyncHead != p.RebuildFromHead && o.SyncHead != p.PendingPublishFrom {
				reason = "published head differs from tested head"
			}
			if p.ValidatedHead != "" && o.SyncHead == p.ValidatedHead {
				p.RebuildFromHead = ""
				p.PendingPublishFrom = ""
			}
			if pull != nil && pull["base"] == p.TargetBranch {
				p.RetargetFrom = ""
			}
			if reason != "" {
				p.QueueStale = reason
				p.NextAttempt = 0
			}
		}
		b.Progress, b.Observation = unqueuedProgress(p), o
	}
	if integration == "" {
		// An entirely completed queue still discovers upstream/main changes and
		// checks repository access, without rediscovering any historical PR.
		o, err := source.CollectFork(ctx, f, ForkProgress{Upstream: next.Upstream})
		if err != nil {
			return err
		}
		observePrivacy(o)
		integration, upstream = o.IntegrationTarget, o.Upstream
		if integration == "" {
			integration = o.Target
		}
	}
	// Repository visibility is live even when PR evidence is historical. Copy
	// restrictions to every batch so selecting an accepted batch cannot restore
	// public classification or lose a private repository's access requirement.
	for i := range next.Queue.Batches {
		b := &next.Queue.Batches[i]
		for _, repo := range privacy.PrivateRepos {
			if !slices.Contains(b.Progress.PrivateRepos, repo) {
				b.Progress.PrivateRepos = append(b.Progress.PrivateRepos, repo)
			}
			if !slices.Contains(b.Observation.PrivateRepos, repo) {
				b.Observation.PrivateRepos = append(b.Observation.PrivateRepos, repo)
			}
		}
		if privacy.Classification == "private" || len(b.Progress.PrivateRepos) > 0 {
			b.Observation.Classification = "private"
		}
	}
	// The source cursor is valid only while its last accepted merge remains in
	// the integration history, including after its batch leaves the active window.
	if next.Queue.MergedThrough != "" {
		if !shaRE.MatchString(next.Queue.MergedTarget) {
			return errors.New("queue merge frontier has no verified target; reconcile its history")
		}
		repo, err := openForkGitRepository(ctx, f)
		if err != nil {
			return err
		}
		defer repo.close()
		for _, sha := range []string{next.Queue.MergedTarget, integration} {
			if err = repo.ensureCommit(ctx, f.Target.Repo, sha); err != nil {
				return err
			}
		}
		present, err := repo.contains(ctx, next.Queue.MergedTarget, integration)
		if err != nil {
			return err
		}
		if !present {
			return errors.New("integration history no longer contains the accepted queue frontier; reconcile before continuing")
		}
	}
	i := queueIndex(next, next.BatchID)
	if i < 0 {
		return ErrStaleWork
	}
	selected := next.Queue.Batches[i]
	if completedQueueBatch(selected) {
		// The selected historical PR remains auditable, while the context used
		// to prepare its successor reports the live repository revisions.
		selected.Progress.ObservedUpstream = upstream
		selected.Observation.IntegrationTarget = integration
		selected.Observation.Target = integration
		selected.Observation.Upstream = upstream
	}
	q := next.Queue
	next = selected.Progress
	next.Queue, next.Revision, next.LastSuccess = q, expected.Revision, q.MergedThrough
	// One CAS writes every batch, the selected snapshot and its notification.
	return s.Store.StartForkBatch(ctx, f, expected, next, selected.Observation, "")
}

func completedQueueBatch(b ForkQueuedBatch) bool {
	p, o := b.Progress, b.Observation
	return p.Phase == "synced" && p.MergedAt > 0 &&
		o.MergeVerified && shaRE.MatchString(o.MergeCommit)
}

func (s *Service) queueContext(ctx context.Context, in WorkInput) (WorkContext, ForkSpec, error) {
	if _, err := s.RefreshWorkContext(ctx, in.EventID); err != nil {
		return WorkContext{}, ForkSpec{}, err
	}
	w, err := s.checkWork(ctx, in, "coordinate")
	if err != nil {
		return w, ForkSpec{}, err
	}
	f, ok := s.Config.Fork(w.Event.Key)
	if !ok || f.Queue == nil || w.Fork == nil || w.Guide.ForkSyncMode != adaptedBatches {
		return w, f, ErrWorkDenied
	}
	if err = requireForkContext(w, in); err != nil {
		return w, f, err
	}
	if queueValidationRunning(*w.Fork) {
		return w, f, errors.New("a queue validation is still running; do not switch or rebuild its workspace")
	}
	return w, f, nil
}

func (s *Service) SelectForkBatch(ctx context.Context, in ForkQueueInput) (ForkProgress, error) {
	w, f, err := s.queueContext(ctx, in.WorkInput)
	if err != nil {
		return ForkProgress{}, err
	}
	p := cloneForkProgress(*w.Fork)
	i := queueIndex(p, in.Batch)
	if i < 0 {
		return p, errors.New("batch is not in the current queue")
	}
	if p.BatchID == in.Batch {
		return p, nil
	}
	b := p.Queue.Batches[i]
	next := b.Progress
	next.Queue, next.Revision, next.LastSuccess = p.Queue, p.Revision, p.Queue.MergedThrough
	if err = s.Store.StartForkBatch(ctx, f, p, next, b.Observation, in.EvidenceVersion); err != nil {
		return p, err
	}
	return s.Store.ForkProgress(ctx, f.Key())
}

// A generation changes the local workspace/proof identity, never the source
// range. The original owner replays the port and signs commits in the new
// workspace. Existing dirty worktrees and remote branches are not reset here.
func (s *Service) RebuildForkBatch(ctx context.Context, in ForkQueueInput) (ForkProgress, error) {
	w, f, err := s.queueContext(ctx, in.WorkInput)
	if err != nil {
		return ForkProgress{}, err
	}
	p := cloneForkProgress(*w.Fork)
	if in.Batch != p.BatchID {
		return p, errors.New("select the batch before rebuilding it")
	}
	i := queueIndex(p, p.BatchID)
	if i < 0 || p.Phase == "synced" {
		return p, ErrWorkDenied
	}
	b := p.Queue.Batches[i]
	if b.Observation.Objects["pull"]["merged"] == true {
		return p, errors.New("merged batch requires audited reconciliation, not a rebuild")
	}
	if p.QueueStale == "" && b.Observation.Objects["pull"]["state"] != "closed" {
		return p, errors.New("batch has no stale dependency or closed PR to rebuild")
	}
	target := b.Observation.IntegrationTarget
	if target == "" {
		target = b.Observation.Target
	}
	branch, parentID, parentHead := f.Target.Branch, "", ""
	if i > 0 {
		parent := p.Queue.Batches[i-1]
		if parent.Progress.Phase != "synced" {
			if !queueReviewReady(parent, w.Guide.Version) {
				return p, errors.New("repair and validate the predecessor before rebuilding its dependent")
			}
			target, branch, parentID, parentHead = parent.Progress.ValidatedHead, parent.Progress.Branch, parent.Progress.BatchID, parent.Progress.ValidatedHead
		}
	}
	if !shaRE.MatchString(target) {
		return p, ErrStaleWork
	}
	next := p
	next.Round = hash([]byte(fmt.Sprintf("%s:rebuild:%s:%d", p.Round, target, p.Revision+1)))
	next.InitialTarget, next.TargetBranch, next.ParentBatch, next.ParentHead = target, branch, parentID, parentHead
	next.IntegrationBase = b.Observation.IntegrationTarget
	next.RebuildSourceHead, next.RebuildFromHead, next.RebuildFromBase = p.ValidatedHead, b.Observation.SyncHead, p.InitialTarget
	next.PendingPublishFrom = ""
	next.RetargetFrom = ""
	if old := str(b.Observation.Objects["pull"]["base"]); old != "" && old != branch {
		next.RetargetFrom = old
	}
	next.QueueStale, next.Phase, next.ReadyAt = "", "working", 0
	next.Validation = nil
	next.GenerationAt = time.Now().Unix()
	next.ValidatedHead, next.ValidatedBase, next.ValidatedTree, next.ValidatedGuide = "", "", "", ""
	next.ValidatedMapping, next.ValidatedPRSection, next.TestLog, next.TestLogHash = "", "", "", ""
	next.NextAttempt = time.Now().Add(4 * time.Hour).Unix()
	o := b.Observation
	o.Target = target
	if o.Objects["pull"]["state"] == "closed" {
		next.Branch = "ghraft/sync/" + f.ID + "/" + next.Round[:12]
		next.PR, next.RebuildFromHead = 0, ""
		o.PR, o.SyncHead, o.Objects = 0, "", Objects{}
		next.RetargetFrom = ""
	}
	// Until retarget-pr is completed, the old PR base is retained in observation;
	// publication uses the exact captured lease and retarget has its own gate.
	next.Queue.Batches[i] = ForkQueuedBatch{Progress: unqueuedProgress(next), Observation: o}
	if err = s.Store.StartForkBatch(ctx, f, p, next, o, in.EvidenceVersion); err != nil {
		return p, err
	}
	return s.Store.ForkProgress(ctx, f.Key())
}

func (s *Service) startQueuedForkBatch(ctx context.Context, in ForkBatchInput, w WorkContext, f ForkSpec) (ForkProgress, error) {
	p := cloneForkProgress(*w.Fork)
	if queueValidationRunning(p) {
		return p, errors.New("validation is running; wait for its saved result")
	}
	var snap Snapshot
	if json.Unmarshal(w.Snapshot, &snap) != nil {
		return p, ErrStaleWork
	}
	facts := snap.Objects["fork"]
	integration := str(facts["integration_target_sha"])
	if integration == "" {
		integration = str(facts["target_sha"])
	}
	q := p.Queue
	if q == nil {
		if p.Phase != "synced" && (p.PR != 0 || str(facts["remote_head"]) != "") {
			return p, errors.New("finish the published serial round before initializing the queue")
		}
		if p.Phase == "synced" && p.SyncMode == adaptedBatches && (facts["merge_verified"] != true || !validForkProofHead(p)) {
			return p, errors.New("verify the completed serial merge before initializing the queue")
		}
		q = &ForkQueueState{MergedThrough: p.LastSuccess, PreparedThrough: p.LastSuccess, MergedTarget: str(facts["merge_commit_sha"])}
		if q.MergedTarget == "" {
			// An unpublished serial successor already pinned a target after its
			// preceding merge was verified by StartForkBatch.
			q.MergedTarget = p.InitialTarget
		}
	}
	if queueUnfinished(ForkProgress{Queue: q}) >= f.queueLimit() {
		return p, errors.New("fork PR queue is full; finish its oldest batch before appending")
	}
	after, target, targetBranch := q.MergedThrough, integration, f.Target.Branch
	parentID, parentHead := "", ""
	for _, b := range q.Batches {
		if b.Progress.Phase != "synced" && !queueReviewReady(b, w.Guide.Version) {
			return p, errors.New("only batches with saved passing validation awaiting review may have successors")
		}
	}
	if len(q.Batches) > 0 {
		tail := q.Batches[len(q.Batches)-1]
		after = tail.Progress.Upstream
		if tail.Progress.Phase != "synced" {
			target, targetBranch, parentID, parentHead = tail.Progress.ValidatedHead, tail.Progress.Branch, tail.Progress.BatchID, tail.Progress.ValidatedHead
		}
	}
	if after == "" && p.SyncMode == adaptedBatches {
		after = p.BatchAfter
	}
	if after == "" {
		after = in.After
	}
	if in.After != "" && in.After != after {
		return p, errors.New("queue append must continue the exact prepared frontier; source ranges cannot be skipped")
	}
	if !shaRE.MatchString(target) {
		return p, ErrStaleWork
	}
	repo, err := openForkGitRepository(ctx, f)
	if err != nil {
		return p, err
	}
	defer repo.close()
	if err = repo.ensureCommit(ctx, f.Upstream.Repo, p.ObservedUpstream); err != nil {
		return p, err
	}
	if err = repo.ensureCommit(ctx, f.Target.Repo, target); err != nil {
		return p, err
	}
	batch, err := nextForkBatch(ctx, repo, after, p.ObservedUpstream, in.Count)
	if err != nil {
		return p, err
	}
	end := batch[len(batch)-1]
	id := hash([]byte(fmt.Sprintf("%s:batch:%s:%s", f.Key(), after, end)))
	round := hash([]byte(fmt.Sprintf("%s:%s:%d", id, target, p.Revision+1)))
	baseline := p.AdaptationBaseline
	if baseline == "" {
		baseline = after
	}
	next := ForkProgress{Queue: q, BatchID: id, IntegrationBase: integration, ParentBatch: parentID, ParentHead: parentHead, TargetBranch: targetBranch,
		AdaptationBaseline: baseline, PrivateRepos: p.PrivateRepos, SpecVersion: f.Version(), SyncMode: adaptedBatches, Round: round,
		Phase: "working", BatchAfter: after, BatchCommits: batch, BatchReason: in.Reason, Upstream: end, InitialTarget: target,
		ObservedUpstream: p.ObservedUpstream, Branch: "ghraft/sync/" + f.ID + "/" + round[:12], LastSuccess: q.MergedThrough,
		CreatedAt: time.Now().Unix(), GenerationAt: time.Now().Unix(), NextAttempt: time.Now().Add(4 * time.Hour).Unix()}
	// Completed records are archived by the transaction before removing them from
	// the bounded active window. A child still based on one must first rebuild.
	for len(q.Batches) > 0 && q.Batches[0].Progress.Phase == "synced" && (len(q.Batches) == 1 || q.Batches[1].Progress.ParentBatch == "") {
		q.Batches = q.Batches[1:]
	}
	o := ForkObservation{Classification: snap.Classification, Upstream: p.ObservedUpstream, Target: target, IntegrationTarget: integration, Objects: Objects{}}
	q.Batches = append(q.Batches, ForkQueuedBatch{Progress: unqueuedProgress(next), Observation: o})
	if _, err = s.checkWork(ctx, in.WorkInput, "coordinate"); err != nil {
		return p, err
	}
	if err = s.Store.StartForkBatch(ctx, f, *w.Fork, next, o, in.EvidenceVersion); err != nil {
		return p, err
	}
	return s.Store.ForkProgress(ctx, f.Key())
}

// Runtime durations are calculated when status is requested, not in snapshots:
// a ticking clock must not continuously invalidate tests or emit events.
func queueMetrics(p ForkProgress, now int64) map[string]any {
	if p.Queue == nil {
		return nil
	}
	rows := []map[string]any{}
	oldest := int64(0)
	for _, b := range p.Queue.Batches {
		x := b.Progress
		if x.Phase != "synced" && x.CreatedAt > 0 && (oldest == 0 || x.CreatedAt < oldest) {
			oldest = x.CreatedAt
		}
		r := map[string]any{"batch_id": x.BatchID, "phase": x.Phase, "pr": x.PR}
		if v := x.Validation; v != nil {
			if x.GenerationAt > 0 {
				r["preparation_seconds"] = max(int64(0), v.StartedAt-x.GenerationAt)
			}
			end := v.FinishedAt
			if end == 0 {
				end = now
			}
			r["validation_seconds"] = max(int64(0), end-v.StartedAt)
		}
		if x.ReadyAt > 0 {
			end := x.MergedAt
			if end == 0 {
				end = now
			}
			r["review_wait_seconds"] = max(int64(0), end-x.ReadyAt)
		}
		rows = append(rows, r)
	}
	age := int64(0)
	if oldest > 0 {
		age = max(int64(0), now-oldest)
	}
	return map[string]any{"prepared_through": p.Queue.PreparedThrough, "merged_through": p.Queue.MergedThrough, "unfinished": queueUnfinished(p), "oldest_pending_seconds": age, "batches": rows}
}

func queueArchiveName(resource, round string) string {
	// Keep the key within the metadata column limit even for 48-byte fork IDs.
	return "fork-batch:" + hash([]byte(resource+":"+round))
}

func queueArchive(b ForkQueuedBatch) []byte {
	// Retain proof/provenance and remote identity without indefinitely retaining
	// discussion bodies outside the normal snapshot/event retention policy.
	return canonical(struct {
		Progress     ForkProgress `json:"progress"`
		EvidenceHash string       `json:"evidence_hash"`
		PR           int          `json:"pr"`
		Head         string       `json:"head"`
		Base         string       `json:"base"`
		Merge        string       `json:"merge"`
	}{unqueuedProgress(b.Progress), hash(canonical(b.Observation)), b.Observation.PR, str(b.Observation.Objects["pull"]["head"]), str(b.Observation.Objects["pull"]["base"]), b.Observation.MergeCommit})
}
