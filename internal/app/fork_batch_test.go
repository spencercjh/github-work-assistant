package app

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func beginAdaptedFixture(t *testing.T) (*Service, *forkFixtureSource, *taskAPI, WorkInput, WorkContext, Workspace) {
	t.Helper()
	s, g, api := forkFixtureConfigured(t, true)
	after := fixtureGit(t, g.repository, "", "rev-parse", g.upstream+"^")
	in, _ := currentForkInput(t, s)
	if _, err := s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, After: after, Count: 1, Reason: "Fixture baseline; preserve custom offset while porting upstream scale."}); err != nil {
		t.Fatal(err)
	}
	in, w := currentForkInput(t, s)
	ws, err := s.PrepareWorkspace(t.Context(), in, g.repository)
	if err != nil || ws.Head != g.target {
		t.Fatal("batch must start from target", ws, err)
	}
	if _, err = s.TestWorkspace(t.Context(), in); err == nil {
		t.Fatal("unadapted baseline accepted as completed work")
	}
	// Model a port: identical resulting files, but only target ancestry remains.
	tree := fixtureGit(t, g.repository, "", "merge-tree", "--write-tree", g.target, g.upstream)
	head := mappedCommit(t, g.repository, tree, g.target, g.upstream)
	fixtureGit(t, ws.Path, "", "reset", "--hard", head)
	ws, err = s.VerifyWorkspace(t.Context(), in)
	if err != nil || ws.Head != head {
		t.Fatal(err)
	}
	writeMapping(t, ws.MappingFile, []ForkCommitMapping{{Upstream: g.upstream, Commit: head, Status: "ported", Reason: "Preserve custom offset while porting scale."}})
	return s, g, api, in, w, ws
}

func TestAdaptedForkBatchKeepsOwnerAndAdvancesOnlyAfterHumanMerge(t *testing.T) {
	s, g, api, in, w, ws := beginAdaptedFixture(t)
	s.Config.Repositories[0].GitHubActions = []string{"push", "open-pr"}
	in, _ = currentForkInput(t, s)
	proof, err := s.TestWorkspace(t.Context(), in)
	if err != nil || !proof.Passed || proof.Head != ws.Head {
		t.Fatal(proof, err)
	}
	if _, err = gitLocal(t.Context(), g.repository, "merge-base", "--is-ancestor", w.Fork.Upstream, ws.Head); err == nil {
		t.Fatal("fixture did not model ported history")
	}
	check := GitHubActionInput{WorkInput: in, Action: "push", Head: ws.Head, Base: g.target, Ref: w.Fork.Branch}
	if _, err = s.CheckGitHubAction(t.Context(), check); err != nil {
		t.Fatal("tested adapted push rejected", err)
	}
	g.pr, g.prBranch, g.remoteHead = 1, w.Fork.Branch, ws.Head
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, _ = currentForkInput(t, s)
	check.WorkInput, check.Action, check.Ref = in, "merge", "main"
	if _, err = s.CheckGitHubAction(t.Context(), check); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("adaptation expanded merge permission", err)
	}
	if _, err = s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, Count: 1, Reason: "Next batch"}); err == nil {
		t.Fatal("unfinished published PR abandoned")
	}
	g.mergeCommit = syntheticCommit(t, g.repository, proof.Tree, g.target)
	g.target, g.closed, g.merged = g.mergeCommit, true, true
	// More upstream arrived while the old batch was being reviewed.
	tree := fixtureGit(t, g.repository, "", "rev-parse", g.upstream+"^{tree}")
	g.upstream = syntheticCommit(t, g.repository, tree, g.upstream)
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, before := currentForkInput(t, s)
	if before.Fork.Upstream != w.Fork.Upstream || before.Fork.LastSuccess != "" {
		t.Fatal("unverified cursor advanced")
	}
	if err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "merged", Summary: "Human reviewed ported changes; squash parent and tested tree match."}); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t)
	s.Store, s.Raft.Store = reopened, reopened
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, done := currentForkInput(t, s)
	if done.Fork.Phase != "synced" || done.Fork.LastSuccess != w.Fork.Upstream || done.Fork.Round != w.Fork.Round || done.Fork.ObservedUpstream != g.upstream {
		t.Fatal(done.Fork)
	}
	if _, err = s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, After: g.upstream, Count: 1, Reason: "Skip ahead"}); err == nil {
		t.Fatal("skipped cursor")
	}
	next, err := s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, Count: 1, Reason: "Next mainline change"})
	if err != nil || next.BatchAfter != w.Fork.Upstream || next.Upstream != g.upstream || next.ValidatedHead != "" || next.PR != 0 {
		t.Fatal(next, err)
	}
	_, current := currentForkInput(t, s)
	if current.Task.TaskID != in.TaskID || api.creates != 1 {
		t.Fatal("new task/owner created", api.creates)
	}
}

func TestAdaptedForkRejectsStaleProofAndAuthority(t *testing.T) {
	for _, scenario := range []string{"new head", "new base", "new guide", "new owner", "revoke mode", "failed tests", "changed during tests", "modified ancestry"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, api, in, w, ws := beginAdaptedFixture(t)
			if _, err := s.TestWorkspace(t.Context(), in); err != nil {
				t.Fatal(err)
			}
			check := GitHubActionInput{WorkInput: in, Action: "push", Head: ws.Head, Base: g.target, Ref: w.Fork.Branch}
			switch scenario {
			case "modified ancestry":
				common := fixtureGit(t, g.repository, "", "rev-parse", "--path-format=absolute", "--git-common-dir")
				if err := os.WriteFile(filepath.Join(common, "info", "grafts"), []byte(ws.Head+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "new head":
				tree := fixtureGit(t, g.repository, "", "rev-parse", ws.Head+"^{tree}")
				check.Head = syntheticCommit(t, g.repository, tree, ws.Head)
				fixtureGit(t, ws.Path, "", "reset", "--hard", check.Head)
			case "new base":
				tree := fixtureGit(t, g.repository, "", "rev-parse", g.target+"^{tree}")
				g.target = syntheticCommit(t, g.repository, tree, g.target)
				if err := s.Collect(t.Context()); err != nil {
					t.Fatal(err)
				}
				check.WorkInput, _ = currentForkInput(t, s)
				check.Base = g.target
			case "new guide":
				s.Config.Repositories[0].Goal += " changed"
				check.WorkInput, _ = currentForkInput(t, s)
			case "new owner":
				api.owner = "successor"
			case "revoke mode":
				s.Config.Repositories[0].ForkSyncMode = ""
			case "failed tests":
				s.Config.Repositories[0].TestCommands = [][]string{{"python3", "-c", "raise SystemExit(1)"}}
				check.WorkInput, _ = currentForkInput(t, s)
				if result, err := s.TestWorkspace(t.Context(), check.WorkInput); err == nil || result.Passed {
					t.Fatal("failed tests accepted")
				}
			case "changed during tests":
				tree := fixtureGit(t, g.repository, "", "rev-parse", ws.Head+"^{tree}")
				other := syntheticCommit(t, g.repository, tree, ws.Head)
				s.Config.Repositories[0].TestCommands = [][]string{{"git", "-C", ws.Path, "reset", "--hard", other}}
				check.WorkInput, _ = currentForkInput(t, s)
				if result, err := s.TestWorkspace(t.Context(), check.WorkInput); err == nil || result.Passed {
					t.Fatal("source changed during tests")
				}
			}
			if _, err := s.CheckGitHubAction(t.Context(), check); err == nil {
				t.Fatal("stale proof or revoked authority accepted")
			}
		})
	}
}

func TestAdaptedForkSelectsFirstParentOrderAndRejectsSkipping(t *testing.T) {
	s, g, _ := forkFixtureConfigured(t, true)
	after := fixtureGit(t, g.repository, "", "rev-parse", g.upstream+"^")
	first := g.upstream
	tree := fixtureGit(t, g.repository, "", "rev-parse", first+"^{tree}")
	second := syntheticCommit(t, g.repository, tree, first)
	side := syntheticCommit(t, g.repository, tree, after, g.target)
	g.upstream = syntheticCommit(t, g.repository, tree, second, side)
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, old := currentForkInput(t, s)
	if _, err := s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, After: side, Count: 1, Reason: "Not on mainline"}); err == nil {
		t.Fatal("side parent baseline accepted")
	}
	p, err := s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, After: after, Count: 2, Reason: "Oldest two mainline commits"})
	if err != nil || p.Upstream != second || strings.Join(p.BatchCommits, ",") != first+","+second || p.LastSuccess != "" {
		t.Fatal(p, err)
	}
	if _, err = s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, After: after, Count: 2, Reason: "Retry stale command"}); !errors.Is(err, ErrStaleWork) {
		t.Fatal("stale plan mutated round", err)
	}
	in, _ = currentForkInput(t, s)
	if _, err = s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, After: second, Count: 1, Reason: "Skip incomplete batch"}); err == nil {
		t.Fatal("incomplete batch skipped")
	}
	p, err = s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, Count: 3, Reason: "Expand unpublished batch, retaining same start"})
	if err != nil || p.Upstream != g.upstream || p.Round == old.Fork.Round || p.ValidatedHead != "" {
		t.Fatal(p, err)
	}
}

func TestAdaptedForkCollectorVerifiesActualMergeWithoutInventingUpstreamAncestry(t *testing.T) {
	for _, scenario := range []string{"valid squash", "wrong parent", "wrong tree", "target advanced", "unreachable merge", "changed head", "new merge commit", "completed legacy merge", "legacy missing cursor", "raw squash"} {
		t.Run(scenario, func(t *testing.T) {
			g, fixture, f, target, upstream := independentForkFixture(t)
			after := fixtureGit(t, f.LocalRepository, "", "rev-parse", upstream+"^")
			tree := fixtureGit(t, f.LocalRepository, "", "merge-tree", "--write-tree", target, upstream)
			targetTree := fixtureGit(t, f.LocalRepository, "", "rev-parse", target+"^{tree}")
			prepared := syntheticCommit(t, f.LocalRepository, targetTree, target)
			head := syntheticCommit(t, f.LocalRepository, tree, prepared)
			p := ForkProgress{SyncMode: adaptedBatches, BatchAfter: after, BatchCommits: []string{upstream}, Round: strings.Repeat("a", 64), Branch: "ghraft/sync/independent/batch", Upstream: upstream, InitialTarget: target, ValidatedHead: head, ValidatedBase: target, ValidatedTree: tree}
			parent, mergedTree := target, tree
			if scenario == "wrong parent" {
				parent = syntheticCommit(t, f.LocalRepository, tree, target)
			}
			if scenario == "wrong tree" {
				mergedTree = fixtureGit(t, f.LocalRepository, "", "rev-parse", target+"^{tree}")
			}
			parents := []string{parent}
			if scenario == "new merge commit" || strings.Contains(scenario, "legacy") {
				parents = append(parents, head)
			}
			if strings.Contains(scenario, "legacy") {
				p.Phase = "synced"
				if scenario == "completed legacy merge" {
					p.LastSuccess = upstream
				}
			}
			if scenario == "raw squash" {
				p.SyncMode, p.ValidatedHead, head = "", upstream, upstream
			}
			merge := syntheticCommit(t, f.LocalRepository, mergedTree, parents...)
			remoteTarget := merge
			if scenario == "unreachable merge" {
				remoteTarget = target
			}
			if scenario == "changed head" {
				head = syntheticCommit(t, f.LocalRepository, tree, head)
			}
			if scenario == "target advanced" {
				remoteTarget = syntheticCommit(t, f.LocalRepository, tree, merge)
			}
			data := readFixture(t, fixture)
			data["repos/example/fixture/commits/"+head+"/statuses?per_page=100"] = []any{[]any{}}
			data["repos/example/fixture/commits/"+head+"/check-runs?per_page=100"] = []any{map[string]any{"check_runs": []any{}}}
			obj(obj(data["repos/example/fixture/git/ref/heads/main"])["object"])["sha"] = remoteTarget
			data["repos/example/fixture/git/matching-refs/heads/"+url.PathEscape(p.Branch)] = []any{map[string]any{"ref": "refs/heads/" + p.Branch, "object": map[string]any{"sha": head, "type": "commit"}}}
			q := url.Values{"state": {"all"}, "head": {"example:" + p.Branch}, "base": {"main"}, "per_page": {"100"}}
			data["repos/example/fixture/pulls?"+q.Encode()] = []any{[]any{map[string]any{"number": 1}}}
			pull := obj(data["repos/example/fixture/pulls/1"])
			pull["state"], pull["merged"], pull["merge_commit_sha"] = "closed", true, merge
			obj(pull["head"])["sha"], obj(pull["head"])["ref"] = head, p.Branch
			obj(pull["base"])["sha"] = remoteTarget
			apiParents := []any{}
			for _, sha := range parents {
				apiParents = append(apiParents, map[string]any{"sha": sha})
			}
			data["repos/example/fixture/git/commits/"+merge] = map[string]any{"tree": map[string]any{"sha": mergedTree}, "parents": apiParents}
			writeFixture(t, fixture, data)
			o, err := g.CollectFork(t.Context(), f, p)
			if err != nil {
				t.Fatal(err)
			}
			want := scenario == "valid squash" || scenario == "target advanced" || scenario == "completed legacy merge"
			if o.MergeVerified != want || o.ContainsPinned || (len(parents) == 1 && o.ContainsValidated) {
				t.Fatal("wrong merge proof or fabricated upstream ancestry", o)
			}
		})
	}
}

func TestAdaptedForkClosedRoundRetriesSameRange(t *testing.T) {
	s, g, _, in, w, ws := beginAdaptedFixture(t)
	if _, err := s.TestWorkspace(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	g.pr, g.prBranch, g.remoteHead, g.closed = 1, w.Fork.Branch, ws.Head, true
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, _ = currentForkInput(t, s)
	if _, err := s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, Count: 1, Reason: "Retry"}); err == nil {
		t.Fatal("unretired PR replaced")
	}
	if _, err := s.RestartFork(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	p, err := s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, Count: 1, Reason: "Retry explicitly retired PR"})
	if err != nil || p.BatchAfter != w.Fork.BatchAfter || p.Upstream != w.Fork.Upstream || p.Round == w.Fork.Round || p.PR != 0 || p.ValidatedHead != "" || p.ValidatedMapping != "" || p.ValidatedPRSection != "" || p.LastSuccess != "" {
		t.Fatal(p, err)
	}
	if head := fixtureGit(t, ws.Path, "", "rev-parse", "HEAD"); head != ws.Head {
		t.Fatal("old worktree reset")
	}
	in, _ = currentForkInput(t, s)
	next, err := s.PrepareWorkspace(t.Context(), in, g.repository)
	if err != nil || next.MappingFile == ws.MappingFile {
		t.Fatal("retry did not get its own mapping", err)
	}
	oldRows, err := readForkMapping(ws.MappingFile)
	if err != nil || len(oldRows) != 1 || oldRows[0].Commit != ws.Head {
		t.Fatal("retry overwrote old inventory", err)
	}
	newRows, err := readForkMapping(next.MappingFile)
	if err != nil || len(newRows) != 1 || newRows[0].Upstream != p.Upstream || newRows[0].Commit != "" {
		t.Fatal("retry inherited old provenance", err)
	}
}

func TestBatchSelectionRequiresCurrentModeAndOwner(t *testing.T) {
	for _, scenario := range []string{"not enabled", "owner changed", "access revoked", "unknown baseline", "missing rationale"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, api := forkFixtureConfigured(t, true)
			in, w := currentForkInput(t, s)
			after := fixtureGit(t, g.repository, "", "rev-parse", g.upstream+"^")
			request := ForkBatchInput{WorkInput: in, After: after, Count: 1, Reason: "Documented baseline"}
			switch scenario {
			case "not enabled":
				s.Config.Repositories[0].ForkSyncMode = ""
			case "owner changed":
				api.owner = "successor"
			case "access revoked":
				g.denied = true
			case "unknown baseline":
				request.After = strings.Repeat("f", 40)
			case "missing rationale":
				request.Reason = ""
			}
			if _, err := s.StartForkBatch(t.Context(), request); err == nil {
				t.Fatal("unauthorized plan accepted")
			}
			p, err := s.Store.ForkProgress(t.Context(), w.Event.Key)
			if err != nil || p.Round != w.Fork.Round || p.LastSuccess != "" {
				t.Fatal("failed plan changed progress", p, err)
			}
		})
	}
}
