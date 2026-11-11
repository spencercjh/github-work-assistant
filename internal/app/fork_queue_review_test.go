package app

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestForkQueueStatusRetry(t *testing.T) {
	status := func(id int, context, state, created string) map[string]any {
		return map[string]any{"id": id, "context": context, "state": state, "created_at": created}
	}
	for _, tc := range []struct {
		name   string
		rows   Objects
		failed bool
	}{
		{"retry succeeds", Objects{
			"status:9":  status(9, "build", "failure", "2026-11-11T01:00:00Z"),
			"status:10": status(10, "build", "success", "2026-11-11T01:01:00Z"),
		}, false},
		{"same second uses numeric id", Objects{
			"status:9":  status(9, "build", "failure", "2026-11-11T01:00:00Z"),
			"status:10": status(10, "build", "success", "2026-11-11T01:00:00Z"),
		}, false},
		{"context is case insensitive", Objects{
			"status:9":  status(9, "Build", "failure", "2026-11-11T01:00:00Z"),
			"status:10": status(10, "build", "success", "2026-11-11T01:01:00Z"),
		}, false},
		{"latest failure", Objects{
			"status:9":  status(9, "build", "success", "2026-11-11T01:00:00Z"),
			"status:10": status(10, "build", "failure", "2026-11-11T01:01:00Z"),
		}, true},
		{"separate context", Objects{
			"status:9":  status(9, "lint", "failure", "2026-11-11T01:00:00Z"),
			"status:10": status(10, "build", "success", "2026-11-11T01:01:00Z"),
		}, true},
		{"check run failure", Objects{
			"check:9":   {"name": "build", "conclusion": "failure"},
			"status:10": status(10, "build", "success", "2026-11-11T01:01:00Z"),
		}, true},
		{"unordered failure cannot be hidden", Objects{
			"status:bad": {"context": "build", "state": "failure"},
			"status:10":  status(10, "build", "success", "2026-11-11T01:01:00Z"),
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := string(canonical(tc.rows))
			if got := queueRemoteFailure(tc.rows); got != tc.failed {
				t.Fatalf("failed = %v, want %v", got, tc.failed)
			}
			if string(canonical(tc.rows)) != before {
				t.Fatal("status history was mutated")
			}
		})
	}
}

func TestForkQueueClosedParentRetryWithDeletedBranch(t *testing.T) {
	s, q, _, after := newQueueFixture(t)
	appendQueue(t, s, after)
	a := publishQueue(t, s, q)
	appendQueue(t, s, "")
	b := publishQueue(t, s, q)
	q.prs[a.Branch].closed = true
	selectQueue(t, s, a.BatchID)
	in, _ := queueInput(t, s)
	retry, err := s.RebuildForkBatch(t.Context(), ForkQueueInput{WorkInput: in, Batch: a.BatchID})
	if err != nil || retry.Branch == a.Branch {
		t.Fatal(retry, err)
	}
	delete(q.prs, a.Branch)
	// Reopening the service must still be able to collect the child against its
	// retired parent generation, even though that branch no longer exists.
	s.Store = openTestStore(t)
	s.Raft.Store = s.Store
	a = publishQueue(t, s, q)
	child := selectQueue(t, s, b.BatchID)
	if child.QueueStale == "" {
		t.Fatal("retired parent branch did not invalidate the child")
	}
	in, _ = queueInput(t, s)
	child, err = s.RebuildForkBatch(t.Context(), ForkQueueInput{WorkInput: in, Batch: b.BatchID})
	if err != nil || child.TargetBranch != a.Branch || child.RetargetFrom != b.TargetBranch {
		t.Fatal(child, err)
	}
	b = publishQueue(t, s, q)
	if b.QueueStale != "" || b.LastSuccess != "" || b.Queue.MergedThrough != "" || b.BatchAfter != child.BatchAfter {
		t.Fatal("recovery skipped source history or failed to rebuild the child")
	}
}

func TestForkQueueRecoverMergedStaleProof(t *testing.T) {
	s, q, _, after := newQueueFixture(t)
	appendQueue(t, s, after)
	a := publishQueue(t, s, q)
	appendQueue(t, s, "")
	b := publishQueue(t, s, q)
	selectQueue(t, s, a.BatchID)
	in, archived := queueInput(t, s)
	file := filepath.Join(t.TempDir(), "original-proof.json")
	data := canonical(archived)
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	r := q.prs[a.Branch]
	r.merge = syntheticCommit(t, q.repository, a.ValidatedTree, q.target)
	r.closed, r.merged, q.target = true, true, r.merge
	q.mergedAt = time.Now().UTC().Format(time.RFC3339)
	// A real rerun started with the cached pre-merge context loses its old proof
	// before its final refresh observes the already completed merge.
	if _, err := s.TestWorkspace(t.Context(), in); err == nil {
		t.Fatal("changed target should invalidate the rerun")
	}
	in, current := queueInput(t, s)
	if current.Fork.QueueStale == "" || current.Fork.Validation.State != "failed" {
		t.Fatal("fixture did not reproduce the failed rerun", current.Fork)
	}
	if _, err := s.RebuildForkBatch(t.Context(), ForkQueueInput{WorkInput: in, Batch: a.BatchID}); err == nil {
		t.Fatal("merged PR cannot be rebuilt")
	}
	if _, err := s.RecoverForkProof(t.Context(), in); err == nil {
		t.Fatal("recovery without an operator grant was allowed")
	}
	s.Config.ForkProofRecoveries = []ForkProofRecovery{{ForkID: s.Config.Forks[0].ID, PR: a.PR, MergeCommit: r.merge, ProofFile: file, ProofSHA256: hash(data), Reason: "Restore the independently pinned pre-merge proof."}}
	// The stale exception does not permit restoring a descendant or using a
	// valid archive with a different dependency binding.
	selectQueue(t, s, b.BatchID)
	childInput, _ := queueInput(t, s)
	if _, err := s.RecoverForkProof(t.Context(), childInput); err == nil {
		t.Fatal("descendant recovery bypassed queue order")
	}
	selectQueue(t, s, a.BatchID)
	in, current = queueInput(t, s)
	badArchive := archived
	badProgress := *archived.Fork
	badProgress.ParentHead = b.ValidatedHead
	badArchive.Fork = &badProgress
	if err := checkArchivedForkProof(badArchive, current, in, s.Config.Forks[0]); err == nil {
		t.Fatal("archive with altered dependency binding accepted")
	}
	receipt, err := s.RecoverForkProof(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := s.Store.ForkProgress(t.Context(), archived.Event.Key)
	if err != nil || restored.QueueStale != "" || !sameForkProof(restored, *archived.Fork) || !reflect.DeepEqual(restored.Validation, current.Fork.Validation) || restored.LastSuccess != "" || restored.Queue.MergedThrough != "" {
		t.Fatal("recovery lost audit state or advanced progress", restored, err)
	}
	i := queueIndex(restored, b.BatchID)
	if i < 0 || !reflect.DeepEqual(restored.Queue.Batches[i], current.Fork.Queue.Batches[i]) || restored.Queue.Batches[i].Progress.QueueStale == "" {
		t.Fatal("recovery modified or unblocked the descendant")
	}
	if repeated, err := s.RecoverForkProof(t.Context(), in); err != nil || !reflect.DeepEqual(receipt, repeated) {
		t.Fatal("recovery replay changed its audit record", err)
	}
	in, _ = queueInput(t, s)
	if err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "merged", Summary: "Record the verified merge using its original proof."}); err != nil {
		t.Fatal(err)
	}
	_, done := queueInput(t, s)
	if done.Fork.LastSuccess != a.Upstream || done.Fork.Queue.MergedThrough != a.Upstream {
		t.Fatal("verified merge could not advance only the queue head")
	}
}

func TestForkQueueValidatedFollowupBeforePush(t *testing.T) {
	s, q, _, after := newQueueFixture(t)
	in, _ := queueInput(t, s)
	p, err := s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, After: after, Count: 2, Reason: "Two related source changes."})
	if err != nil {
		t.Fatal(err)
	}
	in, _ = queueInput(t, s)
	ws, err := s.PrepareWorkspace(t.Context(), in, q.repository)
	if err != nil {
		t.Fatal(err)
	}
	first := p.BatchCommits[0]
	tree := fixtureGit(t, q.repository, "", "merge-tree", "--write-tree", p.InitialTarget, first)
	head := mappedCommit(t, q.repository, tree, p.InitialTarget, first)
	fixtureGit(t, ws.Path, "", "reset", "--hard", head)
	mapping := []ForkCommitMapping{{Upstream: first, Commit: head, Status: "ported", Reason: "Adapt source change."}, {Upstream: p.Upstream, Status: "not-applicable", Reason: "Initial assessment excluded the second change."}}
	writeMapping(t, ws.MappingFile, mapping)
	proof, err := s.TestWorkspace(t.Context(), in)
	if err != nil || !proof.Passed {
		t.Fatal(proof, err)
	}
	body, err := os.ReadFile(proof.PRBodyFile)
	if err != nil {
		t.Fatal(err)
	}
	q.prs[p.Branch] = &queueRemote{head: head, base: p.TargetBranch, baseSHA: p.InitialTarget, number: 101, body: string(body), check: "success"}
	in, _ = queueInput(t, s)
	if err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "changes-ready", Summary: "Initial mapped candidate ready for review."}); err != nil {
		t.Fatal(err)
	}
	// Review finds that the previously excluded source item is required. Port it
	// as a fast-forward while retaining one mapped commit per upstream item.
	tree = fixtureGit(t, q.repository, "", "merge-tree", "--write-tree", head, p.Upstream)
	followup := mappedCommit(t, q.repository, tree, head, p.Upstream)
	fixtureGit(t, ws.Path, "", "reset", "--hard", followup)
	mapping[1] = ForkCommitMapping{Upstream: p.Upstream, Commit: followup, Status: "ported", Reason: "Review confirmed this source item is required."}
	writeMapping(t, ws.MappingFile, mapping)
	in, _ = queueInput(t, s)
	proof, err = s.TestWorkspace(t.Context(), in)
	if err != nil || !proof.Passed {
		t.Fatal(proof, err)
	}
	in, current := queueInput(t, s)
	if current.Fork.QueueStale != "" {
		t.Fatal("fresh local validation requires pushing its followup, not rebuilding", current.Fork.QueueStale)
	}
	input := GitHubActionInput{WorkInput: in, Action: "push", Ref: p.Branch, Head: followup, Base: p.InitialTarget}
	check, err := s.CheckGitHubAction(t.Context(), input)
	if err != nil || !check.Permitted || check.ExpectedRemoteHead != "" {
		t.Fatal("validated fast-forward was blocked or granted force replacement", check, err)
	}
	if err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "changes-ready", Summary: "Not yet pushed."}); err == nil {
		t.Fatal("unpublished validation became review-ready")
	}
	q.prs[p.Branch].head = followup
	in, current = queueInput(t, s)
	if current.Fork.QueueStale != "" {
		t.Fatal("expected publication was marked stale")
	}
	// After publication, moving back to the old head is an external mutation.
	q.prs[p.Branch].head = head
	in, current = queueInput(t, s)
	input.WorkInput = in
	if _, err = s.CheckGitHubAction(t.Context(), input); err == nil || current.Fork.QueueStale == "" {
		t.Fatalf("old publication allowance survived: %v", err)
	}
}

func TestForkQueuePendingPublicationDoesNotGrantForcePush(t *testing.T) {
	s, q, _, after := newQueueFixture(t)
	appendQueue(t, s, after)
	p := publishQueue(t, s, q)
	in, _ := queueInput(t, s)
	ws, err := s.PrepareWorkspace(t.Context(), in, q.repository)
	if err != nil {
		t.Fatal(err)
	}
	files := fixtureGit(t, q.repository, "", "ls-tree", p.ValidatedTree) + "\n"
	blob := fixtureGit(t, q.repository, "adapted", "hash-object", "-w", "--stdin")
	tree := fixtureGit(t, q.repository, files+fmt.Sprintf("100644 blob %s\treview.txt\n", blob), "mktree")
	head := mappedCommit(t, q.repository, tree, p.InitialTarget, p.Upstream)
	fixtureGit(t, ws.Path, "", "reset", "--hard", head)
	writeMapping(t, ws.MappingFile, []ForkCommitMapping{{Upstream: p.Upstream, Commit: head, Status: "ported", Reason: "Review amended the original adaptation."}})
	proof, err := s.TestWorkspace(t.Context(), in)
	if err != nil || !proof.Passed {
		t.Fatal(proof, err)
	}
	in, current := queueInput(t, s)
	check, err := s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: in, Action: "push", Ref: p.Branch, Head: head, Base: p.InitialTarget})
	if err == nil || !strings.Contains(err.Error(), "not an ancestor") || check.Permitted || current.Fork.QueueStale != "" {
		t.Fatal("publication gap granted replacement or required rebuilding", check, err)
	}
	// During that gap, an unrelated remote update still invalidates the proof.
	q.prs[p.Branch].head = syntheticCommit(t, q.repository, p.ValidatedTree, p.ValidatedHead)
	in, current = queueInput(t, s)
	if current.Fork.QueueStale == "" {
		t.Fatal("unexpected remote mutation during publication was ignored")
	}
	if _, err = s.TestWorkspace(t.Context(), in); err == nil {
		t.Fatal("a rerun silently adopted an unexpected remote head")
	}
}
