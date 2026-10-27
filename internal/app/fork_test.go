package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Remote mutations are simulated in memory. Git ancestry and behavioral tests
// use real local commit objects, without git commit, pushes or live API calls.
type forkFixtureSource struct {
	repository, upstream, target, remoteHead, prBranch, mergeCommit string
	pr                                                              int
	closed, merged, private, denied                                 bool
	checks                                                          string
	accessErr                                                       error
}

func (g *forkFixtureSource) CheckIdentity(context.Context) error { return nil }
func (g *forkFixtureSource) RepositoryClassification(_ context.Context, _ string) (string, error) {
	if g.accessErr != nil {
		return "", g.accessErr
	}
	if g.denied {
		return "", ErrRepositoryNotAllowed
	}
	if g.private {
		return "private", nil
	}
	return "public", nil
}
func (g *forkFixtureSource) Discover(context.Context, string) ([]Watch, string, error) {
	if g.pr > 0 {
		return []Watch{{"example/fixture", g.pr}}, time.Now().UTC().Format(time.RFC3339), nil
	}
	return nil, time.Now().UTC().Format(time.RFC3339), nil
}
func (g *forkFixtureSource) Collect(ctx context.Context, w Watch) ([]byte, error) {
	state := "open"
	if g.closed {
		state = "closed"
	}
	return canonical(Snapshot{w.Key(), "public", Objects{
		"issue":   {"title": "Integrate upstream", "number": w.Number},
		"pull":    {"head": g.remoteHead, "head_ref": g.prBranch, "head_repo": "example/fixture", "base": "main", "base_sha": g.target, "state": state, "merged": g.merged, "merge_commit_sha": g.mergeCommit, "draft": false},
		"check:1": {"name": "regression", "status": g.checks},
	}}), nil
}
func (g *forkFixtureSource) CollectFork(ctx context.Context, f ForkSpec, p ForkProgress) (ForkObservation, error) {
	classification, err := g.RepositoryClassification(ctx, f.Target.Repo)
	if err != nil {
		return ForkObservation{}, err
	}
	contains := func(sha string) bool {
		if sha == "" {
			return false
		}
		_, err := gitLocal(ctx, g.repository, "merge-base", "--is-ancestor", sha, g.target)
		return err == nil
	}
	o := ForkObservation{Classification: classification, Upstream: g.upstream, Target: g.target, ContainsLatest: contains(g.upstream), ContainsPinned: contains(p.Upstream), ContainsValidated: contains(p.ValidatedHead), Objects: Objects{}}
	if g.private {
		o.PrivateRepos = []string{f.Target.Repo}
	}
	if p.ValidatedHead != "" && g.merged && g.mergeCommit != "" {
		a, _ := gitLocal(ctx, g.repository, "rev-parse", p.ValidatedHead+"^{tree}")
		b, _ := gitLocal(ctx, g.repository, "rev-parse", g.mergeCommit+"^{tree}")
		o.MergeCommit = g.mergeCommit
		_, testedErr := gitLocal(ctx, g.repository, "merge-base", "--is-ancestor", p.ValidatedHead, g.mergeCommit)
		_, upstreamErr := gitLocal(ctx, g.repository, "merge-base", "--is-ancestor", p.Upstream, g.mergeCommit)
		o.MergeVerified = a != "" && a == b && contains(g.mergeCommit) && testedErr == nil && upstreamErr == nil
	}
	if p.Branch == g.prBranch {
		o.SyncHead = g.remoteHead
		if g.pr > 0 {
			o.PR = g.pr
			b, _ := g.Collect(ctx, Watch{f.Target.Repo, g.pr})
			var snap Snapshot
			_ = json.Unmarshal(b, &snap)
			o.Objects = snap.Objects
		}
	}
	return o, nil
}

func syntheticCommit(t *testing.T, repo, tree string, parents ...string) string {
	t.Helper()
	body := "tree " + tree + "\n"
	for _, p := range parents {
		body += "parent " + p + "\n"
	}
	body += "author Test Fixture <fixture@example.invalid> 1700000001 +0000\ncommitter Test Fixture <fixture@example.invalid> 1700000001 +0000\n\nSynthetic fork update\n"
	return fixtureGit(t, repo, body, "hash-object", "-w", "-t", "commit", "--stdin")
}
func commitFixtureWorkspace(t *testing.T, path string, parents ...string) string {
	t.Helper()
	fixtureGit(t, path, "", "add", ".")
	tree := fixtureGit(t, path, "", "write-tree")
	sha := syntheticCommit(t, path, tree, parents...)
	fixtureGit(t, path, "", "reset", "--hard", sha)
	return sha
}

func forkFixture(t *testing.T) (*Service, *forkFixtureSource, *taskAPI) {
	t.Helper()
	f := &taskAPI{owner: "worker", status: "in_progress"}
	s, _ := sharedFixture(t, f)
	repo, target, upstream := conflictHistory(t)
	c := localPolicy(t, "local-edit")
	c.Repositories[0].TaskTarget = s.Config.DeliveryTarget
	c.Repositories[0].GitHubActions = []string{"push", "open-pr", "merge"}
	s.Config.configDir, s.Config.WorkspaceRoot, s.Config.Repositories = c.configDir, c.WorkspaceRoot, c.Repositories
	s.Config.Watches = nil
	s.Config.Forks = []ForkSpec{{ID: "custom", Upstream: BranchRef{"source/fixture", "main"}, Target: BranchRef{"example/fixture", "main"}}}
	s.Raft.Config = s.Config
	g := &forkFixtureSource{repository: repo, upstream: upstream, target: target}
	s.GH = g
	if err := s.Config.validateForks(); err != nil {
		t.Fatal(err)
	}
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s, g, f
}

func currentForkInput(t *testing.T, s *Service) (WorkInput, WorkContext) {
	t.Helper()
	key := s.Config.Forks[0].Key()
	_, version, err := s.Store.Snapshot(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.Store.Events(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Key != key || event.Version != version {
			continue
		}
		deliverEvent(t, s, event)
		w, err := s.WorkContext(t.Context(), event.ID)
		if err != nil {
			t.Fatal(err)
		}
		in := WorkInput{Executor: "@worker", EventID: event.ID, EvidenceVersion: w.CurrentVersion, GuideVersion: w.Guide.Version, TaskID: fixtureTask}
		if _, err := s.LinkTask(t.Context(), in); err != nil {
			t.Fatal(err)
		}
		w, err = s.WorkContext(t.Context(), event.ID)
		if err != nil {
			t.Fatal(err)
		}
		return in, w
	}
	t.Fatal("current maintenance event missing")
	return WorkInput{}, WorkContext{}
}

func TestForkMaintenanceE2E(t *testing.T) {
	s, g, api := forkFixture(t)
	ctx := t.Context()
	in, work := currentForkInput(t, s)
	firstRound, pinned := work.Fork.Round, work.Fork.Upstream
	ws, err := s.PrepareWorkspace(ctx, in, g.repository)
	if err != nil || len(ws.Conflicts) != 1 {
		t.Fatal("real conflict not prepared", ws, err)
	}
	// Text resolution that loses the fork's custom scaling must fail regression.
	if err := os.WriteFile(filepath.Join(ws.Path, "calculator.py"), []byte("def score(x):\n    return x + 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	bad := commitFixtureWorkspace(t, ws.Path, work.Fork.InitialTarget, pinned)
	if _, err = s.TestWorkspace(ctx, in); err == nil {
		t.Fatal("lost custom behavior passed")
	}
	check := GitHubActionInput{WorkInput: in, Action: "push", Head: bad, Base: g.target, Ref: work.Fork.Branch}
	if _, err = s.CheckGitHubAction(ctx, check); err == nil {
		t.Fatal("failed regression permitted push")
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "calculator.py"), []byte("def score(x):\n    return x * 2 + 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	good := commitFixtureWorkspace(t, ws.Path, bad)
	verified, err := s.TestWorkspace(ctx, in)
	if err != nil || !verified.Passed || verified.Head != good {
		t.Fatal("combined custom/upstream tests failed", verified, err)
	}
	check.Head = good
	if _, err = s.CheckGitHubAction(ctx, check); err != nil {
		t.Fatal("validated push denied", err)
	}
	check.Ref = "main"
	if _, err = s.CheckGitHubAction(ctx, check); err == nil {
		t.Fatal("direct target push allowed")
	}
	check.Ref = work.Fork.Branch
	api.owner = "successor"
	if _, err = s.CheckGitHubAction(ctx, check); !errors.Is(err, ErrClaimUnavailable) {
		t.Fatal("former owner retained authority", err)
	}
	api.owner = "worker"
	// Simulate push and a lost create-PR response. Discovery finds the same PR,
	// associates it with this maintenance job, and suppresses ordinary delivery.
	g.remoteHead, g.prBranch = good, work.Fork.Branch
	if err = s.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	in, _ = currentForkInput(t, s)
	check.WorkInput, check.Action = in, "open-pr"
	if _, err = s.CheckGitHubAction(ctx, check); err != nil {
		t.Fatal(err)
	}
	g.pr, g.checks = 1, "in_progress"
	s.Config.Discover = true
	if err = s.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	in, work = currentForkInput(t, s)
	if work.Fork.PR != 1 || s.checkAccess(ctx, "example/fixture#1") == nil {
		t.Fatal("generated PR acquired independent work")
	}
	// A newer upstream commit is coalesced without changing the active round.
	tree := fixtureGit(t, g.repository, "", "rev-parse", pinned+"^{tree}")
	g.upstream = syntheticCommit(t, g.repository, tree, pinned)
	if err = s.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := s.WorkContext(ctx, in.EventID)
	if err != nil || after.Stale || after.Fork.Upstream != pinned || after.Fork.ObservedUpstream != g.upstream {
		t.Fatal("new upstream interrupted pinned work", after, err)
	}
	g.checks = "success"
	if err = s.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	in, _ = currentForkInput(t, s)
	check.WorkInput, check.Action, check.Ref = in, "merge", "main"
	if _, err = s.CheckGitHubAction(ctx, check); err != nil {
		t.Fatal("tested sync merge denied", err)
	}
	// Remote merge contains the exact tested tree and both parent histories.
	mergedTree := fixtureGit(t, g.repository, "", "rev-parse", good+"^{tree}")
	g.target = syntheticCommit(t, g.repository, mergedTree, g.target, good)
	g.mergeCommit = g.target
	g.closed, g.merged = true, true
	if err = s.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	in, _ = currentForkInput(t, s)
	result := WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "merged", Summary: "Both custom behavior and upstream behavior passed; remote tree matches the tested commit."}
	if err = s.RecordWorkResult(ctx, result); err != nil {
		t.Fatal("verified completion rejected", err)
	}
	p, _ := s.Store.ForkProgress(ctx, "fork:custom")
	if p.LastSuccess != pinned || p.Phase != "synced" {
		t.Fatal("success position wrong", p)
	}
	// Restart the process/store, then start the coalesced next round in the same
	// fixed task. No old validation or previous PR leaks into the new round.
	reopened := openTestStore(t)
	s.Store, s.Raft.Store = reopened, reopened
	if err = s.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	_, work = currentForkInput(t, s)
	if work.Fork.Round == firstRound || work.Fork.Upstream != g.upstream || work.Fork.ValidatedHead != "" || work.Fork.PR != 0 || api.creates != 1 {
		t.Fatal("round recovery/ownership failed", work.Fork, api.creates)
	}
	if forkFacts(work)["remote_head"] != "" || forkFacts(work)["contains_validated"] != false || forkFacts(work)["merge_verified"] != false {
		t.Fatal("previous round proof leaked into new round", forkFacts(work))
	}
	if work.Fork.LastSuccess != pinned {
		t.Fatal("unprocessed upstream counted as success")
	}
}

func TestForkRecoveryRetriesAndConcurrentCollectors(t *testing.T) {
	s, g, _ := forkFixture(t)
	in, w := currentForkInput(t, s)
	f := s.Config.Forks[0]
	p := *w.Fork
	p.NextAttempt = time.Now().Add(-time.Minute).Unix()
	if err := s.Store.SaveForkProgress(t.Context(), f.Key(), p.Revision, p, nil); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Store.ForkProgress(t.Context(), f.Key())
	o, _ := g.CollectFork(t.Context(), f, p)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Go(func() { errs <- s.Store.ObserveFork(t.Context(), f, p, o) })
	}
	wg.Wait()
	close(errs)
	success, stale := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrStaleWork) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatal("concurrent progress overwrite", success, stale)
	}
	after, _ := s.Store.ForkProgress(t.Context(), f.Key())
	if after.Wake != 1 || after.Retries != 1 || after.LastSuccess != "" {
		t.Fatal("retry treated delivery as success", after)
	}
	if _, err := s.CheckWork(t.Context(), in); !errors.Is(err, ErrStaleWork) {
		t.Fatal("old retry event still current", err)
	}
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	again, _ := s.Store.ForkProgress(t.Context(), f.Key())
	if again.Wake != after.Wake {
		t.Fatal("unchanged poll duplicated retry")
	}
}

func TestForkConfigurationAndRevocation(t *testing.T) {
	s, g, _ := forkFixture(t)
	in, _ := currentForkInput(t, s)
	s.Config.Forks[0].Target.Branch = "changed"
	if _, err := s.WorkContext(t.Context(), in.EventID); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("reconfigured relationship exposed old work", err)
	}
	s.Config.Forks[0].Target.Branch = "main"
	g.private = true
	s.Config.PrivateRepositories = []string{"example/fixture"}
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.private = false
	s.Config.PrivateRepositories = nil
	if _, err := s.WorkContext(t.Context(), in.EventID); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("private history became public", err)
	}
	for _, branch := range []string{"../main", "feature//x", "feature.lock", ".hidden", "a@{b", "main/", "a/../b", "--all"} {
		if validBranch(branch) {
			t.Fatal("unsafe branch accepted", branch)
		}
	}
	c := localPolicy(t, "local-edit")
	c.Forks = []ForkSpec{{ID: "one", Upstream: BranchRef{"source/fixture", "main"}, Target: BranchRef{"example/fixture", "main"}}}
	c.Forks = append(c.Forks, ForkSpec{ID: "two", Upstream: BranchRef{"source/fixture", "other"}, Target: c.Forks[0].Target})
	if err := c.validateForks(); err == nil {
		t.Fatal("two rules can mutate one target")
	}
}

func TestForkGHCollectorUsesExactRefsAndRecoversPR(t *testing.T) {
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	gh, fixture := fakeGH(t, head, base)
	f := ForkSpec{ID: "fixture", Upstream: BranchRef{"source/fixture", "release/Main"}, Target: BranchRef{"example/fixture", "main"}}
	p := ForkProgress{Round: strings.Repeat("d", 64), Branch: "ghraft/sync/fixture/abc123", Upstream: head}
	data := readFixture(t, fixture)
	data["repos/source/fixture"] = map[string]any{"full_name": "source/fixture", "private": false}
	ref := func(branch, sha string) map[string]any {
		return map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"sha": sha, "type": "commit"}}
	}
	data["repos/source/fixture/git/ref/heads/release%2FMain"] = ref("release/Main", head)
	data["repos/example/fixture/git/ref/heads/main"] = ref("main", base)
	data["repos/example/fixture/compare/"+head+"..."+base+"?per_page=1"] = map[string]any{"status": "diverged"}
	data["repos/example/fixture/git/matching-refs/heads/"+url.PathEscape(p.Branch)] = []any{ref(p.Branch+"-other", head), ref(p.Branch, head)}
	q := url.Values{"state": {"all"}, "head": {"example:" + p.Branch}, "base": {"main"}, "per_page": {"100"}}
	data["repos/example/fixture/pulls?"+q.Encode()] = []any{[]any{map[string]any{"number": 1}}}
	obj(obj(data["repos/example/fixture/pulls/1"])["head"])["ref"] = p.Branch
	writeFixture(t, fixture, data)
	o, err := gh.CollectFork(t.Context(), f, p)
	if err != nil || o.PR != 1 || o.SyncHead != head || o.ContainsLatest {
		t.Fatal("exact ref/PR recovery failed", o, err)
	}
	// Before push, the local validated SHA is deliberately absent from GitHub.
	p.ValidatedHead = strings.Repeat("c", 40)
	if _, err = gh.CollectFork(t.Context(), f, p); err != nil {
		t.Fatal("queried an unpublished local commit", err)
	}
	p.ValidatedHead = head
	merge := strings.Repeat("e", 40)
	tree := strings.Repeat("f", 40)
	obj(data["repos/example/fixture/pulls/1"])["merged"] = true
	obj(data["repos/example/fixture/pulls/1"])["state"] = "closed"
	obj(data["repos/example/fixture/pulls/1"])["merge_commit_sha"] = merge
	for _, pair := range [][2]string{{merge, base}, {head, merge}} {
		data["repos/example/fixture/compare/"+pair[0]+"..."+pair[1]+"?per_page=1"] = map[string]any{"status": "ahead"}
	}
	for _, sha := range []string{merge, head} {
		data["repos/example/fixture/git/commits/"+sha] = map[string]any{"tree": map[string]any{"sha": tree}}
	}
	writeFixture(t, fixture, data)
	if o, err = gh.CollectFork(t.Context(), f, p); err != nil || !o.MergeVerified || o.MergeCommit != merge {
		t.Fatal("merge commit was not verified", o, err)
	}
	obj(obj(data["repos/example/fixture/pulls/1"])["base"])["ref"] = "other"
	writeFixture(t, fixture, data)
	if _, err := gh.CollectFork(t.Context(), f, p); err == nil {
		t.Fatal("wrong target PR accepted")
	}
	for _, path := range []string{"repos/x/y/git/ref/heads/..%2Fsecret", "repos/x/y/compare/main...other?per_page=1", "repos/x/y/git/refs/heads/main", fmt.Sprintf("repos/x/y/compare/%s...%s/merge?per_page=1", head, base)} {
		if validGitHubReadPath(path) {
			t.Fatal("unbounded branch API path allowed", path)
		}
	}
}

func testedFork(t *testing.T, actions []string) (*Service, *forkFixtureSource, WorkInput, WorkContext, Workspace) {
	t.Helper()
	s, g, _ := forkFixture(t)
	if actions != nil {
		s.Config.Repositories[0].GitHubActions = actions
	}
	in, w := currentForkInput(t, s)
	ws, err := s.PrepareWorkspace(t.Context(), in, g.repository)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(ws.Path, "calculator.py"), []byte("def score(x):\n    return x * 2 + 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	good := commitFixtureWorkspace(t, ws.Path, w.Fork.InitialTarget, w.Fork.Upstream)
	if _, err = s.TestWorkspace(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	g.pr, g.remoteHead, g.prBranch = 1, good, w.Fork.Branch
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, w = currentForkInput(t, s)
	return s, g, in, w, ws
}

func TestForkRecordsHumanMergeAndAdvancedTarget(t *testing.T) {
	for _, scenario := range []string{"human-merge", "advanced-target", "untested-merge-tree"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, in, w, _ := testedFork(t, []string{"push", "open-pr"})
			// Observing a human's merge grants no authority to perform one.
			if _, err := s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: in, Action: "merge", Head: g.remoteHead, Base: g.target, Ref: "main"}); !errors.Is(err, ErrWorkDenied) {
				t.Fatal("merge authorization expanded", err)
			}
			tree := fixtureGit(t, g.repository, "", "rev-parse", g.remoteHead+"^{tree}")
			otherTree := fixtureGit(t, g.repository, "", "rev-parse", g.target+"^{tree}")
			if scenario == "untested-merge-tree" {
				tree = otherTree
			}
			g.mergeCommit = syntheticCommit(t, g.repository, tree, g.target, g.remoteHead)
			g.target, g.closed, g.merged = g.mergeCommit, true, true
			if scenario == "advanced-target" {
				g.target = syntheticCommit(t, g.repository, otherTree, g.target)
			}
			if err := s.Collect(t.Context()); err != nil {
				t.Fatal(err)
			}
			in, _ = currentForkInput(t, s)
			err := s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "merged", Summary: "Observed actual merge and checked tested tree/ancestry."})
			if scenario == "untested-merge-tree" {
				if err == nil {
					t.Fatal("untested merge tree accepted")
				}
				p, _ := s.Store.ForkProgress(t.Context(), "fork:custom")
				if p.LastSuccess != "" {
					t.Fatal("failed completion advanced success")
				}
				return
			}
			if err != nil {
				t.Fatal("verified human merge blocked", err)
			}
			p, _ := s.Store.ForkProgress(t.Context(), "fork:custom")
			if p.Phase != "synced" || p.LastSuccess != w.Fork.Upstream {
				t.Fatal("round was not completed", p)
			}
		})
	}
}

func TestForkTargetMovementRequiresIntegrationAndRetest(t *testing.T) {
	s, g, _, w, ws := testedFork(t, nil)
	tree := fixtureGit(t, g.repository, "", "rev-parse", g.target+"^{tree}")
	g.target = syntheticCommit(t, g.repository, tree, g.target)
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, _ := currentForkInput(t, s)
	check := GitHubActionInput{WorkInput: in, Action: "push", Head: g.remoteHead, Base: g.target, Ref: w.Fork.Branch}
	if _, err := s.CheckGitHubAction(t.Context(), check); err == nil {
		t.Fatal("old tests accepted moved target")
	}
	recovered, err := s.PrepareWorkspace(t.Context(), in, g.repository)
	if err != nil || recovered.Path != ws.Path {
		t.Fatal("same round workspace not recovered", err)
	}
	good := commitFixtureWorkspace(t, ws.Path, g.remoteHead, g.target)
	if _, err = s.TestWorkspace(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	check.Head = good
	if _, err = s.CheckGitHubAction(t.Context(), check); err != nil {
		t.Fatal("integrated target rejected", err)
	}
}

func TestForkClosedPRRestartAndWorkspaceIntentRecovery(t *testing.T) {
	s, g, _, w, _ := testedFork(t, nil)
	g.closed = true
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, _ := currentForkInput(t, s)
	if _, err := s.RestartFork(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, next := currentForkInput(t, s)
	if next.Fork.Round == w.Fork.Round || next.Fork.PR != 0 || next.Fork.LastSuccess != "" || next.Fork.ValidatedHead != "" {
		t.Fatal("restart reused abandoned proof", next.Fork)
	}
	// Simulate process termination after durable intent, before git worktree add.
	path, metadata, err := s.workspacePaths(next.Fork.Round)
	if err != nil {
		t.Fatal(err)
	}
	intent := Workspace{WorkInput: in, Resource: "fork:custom", Repository: g.repository, Path: path, Branch: next.Fork.Branch, Head: next.Fork.InitialTarget, Base: next.Fork.Upstream}
	if err = os.WriteFile(metadata, canonical(intent), 0600); err != nil {
		t.Fatal(err)
	}
	ws, err := s.PrepareWorkspace(t.Context(), in, g.repository)
	if err != nil || ws.Path != path || len(ws.Conflicts) == 0 {
		t.Fatal("durable intent could not recover", ws, err)
	}
	if _, err = s.PrepareWorkspace(t.Context(), in, g.repository); err != nil {
		t.Fatal("repeated preparation failed", err)
	}
}

func TestCurrentEventSurvivesRepeatedSnapshotVersions(t *testing.T) {
	store := freshTestStore(t)
	snap := Snapshot{Key: "example/repo#1", Classification: "public", Objects: Objects{"issue": {"title": "A"}}}
	first, err := store.Observe(t.Context(), snap.Key, canonical(snap))
	if err != nil {
		t.Fatal(err)
	}
	snap.Objects["issue"]["title"] = "B"
	if _, err = store.Observe(t.Context(), snap.Key, canonical(snap)); err != nil {
		t.Fatal(err)
	}
	snap.Objects["issue"]["title"] = "A"
	latest, err := store.Observe(t.Context(), snap.Key, canonical(snap))
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately make timestamps equal: random event hashes are not a clock.
	if _, err = store.db.ExecContext(t.Context(), `UPDATE ghraft_events SET created_at=100`); err != nil {
		t.Fatal(err)
	}
	w, err := store.ReadWork(t.Context(), first)
	if err != nil || w.CurrentEventID != latest {
		t.Fatal("lost latest transition", w.CurrentEventID, latest, err)
	}
	requested, err := store.RequestWork(t.Context(), snap.Key, w.CurrentVersion, fixtureTask, "Reassess this work")
	if err != nil {
		t.Fatal(err)
	}
	w, err = store.ReadWork(t.Context(), first)
	if err != nil || w.CurrentEventID != requested.ID {
		t.Fatal("request did not become current", err)
	}
}

// Simulate crossing the evidence freshness window while regression tests run.
// The test runner itself still executes the real fixture commands.
type expiredTestEvidenceStore struct {
	Persistence
	store             *Store
	afterInvalidation func()
}

func (s *expiredTestEvidenceStore) SaveForkProgress(ctx context.Context, key string, revision int64, p ForkProgress, result *WorkResult) error {
	if err := s.Persistence.SaveForkProgress(ctx, key, revision, p, result); err != nil {
		return err
	}
	if p.ValidatedHead == "" {
		if _, err := s.store.db.ExecContext(ctx, `UPDATE ghraft_snapshots SET observed_at=? WHERE resource_key=?`, time.Now().Add(-11*time.Minute).Unix(), key); err != nil {
			return err
		}
		if s.afterInvalidation != nil {
			s.afterInvalidation()
		}
	}
	return nil
}

func TestWorkspaceRefreshesEvidenceAfterRegressionCommands(t *testing.T) {
	for _, scenario := range []string{"unchanged", "target moved", "access revoked"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, in, w, _ := testedFork(t, nil)
			store := s.Store.(*Store)
			s.Store = &expiredTestEvidenceStore{Persistence: store, store: store, afterInvalidation: func() {
				switch scenario {
				case "target moved":
					tree := fixtureGit(t, g.repository, "", "rev-parse", g.target+"^{tree}")
					g.target = syntheticCommit(t, g.repository, tree, g.target)
				case "access revoked":
					g.denied = true
				}
			}}
			got, err := s.TestWorkspace(t.Context(), in)
			p, readErr := store.ForkProgress(t.Context(), w.Event.Key)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if scenario == "unchanged" {
				if err != nil || !got.Passed || p.ValidatedHead != got.Head || p.ValidatedBase != got.Base {
					t.Fatal("unchanged tested inputs were not saved", got, p, err)
				}
			} else {
				if err == nil || got.Passed || p.ValidatedHead != "" {
					t.Fatal("changed or unavailable remote evidence accepted", got, p, err)
				}
			}
		})
	}
}
