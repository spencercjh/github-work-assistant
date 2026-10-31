package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type retryFixtureSource struct {
	*forkFixtureSource
	run  ciWorkflowRun
	job  ciWorkflowJob
	err  error
	hook func()
}

func (g *retryFixtureSource) CIRetryState(context.Context, string, int64, int64) (ciWorkflowRun, ciWorkflowJob, error) {
	if g.hook != nil {
		g.hook()
	}
	return g.run, g.job, g.err
}

func ciRetryFixture(t *testing.T) (*Service, *retryFixtureSource, *taskAPI, GitHubActionInput) {
	t.Helper()
	s, g, api, _, w, ws := beginAdaptedFixture(t)
	s.Config.Repositories[0].GitHubActions = []string{"push", "open-pr", "edit-pr"}
	in, _ := currentForkInput(t, s)
	proof, err := s.TestWorkspace(t.Context(), in)
	if err != nil || !proof.Passed {
		t.Fatal(proof, err)
	}
	g.pr, g.prBranch, g.remoteHead = 1, w.Fork.Branch, ws.Head
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, _ = currentForkInput(t, s)
	r := &retryFixtureSource{forkFixtureSource: g,
		run: ciWorkflowRun{ID: 120, Attempt: 1, Head: ws.Head, Branch: g.prBranch, Event: "pull_request", Status: "completed", Conclusion: "failure", Repository: ciRepository{"example/fixture"}, HeadRepository: ciRepository{"example/fixture"}, PullRequests: []ciPullRequest{{Number: 1, Head: ciPRRef{SHA: ws.Head, Ref: g.prBranch}, Base: ciPRRef{SHA: g.target, Ref: "main"}}}},
		job: ciWorkflowJob{ID: 240, RunID: 120, Attempt: 1, Head: ws.Head, Status: "completed", Conclusion: "failure"},
	}
	s.GH = r
	s.Config.ForkCIRetries = []ForkCIRetry{{ForkID: "custom", PR: 1, Head: ws.Head, Base: g.target, RunID: 120, RunAttempt: 1, JobID: 240}}
	return s, r, api, GitHubActionInput{WorkInput: in, Action: "rerun-ci", Head: ws.Head, Base: g.target, Ref: g.prBranch, RunID: 120, RunAttempt: 1, JobID: 240}
}

func TestCIRetryPreservesProofAndDoesNotGrantOtherWrites(t *testing.T) {
	s, g, _, in := ciRetryFixture(t)
	before, err := s.Store.ForkProgress(t.Context(), "fork:custom")
	if err != nil {
		t.Fatal(err)
	}
	guide, _ := s.Config.RepositoryGuide("example/fixture")
	check, err := s.CheckGitHubAction(t.Context(), in)
	if err != nil || !check.Permitted || check.Number != 1 || check.Repository != "example/fixture" || check.JobID != 240 || check.RunID != 120 || check.RunAttempt != 1 {
		t.Fatal("exact retry unavailable", check, err)
	}
	if !strings.Contains(check.Verification, "not an atomic reservation") || !strings.Contains(check.Verification, "dependent jobs") {
		t.Fatal("check obscures the external write's scope or race")
	}
	other := in
	other.Action, other.Ref = "merge", "main"
	if _, err = s.CheckGitHubAction(t.Context(), other); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("CI grant authorized merge", err)
	}
	after, err := s.Store.ForkProgress(t.Context(), "fork:custom")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("retry check changed progress or test proof", err)
	}
	s.Config.ForkCIRetries = nil
	afterGuide, _ := s.Config.RepositoryGuide("example/fixture")
	if guide.Version != afterGuide.Version {
		t.Fatal("transient CI grant invalidated source test contract")
	}
	if _, err = s.CheckGitHubAction(t.Context(), in); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("removed grant remained usable", err)
	}
	s.Config.ForkCIRetries = []ForkCIRetry{{ForkID: "custom", PR: 1, Head: in.Head, Base: in.Base, RunID: 120, RunAttempt: 1, JobID: 240}}
	g.run.Attempt = 2
	if _, err = s.CheckGitHubAction(t.Context(), in); err == nil {
		t.Fatal("grant replayed after an actual retry")
	}
}

func TestCIRetryRejectsStaleOrUnrelatedAuthority(t *testing.T) {
	cases := map[string]func(*Service, *retryFixtureSource, *taskAPI, *GitHubActionInput){
		"no grant": func(s *Service, _ *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) {
			s.Config.ForkCIRetries = nil
		},
		"another grant PR": func(s *Service, _ *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) {
			s.Config.ForkCIRetries[0].PR++
		},
		"wrong requested job": func(_ *Service, _ *retryFixtureSource, _ *taskAPI, in *GitHubActionInput) { in.JobID++ },
		"wrong requested run": func(_ *Service, _ *retryFixtureSource, _ *taskAPI, in *GitHubActionInput) { in.RunID++ },
		"wrong requested ref": func(_ *Service, _ *retryFixtureSource, _ *taskAPI, in *GitHubActionInput) { in.Ref = "main" },
		"closed PR":           func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) { g.closed = true },
		"PR head changed":     func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) { g.remoteHead = g.target },
		"owner changed":       func(_ *Service, _ *retryFixtureSource, api *taskAPI, _ *GitHubActionInput) { api.owner = "successor" },
		"owner changed in read": func(_ *Service, g *retryFixtureSource, api *taskAPI, _ *GitHubActionInput) {
			g.hook = func() { api.owner = "successor" }
		},
		"remote error": func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) {
			g.err = ErrGitHubAccessDenied
		},
		"wrong run":     func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) { g.run.ID++ },
		"wrong attempt": func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) { g.run.Attempt++ },
		"running": func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) {
			g.run.Status = "in_progress"
		},
		"successful": func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) {
			g.run.Conclusion = "success"
		},
		"target workflow": func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) {
			g.run.Event = "pull_request_target"
		},
		"wrong run head": func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) { g.run.Head = g.target },
		"wrong repository": func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) {
			g.run.Repository.FullName = "other/fixture"
		},
		"foreign source": func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) {
			g.run.HeadRepository.FullName = "other/fixture"
		},
		"no PR relation": func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) { g.run.PullRequests = nil },
		"wrong PR": func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) {
			g.run.PullRequests[0].Number++
		},
		"old base": func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) {
			g.run.PullRequests[0].Base.SHA = g.upstream
		},
		"wrong job":            func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) { g.job.ID++ },
		"job from another run": func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) { g.job.RunID++ },
		"job from old attempt": func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) { g.job.Attempt++ },
		"job from other head":  func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) { g.job.Head = g.target },
		"successful job": func(_ *Service, g *retryFixtureSource, _ *taskAPI, _ *GitHubActionInput) {
			g.job.Conclusion = "success"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s, g, api, in := ciRetryFixture(t)
			mutate(s, g, api, &in)
			if result, err := s.CheckGitHubAction(t.Context(), in); err == nil || result.Permitted {
				t.Fatal("unsafe retry accepted", result, err)
			}
		})
	}
}

func TestCIRetryConfigurationAndReadScope(t *testing.T) {
	s, _, _, in := ciRetryFixture(t)
	original := s.Config.ForkCIRetries[0]
	for _, mutate := range []func(*ForkCIRetry){
		func(g *ForkCIRetry) { g.ForkID = "unknown" }, func(g *ForkCIRetry) { g.PR = 0 },
		func(g *ForkCIRetry) { g.Head = "abc" }, func(g *ForkCIRetry) { g.Base = "abc" },
		func(g *ForkCIRetry) { g.RunID = -1 }, func(g *ForkCIRetry) { g.RunAttempt = 0 }, func(g *ForkCIRetry) { g.JobID = 0 },
	} {
		g := original
		mutate(&g)
		s.Config.ForkCIRetries = []ForkCIRetry{g}
		if err := s.Config.validateForks(); err == nil {
			t.Fatal("invalid grant accepted", g)
		}
	}
	s.Config.ForkCIRetries = []ForkCIRetry{original, original}
	if err := s.Config.validateForks(); err == nil {
		t.Fatal("ambiguous grants accepted")
	}
	s.Config.Repositories[0].GitHubActions = append(s.Config.Repositories[0].GitHubActions, "rerun-ci")
	if _, err := s.Config.RepositoryGuide("example/fixture"); err == nil {
		t.Fatal("general CI rerun authority accepted")
	}
	ordinary, _, _, ordinaryIn := delegatedFixture(t)
	ordinaryIn.Action, ordinaryIn.RunID, ordinaryIn.RunAttempt, ordinaryIn.JobID = in.Action, in.RunID, in.RunAttempt, in.JobID
	if _, err := ordinary.CheckGitHubAction(t.Context(), ordinaryIn); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("ordinary PR gained retry permission", err)
	}
	for path, want := range map[string]bool{
		"repos/example/fixture/actions/runs/120": true, "repos/example/fixture/actions/jobs/240": true,
		"repos/example/fixture/actions/runs/120/rerun": false, "repos/example/fixture/actions/jobs/240/rerun": false,
		"repos/example/fixture/actions/runs": false, "repos/example/fixture/actions/runs/120/attempts/1": false,
		"repos/example/fixture/actions/runs/0": false, "repos/example/../actions/runs/1": false,
	} {
		if validGitHubReadPath(path) != want {
			t.Fatal("wrong read scope", path)
		}
	}
}

func TestCIRetryGitHubReadOnlyAdapter(t *testing.T) {
	g, path := fakeGH(t, strings.Repeat("a", 40), strings.Repeat("b", 40))
	data := readFixture(t, path)
	data["repos/example/fixture/actions/jobs/240"] = ciWorkflowJob{ID: 240, RunID: 120, Attempt: 1}
	data["repos/example/fixture/actions/runs/120"] = ciWorkflowRun{ID: 120, Attempt: 1}
	writeFixture(t, path, data)
	run, job, err := g.CIRetryState(t.Context(), "example/fixture", 120, 240)
	if err != nil || run.ID != 120 || job.ID != 240 {
		t.Fatal(run, job, err)
	}
	// The fake gh rejects all non-GET calls. A private repository outside scope
	// must be rejected before consulting even a previously populated CI response.
	obj(data["repos/example/fixture"])["private"] = true
	writeFixture(t, path, data)
	if _, _, err = g.CIRetryState(t.Context(), "example/fixture", 120, 240); !errors.Is(err, ErrRepositoryNotAllowed) {
		t.Fatal("private CI read bypassed repository scope", err)
	}
}
