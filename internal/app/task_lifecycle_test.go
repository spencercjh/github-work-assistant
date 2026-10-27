package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type lifecycleSource struct {
	body      []byte
	err       error
	onCollect func()
}

func (*lifecycleSource) CheckIdentity(context.Context) error { return nil }
func (g *lifecycleSource) Collect(context.Context, Watch) ([]byte, error) {
	if g.onCollect != nil {
		g.onCollect()
	}
	return g.body, g.err
}

func lifecycleFixture(t *testing.T, objects Objects) (*Service, *taskAPI, *lifecycleSource, WorkInput) {
	t.Helper()
	f := &taskAPI{owner: "worker", status: "in_progress"}
	s, first := sharedFixture(t, f)
	deliverEvent(t, s, first)
	quietEvent(t, s, first.ID)
	g := &lifecycleSource{}
	s.GH = g
	if err := s.Store.RegisterWatch(t.Context(), s.Config.Watches[0]); err != nil {
		t.Fatal(err)
	}
	in := observeLifecycle(t, s, g, objects)
	ev, err := s.Store.Event(t.Context(), in.EventID)
	if err != nil {
		t.Fatal(err)
	}
	deliverEvent(t, s, ev)
	return s, f, g, in
}

func observeLifecycle(t *testing.T, s *Service, g *lifecycleSource, objects Objects) WorkInput {
	t.Helper()
	g.body = canonical(Snapshot{Key: "example/fixture#1", Classification: "public", Objects: objects})
	if err := s.collectWatch(t.Context(), s.Config.Watches[0]); err != nil {
		t.Fatal(err)
	}
	id, err := s.Store.Meta(t.Context(), "current-event:example/fixture#1", "")
	if err != nil {
		t.Fatal(err)
	}
	return lifecycleInput(t, s, id)
}

func lifecycleInput(t *testing.T, s *Service, id string) WorkInput {
	t.Helper()
	w, err := s.WorkContext(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return WorkInput{Executor: "@worker", EventID: id, EvidenceVersion: w.CurrentVersion, GuideVersion: w.Guide.Version, TaskID: fixtureTask}
}

func quietEvent(t *testing.T, s *Service, id string) {
	t.Helper()
	if err := s.RecordTriage(t.Context(), Triage{Executor: "@worker", EventID: id, Outcome: "quiet", Reason: "Verified this event requires no further action."}); err != nil {
		t.Fatal(err)
	}
}

func updateFixtureTask(t *testing.T, f *taskAPI, calls *[]string) func(context.Context, TaskDispatch, string) error {
	t.Helper()
	return func(_ context.Context, task TaskDispatch, status string) error {
		if task.MessageID != fixtureTask || task.Channel != "#github-work" || task.Number != 17 {
			t.Fatal("updated another task", task)
		}
		*calls = append(*calls, status)
		f.mu.Lock()
		f.status = status
		f.mu.Unlock()
		return nil
	}
}

func TestFinishWorkTracksGitHubStateAndIsIdempotent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		objects Objects
		want    string
	}{
		{"merged PR", Objects{"pull": {"state": "closed", "merged": true}}, "done"},
		{"unmerged closed PR", Objects{"pull": {"state": "closed", "merged": false}}, "done"},
		{"completed issue", Objects{"issue": {"state": "closed", "state_reason": "completed"}}, "done"},
		{"closed issue", Objects{"issue": {"state": "closed", "state_reason": "not_planned"}}, "done"},
		{"open PR", Objects{"pull": {"state": "open", "merged": false}}, "in_review"},
		{"inconsistent issue cannot close open PR", Objects{"issue": {"state": "closed"}, "pull": {"state": "open"}}, "in_review"},
		{"unknown state", Objects{"issue": {"title": "Unavailable state"}}, "in_review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, _, in := lifecycleFixture(t, tc.objects)
			quietEvent(t, s, in.EventID)
			var calls []string
			update := updateFixtureTask(t, f, &calls)
			for range 2 {
				got, err := s.FinishWork(t.Context(), in, update)
				if err != nil || got.Status != tc.want || f.status != tc.want {
					t.Fatal("wrong completion", got, err)
				}
			}
			if len(calls) != 1 || f.creates != 1 {
				t.Fatal("retry changed remote task twice", calls, f.creates)
			}
		})
	}
}

func TestFinishWorkRequiresDeliveryAndHandlingOfEveryEvent(t *testing.T) {
	for _, missing := range []string{"older unhandled", "latest unhandled", "undelivered", "waiting", "needs-decision"} {
		t.Run(missing, func(t *testing.T) {
			s, f, g, previous := lifecycleFixture(t, Objects{"issue": {"state": "open"}})
			if missing != "older unhandled" {
				quietEvent(t, s, previous.EventID)
			}
			in := observeLifecycle(t, s, g, Objects{"issue": {"state": "closed"}})
			ev, _ := s.Store.Event(t.Context(), in.EventID)
			if missing != "undelivered" {
				deliverEvent(t, s, ev)
			}
			if missing == "waiting" || missing == "needs-decision" {
				if missing == "needs-decision" {
					s.Config.DecisionMaker, s.Raft.Config.DecisionMaker = "@owner", "@owner"
					guide, err := s.Config.RepositoryGuide("example/fixture")
					if err != nil {
						t.Fatal(err)
					}
					in.GuideVersion = guide.Version
					f.reportID, f.reportAuthor, f.reportKind, f.reportContent = fixtureReport, "worker", "agent", "@owner Which follow-up should continue?"
				}
				if _, err := s.LinkTask(t.Context(), in); err != nil {
					t.Fatal(err)
				}
				if err := s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: missing, Summary: "Follow-up is unresolved."}); err != nil {
					t.Fatal(err)
				}
			} else if missing != "latest unhandled" {
				// Even a persisted report cannot substitute for confirmed delivery.
				if err := s.Store.RecordTriage(t.Context(), Triage{EventID: in.EventID, Outcome: "quiet", Reason: "Test handled state"}); err != nil {
					t.Fatal(err)
				}
			}
			var calls []string
			if _, err := s.FinishWork(t.Context(), in, updateFixtureTask(t, f, &calls)); err == nil || len(calls) != 0 || f.status != "in_progress" {
				t.Fatal("unfinished work completed", calls, err)
			}
		})
	}
}

func TestFinishWorkAcceptsFinalReportAndKeepsOpenWaitingWorkInReview(t *testing.T) {
	for _, closed := range []bool{false, true} {
		state, outcome, want := "open", "waiting", "in_review"
		if closed {
			state, outcome, want = "closed", "investigated", "done"
		}
		t.Run(state, func(t *testing.T) {
			s, f, _, in := lifecycleFixture(t, Objects{"issue": {"state": state}})
			if _, err := s.LinkTask(t.Context(), in); err != nil {
				t.Fatal(err)
			}
			if err := s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: outcome, Summary: "Current status verified."}); err != nil {
				t.Fatal(err)
			}
			var calls []string
			got, err := s.FinishWork(t.Context(), in, updateFixtureTask(t, f, &calls))
			if err != nil || got.Status != want {
				t.Fatal(got, err)
			}
		})
	}
}

func TestFinishWorkRespectsFreshEvidenceOwnershipAndManualStops(t *testing.T) {
	for _, boundary := range []string{"owner", "human", "closed", "todo", "root", "guide", "evidence", "access", "handoff", "reopen"} {
		t.Run(boundary, func(t *testing.T) {
			s, f, g, in := lifecycleFixture(t, Objects{"issue": {"state": "closed"}})
			quietEvent(t, s, in.EventID)
			switch boundary {
			case "owner":
				in.Executor = "@other"
			case "human":
				f.ownerType = "user"
			case "closed", "todo":
				f.status = boundary
			case "root":
				in.TaskID = fixtureReport
			case "guide":
				in.GuideVersion = strings.Repeat("0", 64)
			case "evidence":
				in.EvidenceVersion = strings.Repeat("0", 64)
			case "access":
				g.err = ErrGitHubAccessDenied
			case "handoff":
				g.onCollect = func() { f.mu.Lock(); f.owner = "successor"; f.mu.Unlock() }
			case "reopen":
				g.body = canonical(Snapshot{Key: "example/fixture#1", Classification: "public", Objects: Objects{"issue": {"state": "open"}}})
			}
			var calls []string
			if _, err := s.FinishWork(t.Context(), in, updateFixtureTask(t, f, &calls)); err == nil || len(calls) != 0 {
				t.Fatal("boundary bypassed", calls, err)
			}
		})
	}
}

func TestFinishWorkHandlesUnreadHoldAndLostStatusResponse(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "held", true: "response lost"}[committed], func(t *testing.T) {
			s, f, _, in := lifecycleFixture(t, Objects{"issue": {"state": "closed"}})
			quietEvent(t, s, in.EventID)
			var calls []string
			update := updateFixtureTask(t, f, &calls)
			got, err := s.FinishWork(t.Context(), in, func(ctx context.Context, task TaskDispatch, status string) error {
				if committed {
					_ = update(ctx, task, status)
				}
				return errors.New("unread hold or lost response")
			})
			if committed && (err != nil || got.Status != "done") {
				t.Fatal("committed result not recovered", got, err)
			}
			if !committed && (err == nil || f.status != "in_progress") {
				t.Fatal("held write treated as success", err)
			}
			if _, err = s.FinishWork(t.Context(), in, update); err != nil || len(calls) != 1 {
				t.Fatal("normal retry failed or duplicated update", calls, err)
			}
		})
	}
}

func TestCompletionRaceRestoresReopenedWorkAndRecoversAfterRestart(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(map[bool]string{false: "restore", true: "recover"}[interrupted], func(t *testing.T) {
			s, f, g, in := lifecycleFixture(t, Objects{"issue": {"state": "closed"}})
			quietEvent(t, s, in.EventID)
			var calls []string
			update := updateFixtureTask(t, f, &calls)
			_, err := s.FinishWork(t.Context(), in, func(ctx context.Context, task TaskDispatch, status string) error {
				if status == "in_progress" && interrupted {
					return errors.New("restore interrupted")
				}
				if status == "done" {
					reopened := observeLifecycle(t, s, g, Objects{"issue": {"state": "open"}})
					if interrupted {
						ev, _ := s.Store.Event(ctx, reopened.EventID)
						deliverEvent(t, s, ev)
						quietEvent(t, s, ev.ID)
					}
				}
				return update(ctx, task, status)
			})
			if !errors.Is(err, ErrStaleWork) {
				t.Fatal("concurrent reopening missed", err)
			}
			if !interrupted {
				if f.status != "in_progress" || len(calls) != 2 {
					t.Fatal("task not restored", calls)
				}
				return
			}
			s.Store, s.Raft.Store = openTestStore(t), openTestStore(t)
			for range 2 {
				if err := s.ReconcileTaskCompletion(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			pending, err := s.Store.Events(t.Context(), true)
			if err != nil || len(pending) != 1 || !explicitWork(pending[0]) {
				t.Fatal("expected one recovery event for already handled reopening", len(pending), err)
			}
			for _, ev := range pending {
				deliverEvent(t, s, ev)
				if !strings.Contains(f.updates["github-update:"+ev.ID], "恢复为 in_progress") {
					t.Fatal("owner not asked to resume")
				}
			}
			if f.creates != 1 || f.status != "done" {
				t.Fatal("collector recreated or changed task")
			}
		})
	}
}

func TestPostCompletionUpdatesStayDoneWithoutExecutionAuthority(t *testing.T) {
	s, f, g, in := lifecycleFixture(t, Objects{"issue": {"state": "closed", "title": "first"}})
	quietEvent(t, s, in.EventID)
	var calls []string
	if _, err := s.FinishWork(t.Context(), in, updateFixtureTask(t, f, &calls)); err != nil {
		t.Fatal(err)
	}
	second := observeLifecycle(t, s, g, Objects{"issue": {"state": "closed", "title": "second"}})
	third := observeLifecycle(t, s, g, Objects{"issue": {"state": "closed", "title": "third"}})
	for _, next := range []WorkInput{second, third} {
		ev, _ := s.Store.Event(t.Context(), next.EventID)
		deliverEvent(t, s, ev)
		if !strings.Contains(f.updates["github-update:"+ev.ID], "原任务保持 Done") {
			t.Fatal("terminal metadata reopened task")
		}
		if err := s.VerifyClaim(t.Context(), ev.ID, "@worker"); !errors.Is(err, ErrClaimUnavailable) {
			t.Fatal("done granted execution", err)
		}
		if _, err := s.LinkTask(t.Context(), next); !errors.Is(err, ErrClaimUnavailable) {
			t.Fatal("done allowed new engineering work", err)
		}
		if err := s.RecordTriage(t.Context(), Triage{Executor: "@other", EventID: ev.ID, Outcome: "quiet", Reason: "not the owner"}); !errors.Is(err, ErrClaimUnavailable) {
			t.Fatal("outsider triaged", err)
		}
		quietEvent(t, s, ev.ID)
	}
	if f.status != "done" || len(calls) != 1 {
		t.Fatal("quiet triage changed task status")
	}
	requested, err := s.Store.RequestWork(t.Context(), "example/fixture#1", third.EvidenceVersion, fixtureReport, "Investigate a follow-up")
	if err != nil {
		t.Fatal(err)
	}
	deliverEvent(t, s, requested)
	if err := s.RecordTriage(t.Context(), Triage{Executor: "@worker", EventID: requested.ID, Outcome: "quiet", Reason: "skip requested work"}); !errors.Is(err, ErrClaimUnavailable) {
		t.Fatal("explicit request silently discarded", err)
	}
	if !strings.Contains(f.updates["github-update:"+requested.ID], "恢复为 in_progress") {
		t.Fatal("explicit work did not resume")
	}
}

func TestCompletionReconcilesExistingWorkOnceWithoutCreatingAnotherTask(t *testing.T) {
	s, f, _, in := lifecycleFixture(t, Objects{"pull": {"state": "closed", "merged": true}})
	quietEvent(t, s, in.EventID)
	f.status = "in_review"
	before := len(f.updates)
	for range 2 {
		if err := s.Deliver(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if f.creates != 1 || len(f.updates) != before+1 || f.status != "in_review" {
		t.Fatal("completion request duplicated or mutated task")
	}
	id, _ := s.Store.Meta(t.Context(), "current-event:example/fixture#1", "")
	if id == in.EventID {
		t.Fatal("no completion request")
	}
	in = lifecycleInput(t, s, id)
	f.status = "in_progress"
	quietEvent(t, s, id)
	var calls []string
	if _, err := s.FinishWork(t.Context(), in, updateFixtureTask(t, f, &calls)); err != nil {
		t.Fatal(err)
	}
	s.Store, s.Raft.Store = openTestStore(t), openTestStore(t)
	if err := s.Deliver(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.updates) != before+1 || f.status != "done" {
		t.Fatal("restart woke completed work again")
	}
}

func TestCompletionReconciliationRespectsManualStopsAndStaleEvidence(t *testing.T) {
	for _, blocked := range []string{"closed", "human", "waiting", "stale", "revoked"} {
		t.Run(blocked, func(t *testing.T) {
			s, f, _, in := lifecycleFixture(t, Objects{"issue": {"state": "closed"}})
			if blocked != "waiting" {
				quietEvent(t, s, in.EventID)
			}
			f.status = "in_review"
			switch blocked {
			case "closed":
				f.status = "closed"
			case "human":
				f.ownerType = "user"
			case "revoked":
				s.Config.Watches = nil
			case "stale":
				store := s.Store.(*Store)
				if _, err := store.db.ExecContext(t.Context(), `UPDATE ghraft_snapshots SET observed_at=?`, time.Now().Add(-time.Hour).Unix()); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.ReconcileTaskCompletion(t.Context()); err != nil {
				t.Fatal(err)
			}
			pending, _ := s.Store.Events(t.Context(), true)
			if len(pending) != 0 {
				t.Fatal("completion requested outside active scope", pending)
			}
		})
	}
}

func TestCompletionRecoveryDistinguishesRepeatedCloseTransitions(t *testing.T) {
	closed := Objects{"issue": {"state": "closed"}}
	s, f, g, in := lifecycleFixture(t, closed)
	quietEvent(t, s, in.EventID)
	var requests []string
	var calls []string
	for round := range 2 {
		f.status = "in_review" // Worker stopped after its report, before finishing.
		if err := s.Deliver(t.Context()); err != nil {
			t.Fatal(err)
		}
		id, _ := s.Store.Meta(t.Context(), "current-event:example/fixture#1", "")
		requests = append(requests, id)
		if !strings.Contains(f.updates["github-update:"+id], "任务收尾") {
			t.Fatal("missing labeled completion request")
		}
		f.status = "in_progress"
		quietEvent(t, s, id)
		f.status = "in_review"
		if err := s.ReconcileTaskCompletion(t.Context()); err != nil {
			t.Fatal(err)
		}
		pending, _ := s.Store.Events(t.Context(), true)
		if len(pending) != 0 {
			t.Fatal("handled completion request generated another request")
		}
		if _, err := s.FinishWork(t.Context(), lifecycleInput(t, s, id), updateFixtureTask(t, f, &calls)); err != nil {
			t.Fatal(err)
		}
		if round == 0 {
			for _, objects := range []Objects{{"issue": {"state": "open"}}, closed} {
				next := observeLifecycle(t, s, g, objects)
				ev, _ := s.Store.Event(t.Context(), next.EventID)
				deliverEvent(t, s, ev)
				f.status = "in_progress"
				quietEvent(t, s, ev.ID)
			}
		}
	}
	if requests[0] == requests[1] || f.creates != 1 {
		t.Fatal("new close transition reused an obsolete request or created a task")
	}
}

func TestFinishForkRoundKeepsMaintenanceTaskOpen(t *testing.T) {
	s, _, f := forkFixture(t)
	in, _ := currentForkInput(t, s)
	quietEvent(t, s, in.EventID)
	var calls []string
	got, err := s.FinishWork(t.Context(), in, updateFixtureTask(t, f, &calls))
	if err != nil || got.Status != "in_review" {
		t.Fatal("maintenance task completed", got, err)
	}
	if githubEnded(canonical(Snapshot{Objects: Objects{"fork": {"phase": "synced"}, "pull": {"state": "closed", "merged": true}}})) {
		t.Fatal("merged sync PR ended long-lived maintenance")
	}
}
