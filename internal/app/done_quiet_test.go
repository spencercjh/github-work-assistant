package app

import (
	"errors"
	"strings"
	"testing"
)

func TestManualDoneAcceptsQuietOpenUpdatesWithoutResuming(t *testing.T) {
	for _, kind := range []string{"issue", "pull"} {
		t.Run(kind, func(t *testing.T) {
			objects := Objects{kind: {"state": "open", "title": "Still open"}}
			s, f, g, first := lifecycleFixture(t, objects)
			quietEvent(t, s, first.EventID)
			f.status = "done" // The human finished tracking without closing GitHub.
			objects["comment:1"] = map[string]any{"body": "part of #826"}
			second := observeLifecycle(t, s, g, objects)
			objects["comment:2"] = map[string]any{"body": "Background context only"}
			third := observeLifecycle(t, s, g, objects)
			for _, in := range []WorkInput{second, third} {
				ev, err := s.Store.Event(t.Context(), in.EventID)
				if err != nil {
					t.Fatal(err)
				}
				deliverEvent(t, s, ev)
				if err := s.VerifyClaim(t.Context(), ev.ID, "@worker"); !errors.Is(err, ErrClaimUnavailable) {
					t.Fatal("Done granted execution authority", err)
				}
				if _, err := s.LinkTask(t.Context(), in); !errors.Is(err, ErrClaimUnavailable) {
					t.Fatal("Done allowed new work", err)
				}
				quietEvent(t, s, ev.ID)
				message := f.updates["github-update:"+ev.ID]
				if !strings.Contains(message, "原任务保持 Done") || !strings.Contains(message, "无需调用 finish-work") {
					t.Fatal("ordinary update asked to reopen or finish the manual Done task", message)
				}
			}
			if rows, err := s.Store.UnprocessedPage(t.Context(), 0); err != nil || len(rows) != 0 {
				t.Fatal("quiet events remained unprocessed", rows, err)
			}
			// Reconciliation and process restarts must not turn these judgments
			// into a work request or overwrite the human's completed state.
			s.Store, s.Raft.Store = openTestStore(t), openTestStore(t)
			before := f.updateWrites
			for range 2 {
				if err := s.Deliver(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if f.status != "done" || f.owner != "worker" || f.creates != 1 || f.updateWrites != before {
				t.Fatal("manual completion was changed or notified again")
			}
			var writes []string
			if _, err := s.FinishWork(t.Context(), third, updateFixtureTask(t, f, &writes)); !errors.Is(err, ErrClaimUnavailable) || len(writes) != 0 {
				t.Fatal("finish-work must not reopen a manually completed open item", err, writes)
			}
		})
	}
}

func TestDoneQuietStillRejectsReopeningAndExplicitWork(t *testing.T) {
	for _, kind := range []string{"issue", "pull", "request"} {
		t.Run(kind, func(t *testing.T) {
			objects := Objects{"issue": {"state": "closed"}}
			if kind == "pull" {
				objects = Objects{"pull": {"state": "closed", "merged": false}}
			}
			s, f, g, in := lifecycleFixture(t, objects)
			quietEvent(t, s, in.EventID)
			f.status = "done"
			var ev Event
			var err error
			if kind == "request" {
				ev, err = s.Store.RequestWork(t.Context(), "example/fixture#1", in.EvidenceVersion, fixtureReport, "Investigate a follow-up")
			} else {
				objects[kind]["state"] = "open"
				next := observeLifecycle(t, s, g, objects)
				ev, err = s.Store.Event(t.Context(), next.EventID)
			}
			if err != nil {
				t.Fatal(err)
			}
			deliverEvent(t, s, ev)
			if err := s.RecordTriage(t.Context(), Triage{Executor: "@worker", EventID: ev.ID, Outcome: "quiet", Reason: "Skip new work"}); !errors.Is(err, ErrClaimUnavailable) {
				t.Fatal("reopening or explicit work was discarded while Done", err)
			}
			if !strings.Contains(f.updates["github-update:"+ev.ID], "恢复为 in_progress") {
				t.Fatal("reopening/request did not ask the current owner to resume")
			}
		})
	}
}

func TestManualDoneQuietPreservesAccessDeliveryAndOwnershipChecks(t *testing.T) {
	for _, boundary := range []string{"other owner", "human", "closed", "in_review", "todo", "undelivered", "revoked", "task unavailable", "fork", "notified"} {
		t.Run(boundary, func(t *testing.T) {
			objects := Objects{"issue": {"state": "open"}}
			s, f, g, in := lifecycleFixture(t, objects)
			quietEvent(t, s, in.EventID)
			f.status = "done"
			objects["comment:1"] = map[string]any{"body": "Background only"}
			if boundary == "fork" {
				objects["fork"] = map[string]any{"target_repo": "example/fixture", "round": "fixture-round"}
			}
			in = observeLifecycle(t, s, g, objects)
			ev, _ := s.Store.Event(t.Context(), in.EventID)
			if boundary != "undelivered" {
				deliverEvent(t, s, ev)
			}
			triage := Triage{Executor: "@worker", EventID: ev.ID, Outcome: "quiet", Reason: "No action"}
			switch boundary {
			case "other owner":
				f.owner = "successor"
			case "human":
				f.ownerType = "user"
			case "closed", "in_review", "todo":
				f.status = boundary
			case "revoked":
				s.Config.Watches = nil
			case "task unavailable":
				f.forbidden = true
			case "notified":
				triage.Outcome = "notified"
			}
			if err := s.RecordTriage(t.Context(), triage); err == nil {
				t.Fatal("quiet completion boundary bypassed", boundary)
			}
			if rows, err := s.Store.UnprocessedPage(t.Context(), 0); err != nil || len(rows) != 1 {
				t.Fatal("rejected event was still marked handled", rows, err)
			}
		})
	}
}
