package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func forkCompletionFixture(t *testing.T) (*Service, *forkFixtureSource, *taskAPI, WorkInput, WorkInput) {
	t.Helper()
	s, g, owner := forkFixture(t)
	first, w := currentForkInput(t, s)
	if err := s.RecordWorkResult(t.Context(), WorkResult{WorkInput: first, ReportID: fixtureReport, Outcome: "waiting", Summary: "Waiting for validation"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareWorkspace(t.Context(), first, g.repository); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TestWorkspace(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	g.pr, g.prBranch, g.remoteHead, g.checks = 1, w.Fork.Branch, w.Fork.Upstream, "pending"
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	old, _ := currentForkInput(t, s)
	g.checks = "success"
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	latest, _ := currentForkInput(t, s)
	if err := s.RecordWorkResult(t.Context(), WorkResult{WorkInput: latest, ReportID: fixtureReport, Outcome: "changes-ready", Summary: "Validated changes and all earlier checks; ready for review"}); err != nil {
		t.Fatal(err)
	}
	return s, g, owner, old, latest
}

func TestForkCompletionExplicitlyCoversOlderEvents(t *testing.T) {
	s, _, owner, old, latest := forkCompletionFixture(t)
	var calls []string
	update := updateFixtureTask(t, owner, &calls)
	if _, err := s.FinishWork(t.Context(), latest, update); err == nil || len(calls) != 0 {
		t.Fatal("unacknowledged older event did not block completion", err)
	}
	if err := s.RecordWorkResult(t.Context(), WorkResult{WorkInput: old, ReportID: fixtureReport, Outcome: "changes-ready", Summary: "Old evidence"}); !errors.Is(err, ErrStaleWork) {
		t.Fatal("stale reports must remain forbidden", err)
	}
	before, err := s.Store.ForkProgress(t.Context(), s.Config.Forks[0].Key())
	if err != nil {
		t.Fatal(err)
	}
	triage := Triage{Executor: "@worker", EventID: old.EventID, Outcome: "superseded", Reason: "The latest validated report covers this earlier check", SupersededBy: latest.EventID, ReportID: fixtureReport}
	for range 2 {
		if err := s.RecordTriage(t.Context(), triage); err != nil {
			t.Fatal("cannot acknowledge superseded evidence", err)
		}
	}
	rows, err := s.Store.UnprocessedPage(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row["resource_key"] == s.Config.Forks[0].Key() {
			t.Fatal("covered event remained pending", row)
		}
	}
	got, err := s.FinishWork(t.Context(), latest, update)
	if err != nil || got.Status != "in_progress" || owner.status != "in_progress" {
		t.Fatal("review-ready fork did not finish", got, err)
	}
	after, err := s.Store.ForkProgress(t.Context(), s.Config.Forks[0].Key())
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("coverage changed fork progress or claimed a merge", err)
	}
	if rows, err := s.Store.History(t.Context(), s.Config.Forks[0].Key()); err != nil || len(rows) != 3 {
		t.Fatal("old waiting or current result was lost", len(rows), err)
	}
	s.Store, s.Raft.Store = openTestStore(t), openTestStore(t)
	if _, err := s.FinishWork(t.Context(), latest, update); err != nil || len(calls) != 0 {
		t.Fatal("restart repeated or lost completion", err, calls)
	}
}

func TestForkCoverageRejectsUnresolvedOrUnrelatedEvidence(t *testing.T) {
	for _, boundary := range []string{"waiting", "needs-decision", "foreign report owner", "foreign report task", "foreign report guide", "missing report", "self", "old pending", "cover pending", "explicit request", "compacted event", "new event", "other owner", "human", "done", "closed", "in_review", "revoked", "guide changed", "different work"} {
		t.Run(boundary, func(t *testing.T) {
			s, g, owner, old, latest := forkCompletionFixture(t)
			store := s.Store.(*Store)
			triage := Triage{Executor: "@worker", EventID: old.EventID, Outcome: "superseded", SupersededBy: latest.EventID, ReportID: fixtureReport, Reason: "Covered by this report"}
			otherReport := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			switch boundary {
			case "waiting", "needs-decision", "foreign report owner", "foreign report task", "foreign report guide":
				r := WorkResult{WorkInput: latest, Outcome: "changes-ready", ReportID: otherReport, Summary: "Another report"}
				switch boundary {
				case "waiting", "needs-decision":
					r.Outcome = boundary
				case "foreign report owner":
					r.Executor = "@former"
				case "foreign report task":
					r.TaskID = otherReport
				case "foreign report guide":
					r.GuideVersion = "old-guide"
				}
				if err := store.SaveWorkResult(t.Context(), s.Config.Forks[0].Key(), r); err != nil {
					t.Fatal(err)
				}
				triage.ReportID = otherReport
			case "missing report":
				triage.ReportID = otherReport
			case "self":
				triage.EventID = latest.EventID
			case "old pending", "cover pending":
				id := old.EventID
				if boundary == "cover pending" {
					id = latest.EventID
				}
				if _, err := store.db.ExecContext(t.Context(), "UPDATE ghraft_events SET state='pending' WHERE event_id=?", id); err != nil {
					t.Fatal(err)
				}
			case "explicit request":
				ev, err := store.RequestWork(t.Context(), s.Config.Forks[0].Key(), latest.EvidenceVersion, otherReport, "Follow up a decision")
				if err != nil {
					t.Fatal(err)
				}
				deliverEvent(t, s, ev)
				g.checks = "completed"
				if err = s.Collect(t.Context()); err != nil {
					t.Fatal(err)
				}
				latest, _ = currentForkInput(t, s)
				if err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: latest, ReportID: fixtureReport, Outcome: "changes-ready", Summary: "Current work"}); err != nil {
					t.Fatal(err)
				}
				triage.EventID, triage.SupersededBy = ev.ID, latest.EventID
			case "new event":
				g.checks = "new check"
				if err := s.Collect(t.Context()); err != nil {
					t.Fatal(err)
				}
			case "compacted event":
				if _, err := store.db.ExecContext(t.Context(), "UPDATE ghraft_events SET payload=? WHERE event_id=?", `{"body_pruned":true}`, old.EventID); err != nil {
					t.Fatal(err)
				}
			case "other owner":
				owner.owner = "successor"
			case "human":
				owner.ownerType = "user"
			case "done", "closed", "in_review":
				owner.status = boundary
			case "revoked":
				s.Config.Forks = nil
			case "guide changed":
				s.Config.Repositories[0].GitHubActions = []string{"push"}
			case "different work":
				events, err := store.Events(t.Context(), false)
				if err != nil {
					t.Fatal(err)
				}
				for _, ev := range events {
					if ev.Key == "example/fixture#1" {
						triage.SupersededBy = ev.ID
					}
				}
			}
			if err := s.RecordTriage(t.Context(), triage); err == nil {
				t.Fatal("invalid coverage was accepted", boundary)
			}
			var count int
			if err := store.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM ghraft_triage WHERE event_id=?", triage.EventID).Scan(&count); err != nil || count != 0 {
				t.Fatal("rejected coverage still saved a receipt", count, err)
			}
		})
	}
}

type coverageInterruption struct {
	Persistence
	interrupt func()
}

func (s coverageInterruption) RecordTriage(ctx context.Context, in Triage) error {
	s.interrupt()
	return s.Persistence.RecordTriage(ctx, in)
}

func TestForkCoverageRechecksAConcurrentRequest(t *testing.T) {
	s, _, owner, old, latest := forkCompletionFixture(t)
	store := s.Store
	s.Store = coverageInterruption{Persistence: store, interrupt: func() {
		// Same snapshot version, but this request must remain actionable.
		if _, err := store.RequestWork(t.Context(), s.Config.Forks[0].Key(), latest.EvidenceVersion, fixtureReport, "New work during reconciliation"); err != nil {
			t.Fatal(err)
		}
	}}
	if err := s.RecordTriage(t.Context(), Triage{Executor: "@worker", EventID: old.EventID, Outcome: "superseded", Reason: "Covered", SupersededBy: latest.EventID, ReportID: fixtureReport}); !errors.Is(err, ErrStaleWork) {
		t.Fatal("concurrent request was ignored", err)
	}
	var calls []string
	if _, err := s.FinishWork(t.Context(), latest, updateFixtureTask(t, owner, &calls)); !errors.Is(err, ErrStaleWork) || len(calls) != 0 {
		t.Fatal("stale completion changed the task", err, calls)
	}
}

func TestForkCoverageSurvivesHistoryCompaction(t *testing.T) {
	s, _, owner, old, latest := forkCompletionFixture(t)
	in := Triage{Executor: "@worker", EventID: old.EventID, Outcome: "superseded", Reason: "Covered", SupersededBy: latest.EventID, ReportID: fixtureReport}
	if err := s.RecordTriage(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	store := s.Store.(*Store)
	stamp := time.Now().Add(-72 * time.Hour).Unix()
	for _, query := range []string{"UPDATE ghraft_events SET accepted_at=?", "UPDATE ghraft_results SET created_at=?", "UPDATE ghraft_triage SET created_at=?"} {
		if _, err := store.db.ExecContext(t.Context(), query, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Cleanup(t.Context(), 1, false); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := store.db.QueryRowContext(t.Context(), "SELECT body FROM ghraft_triage WHERE event_id=?", old.EventID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var kept Triage
	if err := json.Unmarshal([]byte(raw), &kept); err != nil || kept.SupersededBy != in.SupersededBy || kept.ReportID != in.ReportID || kept.Executor != in.Executor {
		t.Fatal("coverage references lost during cleanup", raw, err)
	}
	if err := s.RecordTriage(t.Context(), in); err != nil {
		t.Fatal("retry after compaction failed", err)
	}
	var calls []string
	if got, err := s.FinishWork(t.Context(), latest, updateFixtureTask(t, owner, &calls)); err != nil || got.Status != "in_progress" {
		t.Fatal("compaction lost completion evidence", got, err)
	}
}
