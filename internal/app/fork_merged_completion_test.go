package app

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// Finish a real adapted merge while a later upstream commit is already known.
func mergedAdaptedFixture(t *testing.T) (*Service, *forkFixtureSource, *taskAPI, WorkInput, WorkInput) {
	t.Helper()
	s, g, owner, old, w, ws := beginAdaptedFixture(t)
	proof, err := s.TestWorkspace(t.Context(), old)
	if err != nil {
		t.Fatal(err)
	}
	g.pr, g.prBranch, g.remoteHead = 1, w.Fork.Branch, ws.Head
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	old, _ = currentForkInput(t, s)
	if err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: old, ReportID: fixtureReport, Outcome: "waiting", Summary: "Await human review"}); err != nil {
		t.Fatal(err)
	}
	g.mergeCommit = syntheticCommit(t, g.repository, proof.Tree, g.target, ws.Head)
	g.target, g.closed, g.merged = g.mergeCommit, true, true
	tree := fixtureGit(t, g.repository, "", "rev-parse", g.upstream+"^{tree}")
	g.upstream = syntheticCommit(t, g.repository, tree, g.upstream)
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	merged, _ := currentForkInput(t, s)
	if err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: merged, ReportID: fixtureReport, Outcome: "merged", Summary: "Verified human merge parents and tested tree"}); err != nil {
		t.Fatal(err)
	}
	return s, g, owner, old, merged
}

func TestAdaptedMergeDoesNotInvalidateItsOwnReport(t *testing.T) {
	s, g, _, _, merged := mergedAdaptedFixture(t)
	for range 2 {
		if err := s.Collect(t.Context()); err != nil {
			t.Fatal(err)
		}
		w, err := s.WorkContext(t.Context(), merged.EventID)
		if err != nil || w.Stale || w.CurrentEventID != merged.EventID || w.Fork.Phase != "synced" || w.Fork.ObservedUpstream != g.upstream {
			t.Fatal("recording a verified merge created new work or invalidated its report", w.CurrentEventID, err)
		}
	}
	// Real subsequent changes must still be delivered.
	g.checks = "new failure"
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	w, err := s.WorkContext(t.Context(), merged.EventID)
	if err != nil || !w.Stale || w.CurrentEventID == merged.EventID {
		t.Fatal("real check update lost", err)
	}
}

// Reproduce the notification already emitted by older releases, without
// rewriting its saved report or event during the upgrade.
func legacyMergedNotification(t *testing.T, s *Service, g *forkFixtureSource) WorkInput {
	t.Helper()
	f := s.Config.Forks[0]
	p, err := s.Store.ForkProgress(t.Context(), f.Key())
	if err != nil {
		t.Fatal(err)
	}
	o, err := g.CollectFork(t.Context(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.(*Store).Observe(t.Context(), f.Key(), forkSnapshot(f, p, o)); err != nil {
		t.Fatal(err)
	}
	in, _ := currentForkInput(t, s)
	return in
}

func TestSyncedMergeCanReconcileExistingNotifications(t *testing.T) {
	s, g, owner, _, merged := mergedAdaptedFixture(t)
	current := legacyMergedNotification(t, s, g)
	if current.EventID == merged.EventID {
		t.Fatal("fixture did not reproduce progress notification")
	}
	before, err := s.Store.ForkProgress(t.Context(), s.Config.Forks[0].Key())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: merged, ReportID: fixtureReport, Outcome: "merged", Summary: "Old evidence"}); !errors.Is(err, ErrStaleWork) {
		t.Fatal("stale report accepted", err)
	}
	report := WorkResult{WorkInput: current, ReportID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", Outcome: "merged", Summary: "Rechecked the same completed merge and remaining earlier events"}
	if err := s.RecordWorkResult(t.Context(), report); err != nil {
		t.Fatal("cannot reconfirm the completed merge", err)
	}
	after, err := s.Store.ForkProgress(t.Context(), s.Config.Forks[0].Key())
	if err != nil {
		t.Fatal(err)
	}
	after.Revision = before.Revision
	if !reflect.DeepEqual(before, after) {
		t.Fatal("reconfirmation changed the completed round", before, after)
	}
	pending, err := s.Store.UnprocessedPage(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range pending {
		if row["resource_key"] != s.Config.Forks[0].Key() {
			continue
		}
		if err := s.RecordTriage(t.Context(), Triage{Executor: current.Executor, EventID: row["event_id"].(string), Outcome: "superseded", SupersededBy: current.EventID, ReportID: report.ReportID, Reason: "Verified report covers this earlier update"}); err != nil {
			t.Fatal(err)
		}
	}
	var calls []string
	if got, err := s.FinishWork(t.Context(), current, updateFixtureTask(t, owner, &calls)); err != nil || got.Status != "in_review" {
		t.Fatal("could not finish covered work", got, err)
	}
	history, err := s.Store.History(t.Context(), s.Config.Forks[0].Key())
	if err != nil {
		t.Fatal(err)
	}
	original := false
	for _, raw := range history {
		var saved WorkResult
		if json.Unmarshal(raw, &saved) == nil && saved.EventID == merged.EventID && saved.ReportID == fixtureReport && saved.Outcome == "merged" {
			original = true
		}
	}
	if !original {
		t.Fatal("original merged report lost")
	}
	// Restarts and repeated finishing preserve both the result and ownership.
	reopened := openTestStore(t)
	s.Store, s.Raft.Store = reopened, reopened
	if _, err = s.FinishWork(t.Context(), current, updateFixtureTask(t, owner, &calls)); err != nil || len(calls) != 1 {
		t.Fatal("restart lost or repeated completion", err, calls)
	}
}

func TestSyncedMergeReconfirmationRequiresFreshProofAndAuthority(t *testing.T) {
	for _, scenario := range []string{"owner", "guide", "mode", "access", "cursor", "proof", "refresh failure", "stale event"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, owner, _, _ := mergedAdaptedFixture(t)
			in := legacyMergedNotification(t, s, g)
			switch scenario {
			case "owner":
				owner.owner = "successor"
			case "guide":
				s.Config.Repositories[0].Goal += " changed"
			case "mode":
				s.Config.Repositories[0].ForkSyncMode = ""
			case "access":
				g.denied = true
			case "refresh failure":
				g.accessErr = errors.New("remote unavailable")
			case "stale event":
				g.checks = "new failure"
			case "cursor", "proof":
				p, err := s.Store.ForkProgress(t.Context(), s.Config.Forks[0].Key())
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "cursor" {
					p.LastSuccess = ""
				} else {
					p.ValidatedBase = g.target
				}
				if err = s.Store.SaveForkProgress(t.Context(), s.Config.Forks[0].Key(), p.Revision, p, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", Outcome: "merged", Summary: "Reconfirm merge"}); err == nil {
				t.Fatal("accepted revoked or unverified completion", scenario)
			}
		})
	}
}

func TestCompletedForkStillDeniesWritesAndNonMergeReports(t *testing.T) {
	s, g, _, _, _ := mergedAdaptedFixture(t)
	in := legacyMergedNotification(t, s, g)
	w, err := s.WorkContext(t.Context(), in.EventID)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"push", "open-pr", "merge", "edit-pr"} {
		ref := w.Fork.Branch
		if action == "merge" {
			ref = "main"
		}
		if _, err = s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: in, Action: action, Head: w.Fork.ValidatedHead, Base: g.target, Ref: ref}); err == nil {
			t.Fatal("completed round allowed a write", action)
		}
	}
	for _, outcome := range []string{"waiting", "changes-ready", "investigated"} {
		if err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: outcome, Summary: "Should not resume completed work"}); err == nil {
			t.Fatal("resumed completed round", outcome)
		}
	}
}

func TestCompletedRawForkCanReconfirmRealFollowup(t *testing.T) {
	s, g, _, _, in := forkCompletionFixture(t)
	p, err := s.Store.ForkProgress(t.Context(), s.Config.Forks[0].Key())
	if err != nil {
		t.Fatal(err)
	}
	g.mergeCommit = syntheticCommit(t, g.repository, p.ValidatedTree, g.target, p.ValidatedHead)
	g.target, g.closed, g.merged = g.mergeCommit, true, true
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, _ = currentForkInput(t, s)
	if err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "merged", Summary: "Verified upstream merge"}); err != nil {
		t.Fatal(err)
	}
	g.checks = "late build passed"
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, _ := currentForkInput(t, s)
	if current.EventID == in.EventID {
		t.Fatal("real follow-up did not produce an event")
	}
	if err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: current, ReportID: fixtureReport, Outcome: "merged", Summary: "Reviewed the late build; merge proof still matches"}); err != nil {
		t.Fatal(err)
	}
}
