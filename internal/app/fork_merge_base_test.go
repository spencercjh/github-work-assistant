package app

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// Model a PR whose recorded base stays at its initial value while the target
// advances. The old proof must fail before a new integration test is run.
func laggedBaseMergeFixture(t *testing.T, adapted bool) (*Service, *forkFixtureSource, *taskAPI, GitHubActionInput, WorkContext, Workspace) {
	t.Helper()
	var s *Service
	var g *forkFixtureSource
	var api *taskAPI
	var in WorkInput
	var w WorkContext
	var ws Workspace
	var err error
	if adapted {
		s, g, api, in, w, ws = beginAdaptedFixture(t)
	} else {
		s, g, api = forkFixture(t)
		in, w = currentForkInput(t, s)
		ws, err = s.PrepareWorkspace(t.Context(), in, g.repository)
		if err != nil {
			t.Fatal(err)
		}
	}
	proof, err := s.TestWorkspace(t.Context(), in)
	if err != nil || !proof.Passed {
		t.Fatal("initial proof", proof, err)
	}
	if adapted {
		body, err := os.ReadFile(proof.PRBodyFile)
		if err != nil {
			t.Fatal(err)
		}
		// A real remote PR body does not disappear while the local proof is
		// cleared at the start of its replacement validation attempt.
		prBody := string(body)
		g.prBody = &prBody
	}
	g.pr, g.prBranch, g.remoteHead, g.prBase = 7, w.Fork.Branch, ws.Head, g.target
	tree := fixtureGit(t, g.repository, "", "rev-parse", g.target+"^{tree}")
	g.target = syntheticCommit(t, g.repository, tree, g.target)
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, w = currentForkInput(t, s)
	check := GitHubActionInput{WorkInput: in, Action: "merge", Head: ws.Head, Base: g.target, PRBase: g.prBase, Ref: "main"}
	if got, err := s.CheckGitHubAction(t.Context(), check); err == nil || got.Permitted {
		t.Fatal("recorded PR base allowed an old-target proof", got, err)
	}
	if proof, err := s.TestWorkspace(t.Context(), in); err != nil || !proof.Passed || proof.Head != ws.Head || proof.Base != g.target {
		t.Fatal("current-target proof", proof, err)
	}
	check.WorkInput, w = currentForkInput(t, s)
	return s, g, api, check, w, ws
}

func TestForkMergeSeparatesPRBaseAndTestedTarget(t *testing.T) {
	for _, adapted := range []bool{false, true} {
		t.Run(map[bool]string{false: "original", true: "adapted"}[adapted], func(t *testing.T) {
			s, g, api, check, w, ws := laggedBaseMergeFixture(t, adapted)
			before := string(canonical(w.Fork))
			got, err := s.CheckGitHubAction(t.Context(), check)
			if err != nil || !got.Permitted || got.Base != g.target || got.PRBase != g.prBase || got.Head != ws.Head || got.Repository != "example/fixture" || got.Number != 7 || len(got.EditableFields) != 0 {
				t.Fatal("tested merge with independently checked PR base rejected", got, err)
			}
			for _, scenario := range []string{"omitted PR base", "wrong PR base", "malformed PR base", "old target", "wrong head", "sync branch", "push", "open-pr", "comment", "review", "close", "rerun-ci", "resolve-review-thread", "retarget-pr"} {
				t.Run(scenario, func(t *testing.T) {
					bad := check
					switch scenario {
					case "omitted PR base":
						bad.PRBase = ""
					case "wrong PR base":
						bad.PRBase = g.target
					case "malformed PR base":
						bad.PRBase = "invalid"
					case "old target":
						bad.Base = g.prBase
					case "wrong head":
						bad.Head = g.target
					case "sync branch":
						bad.Ref = w.Fork.Branch
					default:
						bad.Action, bad.Ref = scenario, w.Fork.Branch
					}
					if got, err := s.CheckGitHubAction(t.Context(), bad); err == nil || got.Permitted {
						t.Fatal("PR base parameter bypassed action or revision fence", got, err)
					}
				})
			}
			// The collector also checks these fields; the action gate must not
			// accept a mismatched PR identity from any snapshot source.
			for field, value := range map[string]any{"state": "closed", "draft": true, "merged": true, "head": g.target, "base_sha": g.target, "head_ref": "other", "base": "other", "head_repo": "other/repository"} {
				bad := w
				var snapshot Snapshot
				if err := json.Unmarshal(w.Snapshot, &snapshot); err != nil {
					t.Fatal(err)
				}
				snapshot.Objects["pull"][field] = value
				bad.Snapshot = canonical(snapshot)
				if got, err := s.checkForkAction(t.Context(), bad, check); err == nil || got.Permitted {
					t.Fatal("mismatched PR field allowed", field, got, err)
				}
			}
			if adapted {
				body := "Source table removed"
				g.prBody = &body
				if got, err := s.CheckGitHubAction(t.Context(), check); err == nil || got.Permitted {
					t.Fatal("PR base parameter bypassed remote provenance", got, err)
				}
				check.WorkInput, _ = currentForkInput(t, s)
				if got, err := s.CheckGitHubAction(t.Context(), check); err == nil || got.Permitted {
					t.Fatal("refreshed body without provenance accepted", got, err)
				}
				g.prBody = nil
				if err = s.Collect(t.Context()); err != nil {
					t.Fatal(err)
				}
				check.WorkInput, _ = currentForkInput(t, s)
				mapping, err := os.ReadFile(ws.MappingFile)
				if err != nil {
					t.Fatal(err)
				}
				writeMapping(t, ws.MappingFile, []ForkCommitMapping{{Upstream: g.upstream, Commit: ws.Head, Status: "ported", Reason: "Changed since testing"}})
				if got, err := s.CheckGitHubAction(t.Context(), check); err == nil || got.Permitted {
					t.Fatal("PR base parameter bypassed tested mapping", got, err)
				}
				if err = os.WriteFile(ws.MappingFile, mapping, 0600); err != nil {
					t.Fatal(err)
				}
			}
			after, err := s.WorkContext(t.Context(), check.EventID)
			if err != nil || string(canonical(after.Fork)) != before {
				t.Fatal("merge checks rewrote proof or progress", err)
			}
			// Fresh-read a changed PR base, even when head and tested target stay
			// unchanged; an explicit old value is never a durable override.
			g.prBase = g.target
			if _, err = s.CheckGitHubAction(t.Context(), check); !errors.Is(err, ErrStaleWork) {
				t.Fatal("changed recorded PR base accepted without refresh", err)
			}
			check.WorkInput, _ = currentForkInput(t, s)
			if got, err := s.CheckGitHubAction(t.Context(), check); err == nil || got.Permitted {
				t.Fatal("old explicit PR base accepted after refresh", got, err)
			}
			check.PRBase = g.prBase
			if _, err = s.CheckGitHubAction(t.Context(), check); err != nil {
				t.Fatal("refreshed exact values rejected", err)
			}
			api.owner = "successor"
			if got, err := s.CheckGitHubAction(t.Context(), check); err == nil || got.Permitted {
				t.Fatal("PR base parameter bypassed ownership", got, err)
			}
			api.owner = "worker"
			permissions := s.Config.Repositories[0].GitHubActions
			s.Config.Repositories[0].GitHubActions = []string{"edit-pr"}
			check.WorkInput, _ = currentForkInput(t, s)
			if got, err := s.CheckGitHubAction(t.Context(), check); err == nil || got.Permitted {
				t.Fatal("metadata delegation allowed a merge", got, err)
			}
			s.Config.Repositories[0].GitHubActions = permissions
			// Both fresh observations and updated input still require a new proof
			// if the target moves after this successful check.
			tree := fixtureGit(t, g.repository, "", "rev-parse", g.target+"^{tree}")
			g.target = syntheticCommit(t, g.repository, tree, g.target)
			if _, err = s.CheckGitHubAction(t.Context(), check); !errors.Is(err, ErrStaleWork) {
				t.Fatal("moving target accepted captured event", err)
			}
			check.WorkInput, _ = currentForkInput(t, s)
			check.Base = g.target
			if got, err := s.CheckGitHubAction(t.Context(), check); err == nil || got.Permitted {
				t.Fatal("moving target accepted previous proof", got, err)
			}
		})
	}
}

func TestLaggedPRBaseDoesNotReplaceTestedMergeParentOrTree(t *testing.T) {
	for _, scenario := range []string{"tested squash", "PR base parent", "wrong tree"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, _, check, w, _ := laggedBaseMergeFixture(t, true)
			if _, err := s.CheckGitHubAction(t.Context(), check); err != nil {
				t.Fatal(err)
			}
			parent, tree := w.Fork.ValidatedBase, w.Fork.ValidatedTree
			if scenario == "PR base parent" {
				parent = g.prBase
			}
			if scenario == "wrong tree" {
				tree = fixtureGit(t, g.repository, "", "rev-parse", g.target+"^{tree}")
			}
			g.mergeCommit = syntheticCommit(t, g.repository, tree, parent)
			g.target, g.closed, g.merged = g.mergeCommit, true, true
			if err := s.Collect(t.Context()); err != nil {
				t.Fatal(err)
			}
			in, _ := currentForkInput(t, s)
			err := s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "merged", Summary: "Observed squash; recorded PR base remains distinct from tested target"})
			want := scenario == "tested squash"
			if (err == nil) != want {
				t.Fatalf("completion error %v, want success %v", err, want)
			}
			p, err := s.Store.ForkProgress(t.Context(), w.Event.Key)
			if err != nil || (p.LastSuccess == p.Upstream) != want || p.ValidatedBase != w.Fork.ValidatedBase || p.ValidatedTree != w.Fork.ValidatedTree || p.TestLogHash != w.Fork.TestLogHash {
				t.Fatal("completion changed proof or accepted an untested merge", err)
			}
		})
	}
}

func TestOrdinaryPRMergeRejectsRecordedBaseParameter(t *testing.T) {
	s, _, _, in := delegatedFixture(t)
	in.Action, in.PRBase = "merge", strings.Repeat("a", 40)
	if got, err := s.CheckGitHubAction(t.Context(), in); err == nil || got.Permitted {
		t.Fatal("ordinary PR accepted the fork-only field", got, err)
	}
}

func TestQueuedMergeRecordedBaseKeepsQueueFences(t *testing.T) {
	s, q, _, after := newQueueFixture(t)
	appendQueue(t, s, after)
	first := publishQueue(t, s, q)
	appendQueue(t, s, "")
	second := publishQueue(t, s, q)
	// The PR can retain an older base SHA even when its current target and
	// validation proof agree. The override must never allow merging a child.
	r := q.prs[first.Branch]
	if after == q.target {
		t.Fatal("fixture must distinguish recorded base from tested target")
	}
	r.baseSHA = after
	selectQueue(t, s, first.BatchID)
	in, _ := queueInput(t, s)
	check := GitHubActionInput{WorkInput: in, Action: "merge", Ref: "main", Head: first.ValidatedHead, Base: q.target, PRBase: r.baseSHA}
	got, err := s.CheckGitHubAction(t.Context(), check)
	if err != nil || !got.Permitted || got.TargetBranch != "main" || got.Base != q.target || got.PRBase != after {
		t.Fatal("queue head with a separately captured PR base rejected", got, err)
	}
	for _, badBase := range []string{"", q.target} {
		bad := check
		bad.PRBase = badBase
		if got, err := s.CheckGitHubAction(t.Context(), bad); err == nil || got.Permitted {
			t.Fatal("queue merge accepted an uncaptured PR base", got, err)
		}
	}
	selectQueue(t, s, second.BatchID)
	in, _ = queueInput(t, s)
	child := GitHubActionInput{WorkInput: in, Action: "merge", Ref: "main", Head: second.ValidatedHead, Base: second.InitialTarget, PRBase: q.prs[second.Branch].baseSHA}
	if got, err := s.CheckGitHubAction(t.Context(), child); err == nil || got.Permitted || !strings.Contains(err.Error(), "first unfinished batch") {
		t.Fatal("captured PR base bypassed the queue-head fence", got, err)
	}
	selectQueue(t, s, first.BatchID)
	r.check = "failure"
	check.WorkInput, _ = queueInput(t, s)
	if got, err := s.CheckGitHubAction(t.Context(), check); err == nil || got.Permitted || !strings.Contains(err.Error(), "current CI failed") {
		t.Fatal("captured PR base bypassed current CI failure", got, err)
	}
}
