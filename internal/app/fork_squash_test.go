package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdaptedSquashCompletesExistingProofAfterGuideUpgrade(t *testing.T) {
	for _, scenario := range []string{"unchanged policy", "changed goal", "changed tests", "changed permissions", "changed guide file"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, _, in, w, ws := beginAdaptedFixture(t)
			proof, err := s.TestWorkspace(t.Context(), in)
			if err != nil {
				t.Fatal(err)
			}
			d, err := s.Config.WorkDefinition(w.Event.Key)
			if err != nil {
				t.Fatal(err)
			}
			oldGuide, err := d.guide(s.Config, legacyAdaptedMergeInstruction)
			if err != nil || oldGuide.Version == in.GuideVersion {
				t.Fatal("fixture did not model the guide upgrade", err)
			}
			p, err := s.Store.ForkProgress(t.Context(), w.Event.Key)
			if err != nil {
				t.Fatal(err)
			}
			p.ValidatedGuide = oldGuide.Version
			if err = s.Store.SaveForkProgress(t.Context(), w.Event.Key, p.Revision, p, nil); err != nil {
				t.Fatal(err)
			}
			// Old proof is valid only for recording an actual merge, never a write.
			check := GitHubActionInput{WorkInput: in, Action: "push", Head: ws.Head, Base: g.target, Ref: w.Fork.Branch}
			if _, err = s.CheckGitHubAction(t.Context(), check); err == nil {
				t.Fatal("old guide authorized a new write")
			}
			switch scenario {
			case "changed goal":
				s.Config.Repositories[0].Goal += " with another requirement"
			case "changed tests":
				s.Config.Repositories[0].TestCommands = [][]string{{"python3", "-c", "raise SystemExit(1)"}}
			case "changed permissions":
				s.Config.Repositories[0].GitHubActions = []string{"push", "open-pr"}
			case "changed guide file":
				if err := os.WriteFile(filepath.Join(s.Config.configDir, s.Config.Repositories[0].GuideFile), []byte("Additional required behavior"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			g.pr, g.prBranch, g.remoteHead = 1, w.Fork.Branch, ws.Head
			g.mergeCommit = syntheticCommit(t, g.repository, proof.Tree, g.target)
			g.target, g.closed, g.merged = g.mergeCommit, true, true
			if err = s.Collect(t.Context()); err != nil {
				t.Fatal(err)
			}
			in, before := currentForkInput(t, s)
			if forkFacts(before)["contains_validated"] != false || forkFacts(before)["merge_verified"] != true {
				t.Fatal("squash fixture did not separate ancestry from verified contents")
			}
			err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "merged", Summary: "Human squash matches the tested baseline and tree"})
			want := scenario == "unchanged policy"
			if (err == nil) != want {
				t.Fatalf("completion = %v, want success %v", err, want)
			}
			after, err := s.Store.ForkProgress(t.Context(), w.Event.Key)
			if err != nil || (after.LastSuccess == p.Upstream) != want || after.ValidatedGuide != oldGuide.Version || after.TestLogHash != p.TestLogHash {
				t.Fatal("completion changed proof or advanced an invalid cursor", err)
			}
		})
	}
}

func TestForkMergeInstructionsFollowSyncMode(t *testing.T) {
	for _, adapted := range []bool{false, true} {
		t.Run(map[bool]string{false: "original commits", true: "adapted commits"}[adapted], func(t *testing.T) {
			var s *Service
			var g *forkFixtureSource
			var in WorkInput
			var w WorkContext
			var ws Workspace
			if adapted {
				s, g, _, in, w, ws = beginAdaptedFixture(t)
			} else {
				s, g, in, w, ws = testedFork(t, []string{"push", "open-pr", "merge"})
			}
			if _, err := s.TestWorkspace(t.Context(), in); err != nil {
				t.Fatal(err)
			}
			g.pr, g.prBranch, g.remoteHead = 1, w.Fork.Branch, ws.Head
			if err := s.Collect(t.Context()); err != nil {
				t.Fatal(err)
			}
			in, w = currentForkInput(t, s)
			got, err := s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: in, Action: "merge", Head: ws.Head, Base: g.target, Ref: "main"})
			if err != nil {
				t.Fatal(err)
			}
			method := "a merge commit"
			if adapted {
				method = "squash"
			}
			if !strings.Contains(got.Verification, "merge this PR using "+method) || !strings.Contains(w.Guide.Text, method) {
				t.Fatal("merge method disagrees with synchronization mode", got.Verification)
			}
		})
	}
}
