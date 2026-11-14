package app

import (
	"strings"
	"testing"
)

func TestOriginalUpstreamQueuePreservesHistoryAndDownstreamBehavior(t *testing.T) {
	s, q, _, after := newQueueFixtureMode(t, false)
	// Without the downstream target, the upstream's own tests pass with offset=1.
	// This extra regression requires the actual downstream behavior in every batch.
	s.Config.Repositories[0].TestCommands = append(s.Config.Repositories[0].TestCommands,
		[]string{"python3", "-c", "from calculator import score; assert score(0) == 2"})
	var batches []ForkProgress
	initialTarget := q.target
	for i := 0; i < 3; i++ {
		appendQueue(t, s, after)
		p := publishQueue(t, s, q)
		if p.SyncMode != "" || p.ValidatedHead != p.Upstream || p.InitialTarget != initialTarget || p.TargetBranch != "main" || p.ValidatedMapping != "" {
			t.Fatal("original source or downstream base replaced", p)
		}
		if i > 0 && p.ParentBatch != batches[i-1].BatchID {
			t.Fatal("lost merge ordering")
		}
		batches = append(batches, p)
		after = ""
	}
	in, w := queueInput(t, s)
	if _, err := s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, Count: 1, Reason: "overflow"}); err == nil {
		t.Fatal("capacity bypassed")
	}
	if _, err := s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: in, Action: "merge", Ref: "main", Head: w.Fork.ValidatedHead, Base: initialTarget}); err == nil {
		t.Fatal("later original PR bypassed queue order")
	}
	store := openTestStore(t)
	s.Store, s.Raft.Store = store, store
	for i, original := range batches {
		selected := selectQueue(t, s, original.BatchID)
		if i > 0 {
			if selected.QueueStale == "" {
				t.Fatal("changed target left old proof current")
			}
			in, _ = queueInput(t, s)
			if _, err := s.TestWorkspace(t.Context(), in); err == nil {
				t.Fatal("stale generation tested without rebuild")
			}
			rebuilt, err := s.RebuildForkBatch(t.Context(), ForkQueueInput{WorkInput: in, Batch: selected.BatchID})
			if err != nil || rebuilt.Round == original.Round || rebuilt.Upstream != original.Upstream || rebuilt.Branch != original.Branch || rebuilt.PR != original.PR || rebuilt.ParentBatch != "" || rebuilt.RetargetFrom != "" {
				t.Fatal("rebuild changed original PR/source identity", rebuilt, err)
			}
			p := publishQueue(t, s, q)
			if p.ValidatedHead != original.ValidatedHead || p.ValidatedBase == original.ValidatedBase || p.ValidatedGuide != original.ValidatedGuide {
				t.Fatal("rebuild did not retain source and refresh target")
			}
			archived, err := s.Store.Meta(t.Context(), queueArchiveName(s.Config.Forks[0].Key(), original.Round), "")
			if err != nil || !strings.Contains(archived, original.ValidatedTree) {
				t.Fatal("old test proof lost", err)
			}
		}
		done := mergeQueue(t, s, q, original.BatchID)
		if done.Queue.MergedThrough != original.Upstream {
			t.Fatal("merged frontier skipped source")
		}
		// Completion remains stable across the next collection, even with newer upstream.
		if done.Phase != "synced" || done.BatchID != original.BatchID {
			t.Fatal("collector replaced a completed bounded batch with latest upstream")
		}
	}
	_, w = queueInput(t, s)
	if queueUnfinished(*w.Fork) != 0 || w.Fork.LastSuccess != q.upstream {
		t.Fatal("queue did not complete")
	}
	for _, p := range batches {
		fixtureGit(t, q.repository, "", "merge-base", "--is-ancestor", p.Upstream, q.target)
	}
}

func TestOriginalUpstreamQueueRequiresIntegratedBaseline(t *testing.T) {
	s, q, _, after := newQueueFixtureMode(t, false)
	in, _ := queueInput(t, s)
	first := fixtureGit(t, q.repository, "", "rev-parse", q.upstream+"~2")
	if _, err := s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, After: first, Count: 1, Reason: "skip pending source"}); err == nil || !strings.Contains(err.Error(), "baseline must already be present") {
		t.Fatal("unmerged baseline accepted", err)
	}
	appendQueue(t, s, after)
	publishQueue(t, s, q)
	in, _ = queueInput(t, s)
	if _, err := s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, After: q.upstream, Count: 1, Reason: "skip prepared frontier"}); err == nil {
		t.Fatal("queue source range skipped")
	}
}

func TestOriginalUpstreamQueueAncestryPolicy(t *testing.T) {
	s, q, _, _ := newQueueFixtureMode(t, false)
	f := &s.Config.Forks[0]
	originalVersion := f.Version()
	def, _ := s.Config.WorkDefinition(f.Key())
	guide, err := def.Guide(s.Config)
	if err != nil || guide.ForkQueue == nil || !strings.Contains(guide.Text, upstreamQueueInstruction) {
		t.Fatal("missing original queue policy", err)
	}
	f.Queue.LocalRepository = ""
	if err := s.Config.validateForks(); err == nil {
		t.Fatal("queue accepted without an ancestry clone")
	}
	f.Queue.LocalRepository = "relative/path"
	if err := s.Config.validateForks(); err == nil {
		t.Fatal("relative clone accepted")
	}
	f.Queue.LocalRepository = q.repository
	if f.Version() != originalVersion {
		t.Fatal("queue setup changed maintained branch relationship")
	}
	if err := s.Config.validateForks(); err != nil {
		t.Fatal(err)
	}
}

func TestOriginalUpstreamQueueCannotForceReplaceOnRebuild(t *testing.T) {
	s, q, _, after := newQueueFixtureMode(t, false)
	appendQueue(t, s, after)
	p := publishQueue(t, s, q)
	q.prs[p.Branch].head = syntheticCommit(t, q.repository, p.ValidatedTree, q.target)
	in, _ := queueInput(t, s)
	if _, err := s.RebuildForkBatch(t.Context(), ForkQueueInput{WorkInput: in, Batch: p.BatchID}); err != nil {
		t.Fatal(err)
	}
	in, _ = queueInput(t, s)
	if _, err := s.PrepareWorkspace(t.Context(), in, q.repository); err != nil {
		t.Fatal(err)
	}
	if proof, err := s.TestWorkspace(t.Context(), in); err != nil || !proof.Passed {
		t.Fatal(proof, err)
	}
	in, w := queueInput(t, s)
	if _, err := s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: in, Action: "push", Ref: p.Branch, Head: w.Fork.ValidatedHead, Base: q.target}); err == nil {
		t.Fatal("queue rebuild granted an original-upstream force push")
	}
}
