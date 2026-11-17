package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Build a reviewed fixture resolution. Both original parent commits stay intact;
// the test commands must still check the downstream offset and upstream scaling.
func resolutionFixture(t *testing.T) (*Service, *forkFixtureSource, *SyncMergeResolution) {
	t.Helper()
	s, g, _ := forkFixture(t, true)
	path := filepath.Join(t.TempDir(), "resolve")
	fixtureGit(t, g.repository, "", "worktree", "add", "--detach", path, g.target)
	if _, err := gitLocal(t.Context(), path, "merge", "--no-commit", "--no-ff", g.upstream); err == nil {
		t.Fatal("fixture must have a real merge conflict")
	}
	if err := os.WriteFile(filepath.Join(path, "calculator.py"), []byte("def score(x):\n    return x * 2 + 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, path, "", "add", "calculator.py")
	tree := fixtureGit(t, path, "", "write-tree")
	head := syntheticCommit(t, g.repository, tree, g.target, g.upstream)
	fixtureGit(t, g.repository, "", "worktree", "remove", "--force", path)
	return s, g, &SyncMergeResolution{Upstream: g.upstream, Base: g.target, Head: head, Tree: tree}
}

func TestForkMergeResolutionCompletesOriginalSource(t *testing.T) {
	s, g, grant := resolutionFixture(t)
	in, w := currentForkInput(t, s)
	ws, err := s.PrepareWorkspace(t.Context(), in, g.repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.TestWorkspace(t.Context(), in); err == nil {
		t.Fatal("unresolved conflict passed")
	}
	s.Config.Repositories[0].SyncMergeResolution = grant
	in, w = currentForkInput(t, s)
	// An operator grant must not reset an existing source workspace.
	prepared, err := s.PrepareWorkspace(t.Context(), in, g.repository)
	if err != nil || prepared.Head != g.upstream {
		t.Fatal("existing source changed", prepared, err)
	}
	fixtureGit(t, ws.Path, "", "checkout", "--detach", grant.Head)
	proof, err := s.TestWorkspace(t.Context(), in)
	if err != nil || !proof.Passed || proof.Head != grant.Head || proof.Tree != grant.Tree || proof.Base != grant.Base {
		t.Fatal(proof, err)
	}
	in, w = currentForkInput(t, s)
	if !validForkProofHead(*w.Fork) || !w.Fork.ValidatedResolution.matches(g.upstream, g.target, proof.Head, proof.Tree) {
		t.Fatal("resolution proof missing", w.Fork)
	}
	check := GitHubActionInput{WorkInput: in, Action: "push", Head: grant.Head, Base: g.target, Ref: w.Fork.Branch}
	if _, err = s.CheckGitHubAction(t.Context(), check); err != nil {
		t.Fatal(err)
	}
	// The resolution grant does not confer a force-push lease.
	g.prBranch, g.remoteHead = w.Fork.Branch, syntheticCommit(t, g.repository, grant.Tree, grant.Base)
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	check.WorkInput, _ = currentForkInput(t, s)
	if _, err = s.CheckGitHubAction(t.Context(), check); err == nil {
		t.Fatal("resolution authorized replacing another writer's branch")
	}
	g.remoteHead = grant.Head
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	check.WorkInput, _ = currentForkInput(t, s)
	check.Action = "open-pr"
	if _, err = s.CheckGitHubAction(t.Context(), check); err != nil {
		t.Fatal(err)
	}
	g.pr, g.checks = 1, "success"
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	check.WorkInput, _ = currentForkInput(t, s)
	check.Action, check.Ref = "merge", "main"
	if _, err = s.CheckGitHubAction(t.Context(), check); err != nil {
		t.Fatal(err)
	}
	g.mergeCommit = syntheticCommit(t, g.repository, grant.Tree, grant.Base, grant.Head)
	g.target, g.closed, g.merged = g.mergeCommit, true, true
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, w = currentForkInput(t, s)
	if _, err = s.RestartFork(t.Context(), in); err == nil {
		t.Fatal("reviewed resolution treated as legacy integration")
	}
	if err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "merged", Summary: "Exact resolution tested and remote merge verified"}); err != nil {
		t.Fatal(err)
	}
	p, err := s.Store.ForkProgress(t.Context(), w.Event.Key)
	if err != nil || p.Phase != "synced" || p.LastSuccess != grant.Upstream {
		t.Fatal("source cursor replaced by integration commit", p, err)
	}
}

func TestForkMergeResolutionRejectsUnreviewedCommits(t *testing.T) {
	for _, scenario := range []string{"absent", "upstream", "base", "tree", "head", "reversed parents", "extra parent", "appended commit"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, approved := resolutionFixture(t)
			grant := *approved
			head := approved.Head
			switch scenario {
			case "upstream":
				grant.Upstream = strings.Repeat("a", 40)
			case "base":
				grant.Base = strings.Repeat("b", 40)
			case "tree":
				grant.Tree = fixtureGit(t, g.repository, "", "rev-parse", g.target+"^{tree}")
			case "head":
				grant.Head = strings.Repeat("c", 40)
			case "reversed parents":
				head = syntheticCommit(t, g.repository, grant.Tree, g.upstream, g.target)
				grant.Head = head
			case "extra parent":
				head = syntheticCommit(t, g.repository, grant.Tree, g.target, g.upstream, fixtureGit(t, g.repository, "", "rev-parse", g.target+"^"))
				grant.Head = head
			case "appended commit":
				head = syntheticCommit(t, g.repository, grant.Tree, approved.Head)
				grant.Head = head
			}
			in, w := currentForkInput(t, s)
			ws, err := s.PrepareWorkspace(t.Context(), in, g.repository)
			if err != nil {
				t.Fatal(err)
			}
			fixtureGit(t, ws.Path, "", "checkout", "--detach", head)
			if scenario != "absent" {
				s.Config.Repositories[0].SyncMergeResolution = &grant
			}
			in, _ = currentForkInput(t, s)
			if proof, err := s.TestWorkspace(t.Context(), in); err == nil || proof.Passed {
				t.Fatal("unreviewed resolution accepted", proof, err)
			}
			p, err := s.Store.ForkProgress(t.Context(), w.Event.Key)
			if err != nil || p.ValidatedHead != "" || p.ValidatedResolution != nil {
				t.Fatal("failed validation saved proof", p, err)
			}
		})
	}
}

func TestForkMergeResolutionStillRequiresTestsAndCurrentAuthority(t *testing.T) {
	for _, scenario := range []string{"failed tests", "revoked", "new target", "changed guide", "no write delegation"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, grant := resolutionFixture(t)
			s.Config.Repositories[0].SyncMergeResolution = grant
			if scenario == "failed tests" {
				s.Config.Repositories[0].TestCommands = [][]string{{"python3", "-c", "raise SystemExit(1)"}}
			}
			if scenario == "no write delegation" {
				s.Config.Repositories[0].GitHubActions = nil
			}
			in, w := currentForkInput(t, s)
			ws, err := s.PrepareWorkspace(t.Context(), in, g.repository)
			if err != nil || ws.Head != grant.Head {
				t.Fatal("new workspace did not select approved resolution", ws, err)
			}
			proof, err := s.TestWorkspace(t.Context(), in)
			if scenario == "failed tests" {
				if err == nil || proof.Passed {
					t.Fatal("grant waived failing test", proof, err)
				}
				return
			}
			if err != nil || !proof.Passed {
				t.Fatal(proof, err)
			}
			switch scenario {
			case "revoked":
				s.Config.Repositories[0].SyncMergeResolution = nil
			case "new target":
				g.target = syntheticCommit(t, g.repository, fixtureGit(t, g.repository, "", "rev-parse", g.target+"^{tree}"), g.target)
			case "changed guide":
				s.Config.Repositories[0].Goal += " New requirements."
			}
			if err = s.Collect(t.Context()); err != nil {
				t.Fatal(err)
			}
			in, _ = currentForkInput(t, s)
			if _, err = s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: in, Action: "push", Head: grant.Head, Base: g.target, Ref: w.Fork.Branch}); err == nil {
				t.Fatal("old proof or undelegated write accepted")
			}
			if scenario == "revoked" || scenario == "new target" {
				if _, err = s.TestWorkspace(t.Context(), in); err == nil {
					t.Fatal("obsolete resolution can be retested")
				}
			}
		})
	}
}

func TestForkMergeResolutionQueueCursorAndRebuild(t *testing.T) {
	s, q, _, after := newQueueFixtureMode(t, false)
	p := appendQueue(t, s, after)
	tree := fixtureGit(t, q.repository, "", "merge-tree", "--write-tree", q.target, p.Upstream)
	grant := &SyncMergeResolution{Upstream: p.Upstream, Base: q.target, Tree: tree, Head: syntheticCommit(t, q.repository, tree, q.target, p.Upstream)}
	s.Config.Repositories[0].SyncMergeResolution = grant
	// Use actual validation and all publication gates, with an in-memory remote.
	in, w := queueInput(t, s)
	if _, err := s.PrepareWorkspace(t.Context(), in, q.repository); err != nil {
		t.Fatal(err)
	}
	if proof, err := s.TestWorkspace(t.Context(), in); err != nil || !proof.Passed {
		t.Fatal(proof, err)
	}
	in, w = queueInput(t, s)
	q.prs[p.Branch] = &queueRemote{head: grant.Head, base: "main", baseSHA: q.target, number: 1, check: "success"}
	in, w = queueInput(t, s)
	if err := s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "changes-ready", Summary: "Resolution ready"}); err != nil {
		t.Fatal(err)
	}
	done := mergeQueue(t, s, q, w.Fork.BatchID)
	if done.Queue.MergedThrough != grant.Upstream || done.LastSuccess != grant.Upstream {
		t.Fatal("queue advanced to resolution instead of source")
	}
	next := appendQueue(t, s, "")
	if next.BatchAfter != grant.Upstream || next.ValidatedResolution != nil {
		t.Fatal("next source range or proof contaminated", next)
	}
	// A new target invalidates a prepared resolution; rebuild retains the range,
	// drops its proof and never gives the old resolution permission for that base.
	nextGrant := &SyncMergeResolution{Upstream: next.Upstream, Base: q.target}
	nextGrant.Tree = fixtureGit(t, q.repository, "", "merge-tree", "--write-tree", q.target, next.Upstream)
	nextGrant.Head = syntheticCommit(t, q.repository, nextGrant.Tree, q.target, next.Upstream)
	s.Config.Repositories[0].SyncMergeResolution = nextGrant
	in, _ = queueInput(t, s)
	if _, err := s.PrepareWorkspace(t.Context(), in, q.repository); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TestWorkspace(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	q.target = syntheticCommit(t, q.repository, nextGrant.Tree, q.target)
	in, _ = queueInput(t, s)
	rebuilt, err := s.RebuildForkBatch(t.Context(), ForkQueueInput{WorkInput: in, Batch: next.BatchID})
	if err != nil || rebuilt.ValidatedResolution != nil || rebuilt.Upstream != next.Upstream {
		t.Fatal(rebuilt, err)
	}
	in, _ = queueInput(t, s)
	ws, err := s.PrepareWorkspace(t.Context(), in, q.repository)
	if err != nil || ws.Head != next.Upstream {
		t.Fatal("old grant adopted onto new target", ws, err)
	}
}

func TestForkMergeResolutionPolicyIsExplicitAndVersioned(t *testing.T) {
	c := localPolicy(t, "local-edit")
	before, err := c.RepositoryGuide("example/fixture")
	if err != nil {
		t.Fatal(err)
	}
	grant := &SyncMergeResolution{Upstream: strings.Repeat("1", 40), Base: strings.Repeat("2", 40), Head: strings.Repeat("3", 40), Tree: strings.Repeat("4", 40)}
	c.Repositories[0].SyncMergeResolution = grant
	after, err := c.RepositoryGuide("example/fixture")
	if err != nil || before.Version == after.Version || len(after.GitHubActions) != 0 {
		t.Fatal("grant not versioned or granted write actions", after, err)
	}
	for _, scenario := range []string{"short SHA", "same head", "read-only", "adapted"} {
		t.Run(scenario, func(t *testing.T) {
			p := c.Repositories[0]
			r := *grant
			p.SyncMergeResolution = &r
			switch scenario {
			case "short SHA":
				r.Tree = "abc"
			case "same head":
				r.Head = r.Upstream
			case "read-only":
				p.Mode = "read-only"
			case "adapted":
				p.ForkSyncMode = adaptedBatches
			}
			if err := p.validate(); err == nil {
				t.Fatal("invalid resolution policy accepted")
			}
		})
	}
}
