package app

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestForkChangesReadyWaitsWithoutRetries(t *testing.T) {
	s, g, owner, _, in := forkCompletionFixture(t)
	f := s.Config.Forks[0]
	p, err := s.Store.ForkProgress(t.Context(), f.Key())
	if err != nil || p.Phase != "awaiting-review" || p.NextAttempt != 0 {
		t.Fatal("review-ready work still scheduled", p, err)
	}
	// Exhausting an earlier execution retry budget must not make human review
	// a new decision, even across a restart or a long wait.
	p.Wake, p.Retries = 5, 5
	if err = s.Store.SaveForkProgress(t.Context(), f.Key(), p.Revision, p, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Store.ForkProgress(t.Context(), f.Key())
	_, version, _ := s.Store.Snapshot(t.Context(), f.Key())
	events, _ := s.Store.Events(t.Context(), false)
	history, _ := s.Store.History(t.Context(), f.Key())
	owner.status = "in_review"
	s.Store, s.Raft.Store = openTestStore(t), openTestStore(t)
	for range 2 {
		if err = s.Collect(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	o, err := g.CollectFork(t.Context(), f, p)
	if err != nil || !reflect.DeepEqual(planFork(f, p, o, time.Now().Add(365*24*time.Hour).Unix()), p) {
		t.Fatal("review wait consumed retries", err)
	}
	after, _ := s.Store.ForkProgress(t.Context(), f.Key())
	w, err := s.WorkContext(t.Context(), in.EventID)
	if err != nil || !reflect.DeepEqual(after, p) || w.CurrentVersion != version || w.Guide.Version != in.GuideVersion || owner.status != "in_review" {
		t.Fatal("review wait changed progress, proof, guidance or task", after, err)
	}
	newEvents, _ := s.Store.Events(t.Context(), false)
	newHistory, _ := s.Store.History(t.Context(), f.Key())
	if !reflect.DeepEqual(events, newEvents) || !reflect.DeepEqual(history, newHistory) {
		t.Fatal("unchanged review wait created events or rewrote reports")
	}
}

func TestForkReviewWaitStillObservesChanges(t *testing.T) {
	for _, change := range []string{"check", "comment", "head", "target", "closed", "merged"} {
		t.Run(change, func(t *testing.T) {
			s, g, _, _, in := forkCompletionFixture(t)
			f := s.Config.Forks[0]
			p, _ := s.Store.ForkProgress(t.Context(), f.Key())
			o, err := g.CollectFork(t.Context(), f, p)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "check":
				o.Objects["check:1"]["status"] = "failure"
			case "comment":
				o.Objects["comment:1"] = map[string]any{"body": "Please update this behavior"}
			case "head":
				o.SyncHead = strings.Repeat("a", 40)
				o.Objects["pull"]["head"] = o.SyncHead
			case "target":
				o.Target = strings.Repeat("b", 40)
			case "closed", "merged":
				o.Objects["pull"]["state"] = "closed"
				o.Objects["pull"]["merged"] = change == "merged"
			}
			if err = s.Store.ObserveFork(t.Context(), f, p, o); err != nil {
				t.Fatal(err)
			}
			w, err := s.WorkContext(t.Context(), in.EventID)
			if err != nil || !w.Stale || w.CurrentEventID == in.EventID || w.CurrentEventID == "" {
				t.Fatal("real remote update did not create work", err)
			}
			if w.Fork.LastSuccess != p.LastSuccess || w.Fork.ValidatedHead != p.ValidatedHead || w.Fork.Wake != p.Wake || w.Fork.Retries != p.Retries {
				t.Fatal("remote observation claimed completion or consumed a retry", w.Fork)
			}
			if change == "closed" && w.Fork.Phase != "needs-decision" {
				t.Fatal("closed PR no longer requires a decision")
			}
		})
	}
}

func TestForkReviewCanResumeWaitingForExecution(t *testing.T) {
	s, _, _, _, in := forkCompletionFixture(t)
	f := s.Config.Forks[0]
	if err := s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", Outcome: "waiting", Summary: "Waiting for a new CI run"}); err != nil {
		t.Fatal(err)
	}
	p, _ := s.Store.ForkProgress(t.Context(), f.Key())
	if p.Phase != "waiting" || p.NextAttempt <= time.Now().Unix() {
		t.Fatal("explicit execution wait lost its retry", p)
	}
	p.NextAttempt, p.Retries = time.Now().Add(-time.Minute).Unix(), 4
	if err := s.Store.SaveForkProgress(t.Context(), f.Key(), p.Revision, p, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Store.ForkProgress(t.Context(), f.Key())
	if after.Phase != "needs-decision" || after.NextAttempt != 0 || after.Retries != 5 || after.Wake != p.Wake+1 {
		t.Fatal("unfinished execution no longer exhausts its bounded retry budget", after)
	}
}

func legacyReviewWait(t *testing.T) (*Service, *forkFixtureSource, WorkInput, ForkProgress) {
	t.Helper()
	s, g, _, _, in := forkCompletionFixture(t)
	f := s.Config.Forks[0]
	store := s.Store.(*Store)
	p, _ := store.ForkProgress(t.Context(), f.Key())
	p.Wake, p.Retries = 5, 5
	if err := store.SaveForkProgress(t.Context(), f.Key(), p.Revision, p, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, _ = currentForkInput(t, s)
	if err := s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "changes-ready", Summary: "Current changes remain ready for human review."}); err != nil {
		t.Fatal(err)
	}
	created := time.Now().Add(-2 * time.Hour).Unix()
	if _, err := store.db.ExecContext(t.Context(), "UPDATE ghraft_results SET created_at=?", created-1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(t.Context(), "UPDATE ghraft_results SET created_at=? WHERE event_id=? AND report_id=?", created, in.EventID, fixtureReport); err != nil {
		t.Fatal(err)
	}
	p, _ = store.ForkProgress(t.Context(), f.Key())
	p.Phase, p.NextAttempt = "waiting", created+int64(time.Hour.Seconds())
	if err := store.SaveForkProgress(t.Context(), f.Key(), p.Revision, p, nil); err != nil {
		t.Fatal(err)
	}
	p, _ = store.ForkProgress(t.Context(), f.Key())
	return s, g, in, p
}

func TestLegacyForkReviewWaitUpgradesQuietly(t *testing.T) {
	s, g, in, before := legacyReviewWait(t)
	f := s.Config.Forks[0]
	o, _ := g.CollectFork(t.Context(), f, before)
	store := s.Store.(*Store)
	body, version, err := store.Snapshot(t.Context(), f.Key())
	if err != nil {
		t.Fatal(err)
	}
	events, _ := store.Events(t.Context(), false)
	history, _ := store.History(t.Context(), f.Key())
	if err := store.ObserveFork(t.Context(), f, before, o); err != nil {
		t.Fatal(err)
	}
	after, _ := store.ForkProgress(t.Context(), f.Key())
	want := before
	want.Phase, want.NextAttempt, want.Revision = "awaiting-review", 0, before.Revision+1
	if !reflect.DeepEqual(after, want) {
		t.Fatal("upgrade changed more than the review schedule", after, want)
	}
	s.Store = openTestStore(t)
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	w, err := s.Store.ReadWork(t.Context(), in.EventID)
	if err != nil || w.CurrentEventID != in.EventID || w.CurrentVersion != version || w.Task.EvidenceVersion != version || string(w.Snapshot) != string(body) {
		t.Fatal("upgrade invalidated existing evidence or task association", w, err)
	}
	newEvents, _ := s.Store.Events(t.Context(), false)
	newHistory, _ := s.Store.History(t.Context(), f.Key())
	if !reflect.DeepEqual(events, newEvents) || !reflect.DeepEqual(history, newHistory) {
		t.Fatal("upgrade created work or rewrote reports")
	}
}

func TestLegacyForkReviewWaitNeedsCurrentUnambiguousReport(t *testing.T) {
	for _, boundary := range []string{"waiting", "tie", "pruned", "task", "guide", "event", "missing link", "snapshot", "pending", "round", "target", "head", "closed", "deadline"} {
		t.Run(boundary, func(t *testing.T) {
			s, g, in, p := legacyReviewWait(t)
			store := s.Store.(*Store)
			f := s.Config.Forks[0]
			sql, args := "", []any{}
			switch boundary {
			case "waiting", "pruned":
				report := WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "waiting"}
				if boundary == "pruned" {
					report = WorkResult{}
				}
				sql, args = "UPDATE ghraft_results SET body=? WHERE event_id=?", []any{string(canonical(report)), in.EventID}
			case "tie":
				sql, args = "UPDATE ghraft_results SET created_at=?", []any{p.NextAttempt - int64(time.Hour.Seconds())}
			case "task", "guide", "event":
				column := map[string]string{"task": "task_id", "guide": "guide_version", "event": "event_id"}[boundary]
				sql, args = "UPDATE ghraft_work_links SET "+column+"=? WHERE resource_key=?", []any{"changed", f.Key()}
			case "missing link":
				sql, args = "DELETE FROM ghraft_work_links WHERE resource_key=?", []any{f.Key()}
			case "snapshot":
				sql, args = "UPDATE ghraft_snapshots SET version_hash=? WHERE resource_key=?", []any{strings.Repeat("f", 64), f.Key()}
			case "pending":
				sql, args = "UPDATE ghraft_events SET state='pending' WHERE event_id=?", []any{in.EventID}
			case "round":
				p.Round = strings.Repeat("a", 64)
			case "target":
				p.ValidatedBase = strings.Repeat("b", 40)
			case "head":
				p.ValidatedHead = strings.Repeat("c", 40)
			case "closed":
				var body string
				if err := store.db.QueryRowContext(t.Context(), "SELECT body FROM ghraft_snapshots WHERE resource_key=?", f.Key()).Scan(&body); err != nil {
					t.Fatal(err)
				}
				var snapshot Snapshot
				_ = json.Unmarshal([]byte(body), &snapshot)
				snapshot.Objects["pull"]["state"] = "closed"
				sql, args = "UPDATE ghraft_snapshots SET body=? WHERE resource_key=?", []any{string(canonical(snapshot)), f.Key()}
			case "deadline":
				p.NextAttempt -= int64(45 * time.Minute.Seconds())
			}
			if sql != "" {
				if _, err := store.db.ExecContext(t.Context(), sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.SaveForkProgress(t.Context(), f.Key(), p.Revision, p, nil); err != nil {
				// Changing the round must also fail the storage compare-and-swap.
				if boundary != "round" {
					t.Fatal(err)
				}
				p, _ = store.ForkProgress(t.Context(), f.Key())
				sql = "UPDATE ghraft_meta SET value=? WHERE name=?"
				p.Round = strings.Repeat("a", 64)
				if _, err = store.db.ExecContext(t.Context(), sql, string(canonical(p)), forkProgressName(f.Key())); err != nil {
					t.Fatal(err)
				}
			}
			p, _ = store.ForkProgress(t.Context(), f.Key())
			o, _ := g.CollectFork(t.Context(), f, p)
			if err := store.ObserveFork(t.Context(), f, p, o); err != nil {
				t.Fatal(err)
			}
			after, _ := store.ForkProgress(t.Context(), f.Key())
			if after.Phase == "awaiting-review" || after.Wake != p.Wake+1 {
				t.Fatal("ambiguous or unrelated history suppressed an execution retry", boundary, after)
			}
		})
	}
}
