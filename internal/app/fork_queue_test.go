package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
)

// Each branch is a separate simulated remote PR, with real local Git history
// and regression commands. No network writes or synthetic passing proof.
type queueRemote struct {
	head, base, baseSHA, merge, body, check string
	number                                  int
	closed, merged                          bool
}
type queueSource struct {
	*forkFixtureSource
	prs     map[string]*queueRemote
	missing string
}

func (q *queueSource) CollectFork(ctx context.Context, f ForkSpec, p ForkProgress) (ForkObservation, error) {
	target := q.target
	branch := forkTargetBranch(f, p)
	if branch != f.Target.Branch {
		r := q.prs[branch]
		if r == nil || r.head == "" {
			return ForkObservation{}, errors.New("missing predecessor ref")
		}
		target = r.head
	}
	g := *q.forkFixtureSource
	g.target = target
	r := q.prs[p.Branch]
	if p.PR > 0 && (r == nil || p.Branch == q.missing) {
		return ForkObservation{}, errors.New("missing known PR")
	}
	if r != nil {
		g.pr, g.prBranch, g.remoteHead, g.prBase, g.mergeCommit, g.closed, g.merged, g.prBody, g.checks = r.number, p.Branch, r.head, r.baseSHA, r.merge, r.closed, r.merged, &r.body, r.check
	}
	o, err := g.CollectFork(ctx, f, p)
	o.IntegrationTarget = q.target
	if r != nil && r.number > 0 {
		o.Objects["pull"]["base"] = r.base
		if r.base != f.Target.Branch {
			o.MergeVerified = false
		}
	}
	return o, err
}
func newQueueFixture(t *testing.T) (*Service, *queueSource, *taskAPI, string) {
	t.Helper()
	return newQueueFixtureMode(t, true)
}
func newQueueFixtureMode(t *testing.T, adapted bool) (*Service, *queueSource, *taskAPI, string) {
	t.Helper()
	s, g, api := forkFixtureConfigured(t, adapted)
	after := fixtureGit(t, g.repository, "", "rev-parse", g.upstream+"^")
	// Three independent changes on a linear upstream, retaining the fork offset.
	for i := 2; i <= 3; i++ {
		files := fixtureGit(t, g.repository, "", "ls-tree", g.upstream) + "\n"
		blob := fixtureGit(t, g.repository, fmt.Sprint(i), "hash-object", "-w", "--stdin")
		tree := fixtureGit(t, g.repository, files+fmt.Sprintf("100644 blob %s\tfeature%d.txt\n", blob, i), "mktree")
		g.upstream = syntheticCommit(t, g.repository, tree, g.upstream)
	}
	q := &queueSource{forkFixtureSource: g, prs: map[string]*queueRemote{}}
	s.GH = q
	s.Config.Forks[0].Queue = &ForkQueueConfig{MaxOpenPRs: 3}
	if !adapted {
		s.Config.Forks[0].Queue.LocalRepository = g.repository
	}
	// Exercise persistent archive keys at the maximum supported identifier size
	// on every backend, including MySQL's bounded metadata name column.
	s.Config.Forks[0].ID = strings.Repeat("q", 48)
	s.Config.Repositories[0].GitHubActions = []string{"push", "open-pr", "edit-pr", "merge", "retarget-pr"}
	s.Raft.Config = s.Config
	queueInput(t, s)
	return s, q, api, after
}
func queueInput(t *testing.T, s *Service) (WorkInput, WorkContext) {
	t.Helper()
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	return currentForkInput(t, s)
}
func appendQueue(t *testing.T, s *Service, after string) ForkProgress {
	t.Helper()
	in, _ := queueInput(t, s)
	p, err := s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, After: after, Count: 1, Reason: "Independent bounded next source change"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func portQueue(t *testing.T, s *Service, q *queueSource) (ForkProgress, Workspace, WorkspaceTest) {
	t.Helper()
	in, w := queueInput(t, s)
	ws, err := s.PrepareWorkspace(t.Context(), in, q.repository)
	if err != nil {
		t.Fatal(err)
	}
	head := w.Fork.Upstream
	if w.Fork.SyncMode == adaptedBatches {
		tree := fixtureGit(t, q.repository, "", "merge-tree", "--write-tree", w.Fork.InitialTarget, w.Fork.Upstream)
		head = mappedCommit(t, q.repository, tree, w.Fork.InitialTarget, w.Fork.Upstream)
		fixtureGit(t, ws.Path, "", "reset", "--hard", head)
		writeMapping(t, ws.MappingFile, []ForkCommitMapping{{Upstream: w.Fork.Upstream, Commit: head, Status: "ported", Reason: "Preserve downstream offset."}})
	}
	proof, err := s.TestWorkspace(t.Context(), in)
	if err != nil || !proof.Passed {
		t.Fatal(proof, err)
	}
	in, w = queueInput(t, s)
	check, err := s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: in, Action: "push", Ref: w.Fork.Branch, Head: head, Base: w.Fork.InitialTarget})
	if err != nil || !check.Permitted {
		t.Fatal(check, err)
	}
	if w.Fork.SyncMode == adaptedBatches && w.Fork.RebuildFromHead != "" && check.ExpectedRemoteHead != w.Fork.RebuildFromHead {
		t.Fatal("rebuild lost exact lease", check)
	}
	ws.Head = head
	return *w.Fork, ws, proof
}
func publishQueue(t *testing.T, s *Service, q *queueSource) ForkProgress {
	t.Helper()
	p, ws, proof := portQueue(t, s, q)
	r := q.prs[p.Branch]
	if r == nil {
		r = &queueRemote{head: ws.Head}
		q.prs[p.Branch] = r
	} else {
		r.head = ws.Head
	}
	in, w := queueInput(t, s)
	body := []byte("Validate the original upstream range with downstream changes.")
	if proof.PRBodyFile != "" {
		var err error
		body, err = os.ReadFile(proof.PRBodyFile)
		if err != nil {
			t.Fatal(err)
		}
	}
	if r.number == 0 {
		check, err := s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: in, Action: "open-pr", Ref: p.Branch, Head: ws.Head, Base: p.InitialTarget, Body: string(body)})
		if err != nil || !check.Permitted || check.TargetBranch != p.TargetBranch {
			t.Fatal(check, err)
		}
		r.number, r.base, r.baseSHA = 100+len(q.prs), p.TargetBranch, p.InitialTarget
	} else if w.Fork.RetargetFrom != "" {
		check, err := s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: in, Action: "retarget-pr", Ref: p.Branch, Head: ws.Head, Base: p.InitialTarget, PRBase: r.baseSHA})
		if err != nil || !check.Permitted || strings.Join(check.EditableFields, ",") != "base" {
			t.Fatal(check, err)
		}
		r.base, r.baseSHA = p.TargetBranch, p.InitialTarget
	}
	r.body, r.check = string(body), "success"
	// Simulate creation succeeding but its client response being lost: collector
	// must recover the same PR instead of allocating another.
	in, w = queueInput(t, s)
	if w.Fork.PR != r.number {
		t.Fatal("orphan PR not recovered")
	}
	if err := s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "changes-ready", Summary: "Validated and published, awaiting independent review."}); err != nil {
		t.Fatal(err)
	}
	_, w = queueInput(t, s)
	return *w.Fork
}
func selectQueue(t *testing.T, s *Service, id string) ForkProgress {
	t.Helper()
	in, _ := queueInput(t, s)
	p, err := s.SelectForkBatch(t.Context(), ForkQueueInput{WorkInput: in, Batch: id})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func mergeQueue(t *testing.T, s *Service, q *queueSource, id string) ForkProgress {
	t.Helper()
	p := selectQueue(t, s, id)
	in, w := queueInput(t, s)
	r := q.prs[p.Branch]
	check, err := s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: in, Action: "merge", Ref: "main", Head: p.ValidatedHead, Base: q.target, PRBase: r.baseSHA})
	if err != nil || !check.Permitted {
		t.Fatal(check, err)
	}
	parents := []string{q.target}
	if p.SyncMode == "" {
		parents = append(parents, p.ValidatedHead)
	}
	r.merge = syntheticCommit(t, q.repository, w.Fork.ValidatedTree, parents...)
	r.closed, r.merged = true, true
	q.target = r.merge
	in, _ = queueInput(t, s)
	if err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "merged", Summary: "Independent review completed; exact tested squash tree and parent verified."}); err != nil {
		t.Fatal(err)
	}
	_, w = queueInput(t, s)
	return *w.Fork
}
func TestForkQueueStackSquashRebuildAndRecovery(t *testing.T) {
	s, q, api, after := newQueueFixture(t)
	a := appendQueue(t, s, after)
	a = publishQueue(t, s, q)
	appendQueue(t, s, "")
	b := publishQueue(t, s, q)
	appendQueue(t, s, "")
	c := publishQueue(t, s, q)
	if b.ParentBatch != a.BatchID || c.ParentBatch != b.BatchID || b.TargetBranch != a.Branch || c.TargetBranch != b.Branch {
		t.Fatal("not a stack")
	}
	if c.LastSuccess != "" || c.Queue.MergedThrough != "" || c.Queue.PreparedThrough != c.Upstream || api.creates != 1 {
		t.Fatal("prepared frontier advanced merged cursor or created another task")
	}
	in, _ := queueInput(t, s)
	if _, err := s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, Count: 1, Reason: "overflow"}); err == nil {
		t.Fatal("capacity not enforced")
	}
	if _, err := s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: in, Action: "merge", Head: c.ValidatedHead, Base: c.ValidatedBase, Ref: b.Branch}); err == nil {
		t.Fatal("descendant merged into predecessor")
	}
	if _, err := s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: in, Action: "merge", Head: c.ValidatedHead, Base: c.ValidatedBase, Ref: "main"}); err == nil {
		t.Fatal("descendant skipped queue")
	}
	// Restart with the same database and recover all independent proofs.
	store := openTestStore(t)
	s.Store, s.Raft.Store = store, store
	_, w := queueInput(t, s)
	if len(w.Fork.Queue.Batches) != 3 {
		t.Fatal("restart lost queue")
	}
	done := mergeQueue(t, s, q, a.BatchID)
	if done.Queue.MergedThrough != a.Upstream {
		t.Fatal("merge did not advance only A")
	}
	oldB := selectQueue(t, s, b.BatchID)
	if oldB.QueueStale == "" {
		t.Fatal("squash did not invalidate descendant")
	}
	in, _ = queueInput(t, s)
	if _, err := s.TestWorkspace(t.Context(), in); err == nil {
		t.Fatal("stale generation tested")
	}
	rebuilt, err := s.RebuildForkBatch(t.Context(), ForkQueueInput{WorkInput: in, Batch: b.BatchID})
	if err != nil || rebuilt.Round == b.Round || rebuilt.Branch != b.Branch || rebuilt.Upstream != b.Upstream || rebuilt.ParentBatch != "" || rebuilt.ValidatedHead != "" || rebuilt.TargetBranch != "main" {
		t.Fatal(rebuilt, err)
	}
	b = publishQueue(t, s, q)
	if b.ValidatedHead == oldB.ValidatedHead {
		t.Fatal("old approval head survived rebuild")
	}
	// The retired proof is audit history, never the current proof.
	raw, err := s.Store.Meta(t.Context(), queueArchiveName(s.Config.Forks[0].Key(), oldB.Round), "")
	if err != nil || !strings.Contains(raw, oldB.ValidatedHead) {
		t.Fatal("missing original proof archive", err)
	}
	mergeQueue(t, s, q, b.BatchID)
	selectQueue(t, s, c.BatchID)
	in, _ = queueInput(t, s)
	if _, err = s.RebuildForkBatch(t.Context(), ForkQueueInput{WorkInput: in, Batch: c.BatchID}); err != nil {
		t.Fatal(err)
	}
	c = publishQueue(t, s, q)
	done = mergeQueue(t, s, q, c.BatchID)
	if done.LastSuccess != q.upstream || done.Queue.MergedThrough != q.upstream || queueUnfinished(done) != 0 || api.creates != 1 {
		t.Fatal("queue failed to complete")
	}
	s.Config.Forks[0].Queue = nil
	_, w = queueInput(t, s)
	if w.Fork.Queue != nil || w.Fork.LastSuccess != q.upstream {
		t.Fatal("drained queue could not return to serial mode")
	}

}

func TestForkQueueFailureAndMutationFences(t *testing.T) {
	for _, scenario := range []string{"failed CI", "closed", "manual head", "manual base", "integration moved", "missing PR", "disabled", "failed test", "unfinished", "source skip"} {
		t.Run(scenario, func(t *testing.T) {
			s, q, _, after := newQueueFixture(t)
			a := appendQueue(t, s, after)
			if scenario != "unfinished" {
				a = publishQueue(t, s, q)
			}
			r := q.prs[a.Branch]
			switch scenario {
			case "failed CI":
				r.check = "failure"
			case "closed":
				r.closed = true
			case "manual head":
				r.head = syntheticCommit(t, q.repository, a.ValidatedTree, r.head)
			case "manual base":
				r.base = "unrelated"
			case "integration moved":
				q.target = syntheticCommit(t, q.repository, a.ValidatedTree, q.target)
			case "missing PR":
				q.missing = a.Branch
			case "disabled":
				s.Config.Forks[0].Queue = nil
			case "failed test":
				p, err := s.Store.ForkProgress(t.Context(), s.Config.Forks[0].Key())
				if err != nil {
					t.Fatal(err)
				}
				p.Validation.State = "failed"
				if err = s.Store.SaveForkProgress(t.Context(), s.Config.Forks[0].Key(), p.Revision, p, nil); err != nil {
					t.Fatal(err)
				}
			}
			in, _ := currentForkInput(t, s)
			if err := s.Collect(t.Context()); err != nil {
				if scenario != "missing PR" && scenario != "disabled" {
					t.Fatal(err)
				}
				return
			}
			in, _ = currentForkInput(t, s)
			override := ""
			if scenario == "source skip" {
				override = q.upstream
			}
			if _, err := s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, After: override, Count: 1, Reason: "must refuse"}); err == nil {
				t.Fatal("unsafe append permitted")
			}
			p, _ := s.Store.ForkProgress(t.Context(), s.Config.Forks[0].Key())
			if p.LastSuccess != "" || len(p.Queue.Batches) != 1 {
				t.Fatal("failed transition mutated frontier")
			}
		})
	}
}

func TestForkQueueClosedRetryDoesNotSkipSource(t *testing.T) {
	s, q, _, after := newQueueFixture(t)
	appendQueue(t, s, after)
	a := publishQueue(t, s, q)
	q.prs[a.Branch].closed = true
	in, w := queueInput(t, s)
	rebuilt, err := s.RebuildForkBatch(t.Context(), ForkQueueInput{WorkInput: in, Batch: w.Fork.BatchID})
	if err != nil || rebuilt.BatchAfter != a.BatchAfter || rebuilt.Upstream != a.Upstream || rebuilt.BatchID != a.BatchID || rebuilt.PR != 0 || rebuilt.Branch == a.Branch {
		t.Fatal(rebuilt, err)
	}
	publishQueue(t, s, q)
}

func TestForkQueueHistoryRewriteAndOutOfOrderMerge(t *testing.T) {
	t.Run("accepted frontier survives batch archival", func(t *testing.T) {
		s, q, _, after := newQueueFixture(t)
		original := q.target
		appendQueue(t, s, after)
		a := publishQueue(t, s, q)
		mergeQueue(t, s, q, a.BatchID)
		b := appendQueue(t, s, "")
		if queueIndex(b, a.BatchID) != -1 || b.Queue.MergedTarget != q.target {
			t.Fatal("completed batch was not archived with a durable frontier")
		}
		q.target = original
		if err := s.Collect(t.Context()); err == nil || !strings.Contains(err.Error(), "accepted queue frontier") {
			t.Fatal("lost merge permitted continued synchronization", err)
		}
		in, _ := currentForkInput(t, s)
		if _, err := s.RebuildForkBatch(t.Context(), ForkQueueInput{WorkInput: in, Batch: b.BatchID}); err == nil {
			t.Fatal("rebuild bypassed a lost accepted merge")
		}
		p, _ := s.Store.ForkProgress(t.Context(), s.Config.Forks[0].Key())
		if p.Queue.MergedThrough != a.Upstream || p.BatchID != b.BatchID {
			t.Fatal("history loss changed cursor or selected work")
		}
	})
	t.Run("manual descendant merge cannot advance cursor", func(t *testing.T) {
		s, q, _, after := newQueueFixture(t)
		appendQueue(t, s, after)
		a := publishQueue(t, s, q)
		appendQueue(t, s, "")
		b := publishQueue(t, s, q)
		r := q.prs[b.Branch]
		r.merge = syntheticCommit(t, q.repository, b.ValidatedTree, a.ValidatedHead)
		r.closed, r.merged = true, true
		in, w := queueInput(t, s)
		if w.Fork.QueueStale == "" {
			t.Fatal("out-of-order merge was not detected")
		}
		if err := s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "merged", Summary: "Manual merge into the predecessor"}); err == nil {
			t.Fatal("manual descendant merge advanced source cursor")
		}
		if _, err := s.RebuildForkBatch(t.Context(), ForkQueueInput{WorkInput: in, Batch: b.BatchID}); err == nil {
			t.Fatal("already merged history was rebuilt")
		}
		p, _ := s.Store.ForkProgress(t.Context(), s.Config.Forks[0].Key())
		if p.Queue.MergedThrough != "" || p.LastSuccess != "" {
			t.Fatal("manual descendant merge changed accepted progress")
		}
	})
}

func TestForkQueueEvidenceComparisonKeepsDependencyChanges(t *testing.T) {
	p := ForkProgress{BatchID: "a", Queue: &ForkQueueState{Batches: []ForkQueuedBatch{{Progress: ForkProgress{BatchID: "a", Phase: "awaiting-review"}}}}}
	objects := Objects{"fork": {"wake": 0, "batch_id": "a"}}
	queueSnapshot(p, objects)
	before := canonical(Snapshot{Objects: objects})
	objects["queue:a"]["phase"] = "working"
	if !forkTestEvidenceMatches(before, canonical(Snapshot{Objects: objects})) {
		t.Fatal("own test phase invalidated its proof")
	}
	objects["queue:a"]["parent_head"] = "changed"
	if forkTestEvidenceMatches(before, canonical(Snapshot{Objects: objects})) {
		t.Fatal("dependency change accepted")
	}
}

func TestForkQueueCollectorUsesParentAndObservesRetarget(t *testing.T) {
	head, base, main := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	gh, fixture := fakeGH(t, head, base)
	f := ForkSpec{ID: "fixture", Upstream: BranchRef{"source/fixture", "main"}, Target: BranchRef{"example/fixture", "main"}, Queue: &ForkQueueConfig{}}
	p := ForkProgress{BatchID: strings.Repeat("d", 64), Round: strings.Repeat("e", 64), Branch: "ghraft/sync/fixture/child", TargetBranch: "ghraft/sync/fixture/parent", Upstream: head}
	data := readFixture(t, fixture)
	ref := func(branch, sha string) map[string]any {
		return map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"sha": sha, "type": "commit"}}
	}
	data["repos/source/fixture"] = map[string]any{"full_name": "source/fixture", "private": false}
	data["repos/source/fixture/git/ref/heads/main"] = ref("main", head)
	data["repos/example/fixture/git/ref/heads/main"] = ref("main", main)
	data["repos/example/fixture/git/ref/heads/"+url.PathEscape(p.TargetBranch)] = ref(p.TargetBranch, base)
	data["repos/example/fixture/compare/"+head+"..."+base+"?per_page=1"] = map[string]any{"status": "diverged"}
	data["repos/example/fixture/git/matching-refs/heads/"+url.PathEscape(p.Branch)] = []any{ref(p.Branch, head)}
	query := url.Values{"state": {"all"}, "head": {"example:" + p.Branch}, "per_page": {"100"}}
	data["repos/example/fixture/pulls?"+query.Encode()] = []any{[]any{map[string]any{"number": 1}}}
	obj(obj(data["repos/example/fixture/pulls/1"])["head"])["ref"] = p.Branch
	obj(obj(data["repos/example/fixture/pulls/1"])["base"])["ref"] = p.TargetBranch
	writeFixture(t, fixture, data)
	o, err := gh.CollectFork(t.Context(), f, p)
	if err != nil || o.Target != base || o.IntegrationTarget != main || o.PR != 1 {
		t.Fatal(o, err)
	}
	obj(obj(data["repos/example/fixture/pulls/1"])["base"])["ref"] = "manual-other"
	writeFixture(t, fixture, data)
	o, err = gh.CollectFork(t.Context(), f, p)
	if err != nil || o.Objects["pull"]["base"] != "manual-other" || o.MergeVerified {
		t.Fatal("manual target edit was lost", o, err)
	}
}

func TestForkQueueCASAndRunningLease(t *testing.T) {
	s, q, _, after := newQueueFixture(t)
	appendQueue(t, s, after)
	a := publishQueue(t, s, q)
	appendQueue(t, s, "")
	b := publishQueue(t, s, q)
	in, w := queueInput(t, s)
	old := cloneForkProgress(*w.Fork)
	if _, err := s.SelectForkBatch(t.Context(), ForkQueueInput{WorkInput: in, Batch: a.BatchID}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.StartForkBatch(t.Context(), s.Config.Forks[0], old, old, old.Queue.Batches[1].Observation, in.EvidenceVersion); !errors.Is(err, ErrStaleWork) {
		t.Fatal("late queue mutation replaced selection", err)
	}
	_, w = queueInput(t, s)
	p := cloneForkProgress(*w.Fork)
	p.Validation = &ForkValidation{ID: strings.Repeat("f", 64), Round: p.Round, State: "running", HeartbeatAt: 9999999999, Deadline: 9999999999}
	if err := s.Store.SaveForkProgress(t.Context(), w.Event.Key, p.Revision, p, nil); err != nil {
		t.Fatal(err)
	}
	in, _ = queueInput(t, s)
	if _, err := s.SelectForkBatch(t.Context(), ForkQueueInput{WorkInput: in, Batch: b.BatchID}); err == nil {
		t.Fatal("selected while test running")
	}
	if _, err := s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, Count: 1, Reason: "next"}); err == nil {
		t.Fatal("appended while test running")
	}
	if _, err := s.RebuildForkBatch(t.Context(), ForkQueueInput{WorkInput: in, Batch: a.BatchID}); err == nil {
		t.Fatal("rebuilt while test running")
	}
}

func TestForkQueueDecisionWaitIsExplicitAndTested(t *testing.T) {
	s, q, _, after := newQueueFixture(t)
	appendQueue(t, s, after)
	publishQueue(t, s, q)
	in, w := queueInput(t, s)
	// Exercise result state after the public entry's existing decision-message
	// check; that check is retained and tested by decision_mention_test.go.
	result := WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "needs-decision", ReviewOnly: true, Summary: "Only human merge choice remains."}
	if err := s.recordForkResult(t.Context(), w, result); err != nil {
		t.Fatal(err)
	}
	appendQueue(t, s, "")
	in, w = queueInput(t, s)
	result.WorkInput = in
	if err := s.recordForkResult(t.Context(), w, result); err == nil {
		t.Fatal("untested batch marked review-only")
	}
	result.ReviewOnly = false
	if err := s.recordForkResult(t.Context(), w, result); err != nil {
		t.Fatal(err)
	}
	in, _ = queueInput(t, s)
	if _, err := s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, Count: 1, Reason: "skip decision"}); err == nil {
		t.Fatal("blocking decision bypassed")
	}
}

func TestForkQueueMetricsAndPolicy(t *testing.T) {
	s, q, _, after := newQueueFixture(t)
	appendQueue(t, s, after)
	p := publishQueue(t, s, q)
	status, err := s.Store.Status(t.Context())
	if err != nil || status["fork_queue_metrics"] == nil {
		t.Fatal(err)
	}
	metrics := queueMetrics(p, p.ReadyAt+10)
	if metrics["unfinished"] != 1 || metrics["oldest_pending_seconds"].(int64) < 10 || metrics["merged_through"] != "" {
		t.Fatal(metrics)
	}
	original := s.Config.Forks[0].Version()
	s.Config.Forks[0].Queue.MaxOpenPRs = 51
	if s.Config.validateForks() == nil {
		t.Fatal("unbounded queue accepted")
	}
	s.Config.Forks[0].Queue.MaxOpenPRs = 0
	if s.Config.Forks[0].queueLimit() != 10 || s.Config.Forks[0].Version() != original {
		t.Fatal("policy changed relationship identity")
	}
	s.Config.Repositories[0].ForkSyncMode = ""
	if err := s.Config.validateForks(); err != nil {
		t.Fatal("original-commit queue rejected", err)
	}
}
