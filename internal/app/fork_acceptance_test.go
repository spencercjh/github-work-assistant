package app

import (
	"errors"
	"strings"
	"testing"
)

func TestForkMergeAcceptanceConfiguration(t *testing.T) {
	for _, scenario := range []string{"valid", "unknown fork", "missing PR", "short SHA", "duplicate", "original mode"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, _ := forkFixtureConfigured(t, true)
			a := ForkMergeAcceptance{ForkID: "custom", PR: 1, MergeCommit: strings.Repeat("a", 40)}
			switch scenario {
			case "unknown fork":
				a.ForkID = "unknown"
			case "missing PR":
				a.PR = 0
			case "short SHA":
				a.MergeCommit = "abcdef0"
			case "original mode":
				s.Config.Repositories[0].ForkSyncMode = ""
			}
			s.Config.ForkMergeAcceptances = []ForkMergeAcceptance{a}
			if scenario == "duplicate" {
				s.Config.ForkMergeAcceptances = append(s.Config.ForkMergeAcceptances, a)
			}
			if err := s.Config.validateForks(); (err == nil) != (scenario == "valid") {
				t.Fatalf("validation = %v for %s", err, scenario)
			}
		})
	}
}

func TestAcceptedHumanMergeCompletesOnlyWithCurrentProofAndOwner(t *testing.T) {
	for _, scenario := range []string{"valid", "no acceptance", "owner changed", "policy changed", "missing proof", "mapping changed"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, api, _, w, ws := beginAdaptedFixture(t)
			s.Config.Repositories[0].GitHubActions = []string{"push", "open-pr"}
			in, _ := currentForkInput(t, s)
			proof, err := s.TestWorkspace(t.Context(), in)
			if err != nil || !proof.Passed {
				t.Fatal(proof, err)
			}
			g.pr, g.prBranch, g.remoteHead = 1, w.Fork.Branch, ws.Head
			merge := syntheticCommit(t, g.repository, proof.Tree, g.target, ws.Head)
			if scenario != "no acceptance" {
				s.Config.ForkMergeAcceptances = []ForkMergeAcceptance{{ForkID: "custom", PR: 1, MergeCommit: merge}}
				g.mergeAcceptances = s.Config.ForkMergeAcceptances
			}
			if err = s.Collect(t.Context()); err != nil {
				t.Fatal(err)
			}
			in, current := currentForkInput(t, s)
			if current.Guide.Version != current.Fork.ValidatedGuide || current.Fork.SpecVersion != w.Fork.SpecVersion {
				t.Fatal("completion acceptance changed test contract or maintenance identity")
			}
			if _, err = s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: in, Action: "merge", Head: ws.Head, Base: g.target, Ref: "main"}); !errors.Is(err, ErrWorkDenied) {
				t.Fatal("acceptance granted merge authority", err)
			}
			g.mergeCommit, g.target, g.closed, g.merged = merge, merge, true, true
			if scenario == "policy changed" {
				s.Config.Repositories[0].Goal += " plus another requirement"
			}
			if scenario == "missing proof" {
				p, err := s.Store.ForkProgress(t.Context(), w.Event.Key)
				if err != nil {
					t.Fatal(err)
				}
				p.ValidatedTree = ""
				if err = s.Store.SaveForkProgress(t.Context(), w.Event.Key, p.Revision, p, nil); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "mapping changed" {
				body := "The tested source table has been removed."
				g.prBody = &body
			}
			if err = s.Collect(t.Context()); err != nil {
				t.Fatal(err)
			}
			in, _ = currentForkInput(t, s)
			if scenario == "owner changed" {
				api.owner = "successor"
			}
			err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "merged", Summary: "Accepted human merge matches the tested parents and tree"})
			want := scenario == "valid"
			if (err == nil) != want {
				t.Fatalf("completion = %v, want success %v", err, want)
			}
			p, err := s.Store.ForkProgress(t.Context(), w.Event.Key)
			if err != nil || (p.LastSuccess == w.Fork.Upstream) != want {
				t.Fatal("invalid completion changed cursor", p, err)
			}
			if want {
				// Removing the one-use configuration cannot invalidate a completed
				// round, and does not manufacture upstream commit ancestry.
				s.Config.ForkMergeAcceptances, g.mergeAcceptances = nil, nil
				if err = s.Collect(t.Context()); err != nil {
					t.Fatal(err)
				}
				_, done := currentForkInput(t, s)
				if done.Fork.Phase != "synced" || forkFacts(done)["merge_verified"] != true || forkFacts(done)["contains_pinned"] == true {
					t.Fatal("completed history changed after removing acceptance")
				}
			}
		})
	}
}
