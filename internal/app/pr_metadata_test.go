package app

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestPRMetadataRequiresSeparateDelegationAndOpenPR(t *testing.T) {
	for _, scenario := range []string{"open", "draft", "undelegated", "closed", "merged", "issue", "old head", "old base", "old guide", "other owner", "done"} {
		t.Run(scenario, func(t *testing.T) {
			s, api, path, in := delegatedFixture(t)
			s.Config.Repositories[0].GitHubActions = []string{"edit-pr"}
			data := readFixture(t, path)
			pull := obj(data["repos/example/fixture/pulls/1"])
			switch scenario {
			case "draft":
				pull["draft"] = true
			case "undelegated":
				s.Config.Repositories[0].GitHubActions = []string{"open-pr"}
			case "closed":
				pull["state"] = "closed"
			case "merged":
				pull["merged"] = true
			case "issue":
				delete(obj(data["repos/example/fixture/issues/1"]), "pull_request")
			}
			writeFixture(t, path, data)
			if _, err := s.RefreshWorkContext(t.Context(), in.EventID); err != nil {
				t.Fatal(err)
			}
			ev := currentEvent(t, s)
			deliverEvent(t, s, ev)
			w, err := s.WorkContext(t.Context(), ev.ID)
			if err != nil {
				t.Fatal(err)
			}
			in.EventID, in.EvidenceVersion, in.GuideVersion, in.Action = ev.ID, w.CurrentVersion, w.Guide.Version, "edit-pr"
			if _, err = s.LinkTask(t.Context(), in.WorkInput); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "issue":
				in.Head, in.Base = "", ""
			case "old head":
				obj(pull["head"])["sha"] = strings.Repeat("c", 40)
				writeFixture(t, path, data)
			case "old base":
				obj(pull["base"])["sha"] = strings.Repeat("d", 40)
				writeFixture(t, path, data)
			case "old guide":
				s.Config.Repositories[0].Goal += " changed"
			case "other owner":
				api.owner = "successor"
			case "done":
				api.status = "done"
			}
			got, err := s.CheckGitHubAction(t.Context(), in)
			if scenario != "open" && scenario != "draft" {
				if err == nil || got.Permitted {
					t.Fatal("out-of-scope metadata edit allowed", got, err)
				}
				return
			}
			if err != nil || !got.Permitted || got.Repository != "example/fixture" || got.Number != 1 || !slices.Equal(got.EditableFields, []string{"title", "body"}) {
				t.Fatal("title/body delegation unavailable", got, err)
			}
			in.Action = "merge"
			if _, err = s.CheckGitHubAction(t.Context(), in); !errors.Is(err, ErrWorkDenied) {
				t.Fatal("metadata permission allowed merging", err)
			}
		})
	}
}

func TestForkMetadataUpdatesWithoutWorkspaceOrTestProof(t *testing.T) {
	for _, adapted := range []bool{false, true} {
		s, g, _ := forkFixtureConfigured(t, adapted)
		s.Config.Repositories[0].GitHubActions = []string{"push", "open-pr", "edit-pr"}
		in, w := currentForkInput(t, s)
		if adapted {
			after := fixtureGit(t, g.repository, "", "rev-parse", g.upstream+"^")
			if _, err := s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, After: after, Count: 1, Reason: "Port oldest change"}); err != nil {
				t.Fatal(err)
			}
			in, w = currentForkInput(t, s)
		}
		g.pr, g.prBranch, g.remoteHead, g.checks = 7, w.Fork.Branch, g.upstream, "failure"
		if err := s.Collect(t.Context()); err != nil {
			t.Fatal(err)
		}
		in, w = currentForkInput(t, s)
		before := canonical(w.Fork)
		check := GitHubActionInput{WorkInput: in, Action: "edit-pr", Head: g.remoteHead, Base: g.target, Ref: w.Fork.Branch}
		got, err := s.CheckGitHubAction(t.Context(), check)
		if err != nil || !got.Permitted || got.Number != 7 || got.Branch != w.Fork.Branch || got.TargetBranch != "main" || !slices.Equal(got.EditableFields, []string{"title", "body"}) {
			t.Fatal("metadata requires irrelevant source testing", got, err)
		}
		after, err := s.WorkContext(t.Context(), in.EventID)
		if err != nil || string(before) != string(canonical(after.Fork)) {
			t.Fatal("metadata check changed progress or test evidence", err)
		}
		for _, action := range []string{"push", "open-pr", "merge"} {
			check.Action = action
			if _, err = s.CheckGitHubAction(t.Context(), check); err == nil {
				t.Fatal("metadata check granted code write or merge", action)
			}
		}
	}
}

func TestForkMetadataRejectsWrongPRAndStaleAuthority(t *testing.T) {
	for _, scenario := range []string{"undelegated", "missing PR", "closed", "merged", "old event", "old guide", "other owner", "wrong head", "wrong base", "wrong ref", "wrong PR head", "wrong PR base", "wrong source branch", "wrong source repository", "wrong target branch"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, api := forkFixture(t)
			s.Config.Repositories[0].GitHubActions = []string{"edit-pr"}
			_, w := currentForkInput(t, s)
			g.pr, g.prBranch, g.remoteHead = 7, w.Fork.Branch, g.upstream
			switch scenario {
			case "undelegated":
				s.Config.Repositories[0].GitHubActions = []string{"push", "open-pr"}
			case "missing PR":
				g.pr = 0
			case "closed":
				g.closed = true
			case "merged":
				g.merged = true
			}
			if err := s.Collect(t.Context()); err != nil {
				t.Fatal(err)
			}
			in, w := currentForkInput(t, s)
			check := GitHubActionInput{WorkInput: in, Action: "edit-pr", Head: g.remoteHead, Base: g.target, Ref: w.Fork.Branch}
			switch scenario {
			case "old event":
				g.checks = "new check result"
			case "old guide":
				s.Config.Repositories[0].GitHubActions = nil
			case "other owner":
				api.owner = "successor"
			case "wrong head":
				check.Head = g.target
			case "wrong base":
				check.Base = g.upstream
			case "wrong ref":
				check.Ref = "main"
			}
			// Validate the scope check independently of the collector's own
			// rejection of a PR retargeted or moved to another repository.
			fields := map[string]string{"wrong PR head": "head", "wrong PR base": "base_sha", "wrong source branch": "head_ref", "wrong source repository": "head_repo", "wrong target branch": "base"}
			if field, ok := fields[scenario]; ok {
				var snapshot Snapshot
				if err := json.Unmarshal(w.Snapshot, &snapshot); err != nil {
					t.Fatal(err)
				}
				snapshot.Objects["pull"][field] = "other"
				w.Snapshot = canonical(snapshot)
				if got, err := s.checkForkAction(t.Context(), w, check); err == nil || got.Permitted {
					t.Fatal("wrong PR identity or revision accepted", got, err)
				}
				return
			}
			if got, err := s.CheckGitHubAction(t.Context(), check); err == nil || got.Permitted {
				t.Fatal("out-of-scope fork metadata edit allowed", got, err)
			}
		})
	}
}
