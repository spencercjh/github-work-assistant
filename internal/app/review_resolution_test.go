package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type reviewFixtureSource struct {
	*forkFixtureSource
	prState reviewPR
	thread  reviewThread
	err     error
	hook    func()
}

func (g *reviewFixtureSource) ReviewThread(context.Context, string, int, string) (reviewPR, reviewThread, error) {
	if g.hook != nil {
		g.hook()
	}
	return g.prState, g.thread, g.err
}

func reviewResolutionFixture(t *testing.T, adapted bool) (*Service, *reviewFixtureSource, *taskAPI, GitHubActionInput) {
	t.Helper()
	var s *Service
	var g *forkFixtureSource
	var api *taskAPI
	var ws Workspace
	var w WorkContext
	if adapted {
		s, g, api, _, w, ws = beginAdaptedFixture(t)
	} else {
		s, g, api = forkFixture(t)
		in, work := currentForkInput(t, s)
		w = work
		var err error
		ws, err = s.PrepareWorkspace(t.Context(), in, g.repository)
		if err != nil {
			t.Fatal(err)
		}
	}
	s.Config.Repositories[0].GitHubActions = []string{"push", "open-pr"}
	s.Config.Repositories[0].ResolveReviewThreads = true
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
	resolved := false
	r := &reviewFixtureSource{forkFixtureSource: g,
		prState: reviewPR{Number: 1, State: "OPEN", Head: ws.Head, Base: g.target, HeadRef: g.prBranch, BaseRef: "main", Repository: reviewRepository{"example/fixture"}, HeadRepository: reviewRepository{"example/fixture"}},
		thread:  reviewThread{ID: "PRRT_fixture", IsResolved: &resolved},
	}
	s.GH = r
	return s, r, api, GitHubActionInput{WorkInput: in, Action: "resolve-review-thread", Head: ws.Head, Base: g.target, Ref: g.prBranch, ReviewThreadID: r.thread.ID, Body: "The published change preserves the fork offset while applying the reviewed scale correction. Both behavioral suites passed."}
}

func TestReviewResolutionRequiresOptInAndPreservesProof(t *testing.T) {
	for _, adapted := range []bool{false, true} {
		t.Run(map[bool]string{false: "original", true: "adapted"}[adapted], func(t *testing.T) {
			s, g, _, in := reviewResolutionFixture(t, adapted)
			before, err := s.Store.ForkProgress(t.Context(), "fork:custom")
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.CheckGitHubAction(t.Context(), in)
			if err != nil || !got.Permitted || got.ReviewThreadID != g.thread.ID || got.Number != 1 || got.Repository != "example/fixture" || got.Head != in.Head {
				t.Fatal(got, err)
			}
			after, err := s.Store.ForkProgress(t.Context(), "fork:custom")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("preflight changed proof or progress", err)
			}
			if !strings.Contains(got.Verification, "not an atomic reservation") || !strings.Contains(got.Verification, "Outdated status alone") {
				t.Fatal("missing semantic/race limitation")
			}
			other := in
			other.Action = "merge"
			other.Ref = "main"
			if _, err = s.CheckGitHubAction(t.Context(), other); !errors.Is(err, ErrWorkDenied) {
				t.Fatal("resolution granted merge", err)
			}
			s.Config.Repositories[0].ResolveReviewThreads = false
			in.WorkInput, _ = currentForkInput(t, s)
			if got, err = s.CheckGitHubAction(t.Context(), in); !errors.Is(err, ErrWorkDenied) || got.Permitted {
				t.Fatal("disabled switch allowed resolution", got, err)
			}
		})
	}
}

func TestReviewResolutionRejectsStaleOrUnrelatedWork(t *testing.T) {
	cases := map[string]func(*Service, *reviewFixtureSource, *taskAPI, *GitHubActionInput){
		"no thread":        func(_ *Service, _ *reviewFixtureSource, _ *taskAPI, in *GitHubActionInput) { in.ReviewThreadID = "" },
		"no explanation":   func(_ *Service, _ *reviewFixtureSource, _ *taskAPI, in *GitHubActionInput) { in.Body = " \n" },
		"other ref":        func(_ *Service, _ *reviewFixtureSource, _ *taskAPI, in *GitHubActionInput) { in.Ref = "main" },
		"unpublished head": func(_ *Service, g *reviewFixtureSource, _ *taskAPI, _ *GitHubActionInput) { g.remoteHead = g.target },
		"closed":           func(_ *Service, g *reviewFixtureSource, _ *taskAPI, _ *GitHubActionInput) { g.closed = true },
		"access revoked":   func(_ *Service, g *reviewFixtureSource, _ *taskAPI, _ *GitHubActionInput) { g.denied = true },
		"thread read failure": func(_ *Service, g *reviewFixtureSource, _ *taskAPI, _ *GitHubActionInput) {
			g.err = ErrGitHubAccessDenied
		},
		"thread mismatch": func(_ *Service, g *reviewFixtureSource, _ *taskAPI, _ *GitHubActionInput) { g.thread.ID = "PRRT_other" },
		"resolved": func(_ *Service, g *reviewFixtureSource, _ *taskAPI, _ *GitHubActionInput) {
			*g.thread.IsResolved = true
		},
		"unknown state": func(_ *Service, g *reviewFixtureSource, _ *taskAPI, _ *GitHubActionInput) { g.thread.IsResolved = nil },
		"other PR":      func(_ *Service, g *reviewFixtureSource, _ *taskAPI, _ *GitHubActionInput) { g.prState.Number++ },
		"other repo": func(_ *Service, g *reviewFixtureSource, _ *taskAPI, _ *GitHubActionInput) {
			g.prState.Repository.Name = "other/private"
		},
		"foreign head repo": func(_ *Service, g *reviewFixtureSource, _ *taskAPI, _ *GitHubActionInput) {
			g.prState.HeadRepository.Name = "other/private"
		},
		"head changed during read": func(_ *Service, g *reviewFixtureSource, _ *taskAPI, _ *GitHubActionInput) { g.prState.Head = g.target },
		"base changed during read": func(_ *Service, g *reviewFixtureSource, _ *taskAPI, _ *GitHubActionInput) {
			g.prState.Base = g.upstream
		},
		"merged during read": func(_ *Service, g *reviewFixtureSource, _ *taskAPI, _ *GitHubActionInput) { g.prState.State = "MERGED" },
		"owner transfer during read": func(_ *Service, g *reviewFixtureSource, a *taskAPI, _ *GitHubActionInput) {
			g.hook = func() { a.owner = "successor" }
		},
		"switch revoked during read": func(s *Service, g *reviewFixtureSource, _ *taskAPI, _ *GitHubActionInput) {
			g.hook = func() { s.Config.Repositories[0].ResolveReviewThreads = false }
		},
		"new guide without retest": func(s *Service, _ *reviewFixtureSource, _ *taskAPI, in *GitHubActionInput) {
			s.Config.Repositories[0].Goal += " updated"
			in.WorkInput, _ = currentForkInput(t, s)
		},
		"proof revoked during read": func(s *Service, g *reviewFixtureSource, _ *taskAPI, _ *GitHubActionInput) {
			g.hook = func() {
				p, err := s.Store.ForkProgress(t.Context(), "fork:custom")
				if err != nil {
					t.Fatal(err)
				}
				p.ValidatedHead = ""
				if err = s.Store.SaveForkProgress(t.Context(), "fork:custom", p.Revision, p, nil); err != nil {
					t.Fatal(err)
				}
			}
		},
		"dirty tested tree": func(s *Service, _ *reviewFixtureSource, _ *taskAPI, in *GitHubActionInput) {
			w, e := s.WorkContext(t.Context(), in.EventID)
			if e != nil {
				t.Fatal(e)
			}
			ws, e := s.verifyForkWorkspace(t.Context(), w, in.WorkInput)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(ws.Path, "scale.py"), []byte("scale=999\n"), 0600); e != nil {
				t.Fatal(e)
			}
		},
	}
	for name, alter := range cases {
		t.Run(name, func(t *testing.T) {
			s, g, a, in := reviewResolutionFixture(t, true)
			alter(s, g, a, &in)
			if got, err := s.CheckGitHubAction(t.Context(), in); err == nil || got.Permitted {
				t.Fatal("unsafe resolution accepted", got, err)
			}
		})
	}
}

func TestReviewResolutionSwitchCannotAuthorizeOrdinaryPR(t *testing.T) {
	s, _, _, in := delegatedFixture(t)
	s.Config.Repositories[0].ResolveReviewThreads = true
	guide, err := s.Config.RepositoryGuide("example/fixture")
	if err != nil {
		t.Fatal(err)
	}
	in.GuideVersion = guide.Version
	if _, err = s.LinkTask(t.Context(), in.WorkInput); err != nil {
		t.Fatal(err)
	}
	in.Action = "resolve-review-thread"
	in.ReviewThreadID = "PRRT_fixture"
	in.Body = "Fixed and checked."
	if _, err = s.CheckGitHubAction(t.Context(), in); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("ordinary PR gained fork-only permission", err)
	}
}

func TestReviewResolutionPolicyDefaultsAndIsolation(t *testing.T) {
	c := localPolicy(t, "local-edit")
	old, err := c.RepositoryGuide("example/fixture")
	if err != nil || old.ResolveReviewThreads || strings.Contains(string(canonical(old)), "resolve_review_threads") {
		t.Fatal(old, err)
	}
	c.Repositories[0].ResolveReviewThreads = true
	enabled, err := c.RepositoryGuide("example/fixture")
	if err != nil || !enabled.ResolveReviewThreads || old.Version == enabled.Version {
		t.Fatal(enabled, err)
	}
	other, err := c.RepositoryGuide("other/repo")
	if err != nil || other.ResolveReviewThreads {
		t.Fatal("switch leaked to another repo", err)
	}
	c.Repositories[0].ResolveReviewThreads = false
	disabled, _ := c.RepositoryGuide("example/fixture")
	if disabled.Version != old.Version {
		t.Fatal("default-off changed existing guide version")
	}
	c.Repositories[0].ResolveReviewThreads = true
	c.Repositories[0].Mode = "read-only"
	if err = c.Repositories[0].validate(); err == nil {
		t.Fatal("read-only switch accepted")
	}
	c.Repositories[0].Repo = "Project-HAMi/HAMi"
	if err = c.Repositories[0].validate(); err == nil {
		t.Fatal("community exception accepted")
	}
}
