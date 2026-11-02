package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func abandonedValidation(t *testing.T, s *Service, state string) ForkProgress {
	t.Helper()
	p, err := s.Store.ForkProgress(t.Context(), s.Config.Forks[0].Key())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	p.Validation = &ForkValidation{ID: strings.Repeat("a", 64), Round: p.Round, Head: p.Upstream, Base: p.InitialTarget, Guide: "guide", Log: "/private/test.log", State: state, StartedAt: now - 600, HeartbeatAt: now - 180, Deadline: now + 300}
	if state != "running" {
		p.Validation.FinishedAt = now - 360
	}
	p.NextAttempt = now + 4*3600
	if err = s.Store.SaveForkProgress(t.Context(), s.Config.Forks[0].Key(), p.Revision, p, nil); err != nil {
		t.Fatal(err)
	}
	p, err = s.Store.ForkProgress(t.Context(), s.Config.Forks[0].Key())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestForkValidationRecoverySurvivesRestartAndReusesOwner(t *testing.T) {
	for _, state := range []string{"running", "passed", "failed", "interrupted"} {
		t.Run(state, func(t *testing.T) {
			s, _, owner := forkFixture(t)
			in, _ := currentForkInput(t, s)
			before := abandonedValidation(t, s, state)
			// A separate connection has no in-memory process or timer state.
			s.Store = openTestStore(t)
			s.Raft.Store = s.Store
			if err := s.Collect(t.Context()); err != nil {
				t.Fatal(err)
			}
			w, err := s.WorkContext(t.Context(), in.EventID)
			if err != nil || !w.Stale || w.CurrentEventID == in.EventID || w.Fork.Wake != before.Wake+1 || w.Fork.Retries != before.Retries || w.Fork.Phase != "working" || w.Fork.Validation.NotifiedAt == 0 {
				t.Fatal("lost validation did not produce a recovery event", w.Fork, err)
			}
			if w.Fork.ValidatedHead != "" || w.Fork.LastSuccess != before.LastSuccess {
				t.Fatal("recovery fabricated proof or completion")
			}
			if state == "running" && w.Fork.Validation.State != "interrupted" {
				t.Fatal("abandoned process still shown as running")
			}
			ev, err := s.Store.Event(t.Context(), w.CurrentEventID)
			if err != nil || ev.State != "pending" {
				t.Fatal("recovery was not durably queued", err)
			}
			// Repeated collection and another restart do not flood the thread.
			s.Store = openTestStore(t)
			s.Raft.Store = s.Store
			if err = s.Collect(t.Context()); err != nil {
				t.Fatal(err)
			}
			after, err := s.WorkContext(t.Context(), in.EventID)
			if err != nil || after.CurrentEventID != ev.ID || !reflect.DeepEqual(after.Fork, w.Fork) {
				t.Fatal("duplicate recovery", err)
			}
			deliverEvent(t, s, ev)
			if owner.creates != 1 || !strings.Contains(owner.updates["github-update:"+ev.ID], "@worker") || !strings.Contains(owner.updates["github-update:"+ev.ID], "fork.validation") {
				t.Fatal("recovery lost its existing owner or instructions")
			}
			if _, err = s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: in, Action: "push", Head: before.Upstream, Base: before.InitialTarget, Ref: before.Branch}); err == nil {
				t.Fatal("recovery authorized stale/unproven write")
			}
		})
	}
}

func TestForkValidationRecoveryRespectsLiveRunsAndReportedWaits(t *testing.T) {
	for _, scenario := range []string{"live", "completed recently", "handled", "awaiting-review", "needs-decision", "synced", "other round"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, _ := forkFixture(t)
			in, _ := currentForkInput(t, s)
			p := abandonedValidation(t, s, "running")
			switch scenario {
			case "live":
				p.Validation.HeartbeatAt = time.Now().Unix()
			case "completed recently":
				p.Validation.State, p.Validation.FinishedAt = "passed", time.Now().Unix()
			case "handled":
				p.Validation.HandledAt = time.Now().Unix()
			case "other round":
				p.Validation.Round = "old-round"
			default:
				p.Phase, p.NextAttempt = scenario, 0
			}
			if scenario == "synced" {
				p.SyncMode = adaptedBatches
			}
			if err := s.Store.SaveForkProgress(t.Context(), s.Config.Forks[0].Key(), p.Revision, p, nil); err != nil {
				t.Fatal(err)
			}
			before, _ := s.Store.ForkProgress(t.Context(), s.Config.Forks[0].Key())
			if err := s.Collect(t.Context()); err != nil {
				t.Fatal(err)
			}
			w, err := s.WorkContext(t.Context(), in.EventID)
			if err != nil || w.Fork.Wake != before.Wake || w.Fork.Retries != before.Retries || w.Fork.Phase != before.Phase || w.Fork.Validation.NotifiedAt != 0 {
				t.Fatal("spurious validation recovery", w.Fork, err)
			}
		})
	}
}

func TestForkValidationHeartbeatIsFencedAndDoesNotChangeEvidence(t *testing.T) {
	s, _, _ := forkFixture(t)
	in, w := currentForkInput(t, s)
	p := abandonedValidation(t, s, "running")
	key := s.Config.Forks[0].Key()
	if err := s.Store.UpdateForkValidation(t.Context(), key, p.Round, p.Validation.ID, "running"); !errors.Is(err, ErrStaleWork) {
		t.Fatal("expired lease revived", err)
	}
	p.Validation.HeartbeatAt = time.Now().Add(-time.Minute).Unix()
	if err := s.Store.SaveForkProgress(t.Context(), key, p.Revision, p, nil); err != nil {
		t.Fatal(err)
	}
	stale, err := s.Store.ForkProgress(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.UpdateForkValidation(t.Context(), key, p.Round, p.Validation.ID, "running"); err != nil {
		t.Fatal(err)
	}
	after, err := s.WorkContext(t.Context(), in.EventID)
	if err != nil || after.CurrentVersion != w.CurrentVersion || after.CurrentEventID != w.CurrentEventID {
		t.Fatal("heartbeat changed evidence", err)
	}
	if after.Fork.Revision != stale.Revision || after.Fork.Validation.HeartbeatAt <= stale.Validation.HeartbeatAt {
		t.Fatal("heartbeat blocked semantic progress or failed to renew")
	}
	if err := s.Store.SaveForkProgress(t.Context(), key, stale.Revision, stale, nil); err != nil {
		t.Fatal("heartbeat raced a semantic save", err)
	}
	current, err := s.Store.ForkProgress(t.Context(), key)
	if err != nil || current.Validation.HeartbeatAt != after.Fork.Validation.HeartbeatAt {
		t.Fatal("stale progress overwrote the latest heartbeat", err)
	}
	for _, operation := range []struct{ round, id, state string }{{"other", p.Validation.ID, "failed"}, {p.Round, "old-attempt", "interrupted"}, {p.Round, p.Validation.ID, "passed"}} {
		if err := s.Store.UpdateForkValidation(t.Context(), key, operation.round, operation.id, operation.state); err == nil {
			t.Fatal("invalid/late update accepted", operation)
		}
	}
	if err := s.Store.UpdateForkValidation(t.Context(), key, p.Round, p.Validation.ID, "failed"); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.UpdateForkValidation(t.Context(), key, p.Round, p.Validation.ID, "running"); !errors.Is(err, ErrStaleWork) {
		t.Fatal("terminal attempt revived", err)
	}
}

func TestForkValidationStreamsLogsAndRecordsCancellation(t *testing.T) {
	s, g, _ := forkFixture(t)
	marker := filepath.Join(t.TempDir(), "started")
	orphan := filepath.Join(t.TempDir(), "orphan-finished")
	child := "import pathlib,time; time.sleep(2); pathlib.Path(" + string(canonical(orphan)) + ").touch()"
	s.Config.Repositories[0].TestCommands = [][]string{{"python3", "-u", "-c", "import pathlib,time,subprocess,sys; subprocess.Popen([sys.executable, '-c', " + string(canonical(child)) + "]); print('running regression', flush=True); pathlib.Path(" + string(canonical(marker)) + ").touch(); time.sleep(60)"}}
	in, w := currentForkInput(t, s)
	if _, err := s.PrepareWorkspace(t.Context(), in, g.repository); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type outcome struct {
		result WorkspaceTest
		err    error
	}
	done := make(chan outcome, 1)
	go func() { result, err := s.TestWorkspace(ctx, in); done <- outcome{result, err} }()
	deadline := time.Now().Add(10 * time.Second)
	var running ForkProgress
	for {
		if time.Now().After(deadline) {
			t.Fatal("test command did not start")
		}
		running, _ = s.Store.ForkProgress(t.Context(), w.Event.Key)
		if _, err := os.Stat(marker); err == nil && running.Validation != nil {
			b, err := os.ReadFile(running.Validation.Log)
			if err == nil && strings.Contains(string(b), "running regression") {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if running.Validation.State != "running" || running.ValidatedHead != "" {
		t.Fatal("missing durable in-flight state")
	}
	if _, err := s.TestWorkspace(t.Context(), in); err == nil {
		t.Fatal("duplicate validation acquired workspace lock")
	}
	cancel()
	select {
	case got := <-done:
		if got.err == nil || got.result.Passed {
			t.Fatal("cancelled command passed", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled command did not exit")
	}
	p, err := s.Store.ForkProgress(t.Context(), w.Event.Key)
	if err != nil || p.Validation.State != "interrupted" || p.Validation.FinishedAt == 0 || p.ValidatedHead != "" || p.LastSuccess != w.Fork.LastSuccess {
		t.Fatal("interruption not recorded", p, err)
	}
	if _, err := s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: in, Action: "push", Head: g.upstream, Base: g.target, Ref: p.Branch}); err == nil {
		t.Fatal("cancelled validation authorized push")
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled wrapper left a child running", err)
	}
}

func TestForkValidationProofAndFailureHaveDistinctDurableResults(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "pass", true: "fail"}[fail], func(t *testing.T) {
			s, g, _ := forkFixture(t)
			if fail {
				s.Config.Repositories[0].TestCommands = [][]string{{"python3", "-c", "raise SystemExit(1)"}}
			}
			in, w := currentForkInput(t, s)
			if _, err := s.PrepareWorkspace(t.Context(), in, g.repository); err != nil {
				t.Fatal(err)
			}
			result, err := s.TestWorkspace(t.Context(), in)
			p, readErr := s.Store.ForkProgress(t.Context(), w.Event.Key)
			if readErr != nil || (err != nil) != fail || result.Passed == fail || p.Validation == nil || p.Validation.FinishedAt == 0 {
				t.Fatal("invalid terminal result", p, result, err, readErr)
			}
			if fail {
				if p.Validation.State != "failed" || p.ValidatedHead != "" {
					t.Fatal("failed command saved proof")
				}
			} else {
				b, err := os.ReadFile(result.Log)
				if err != nil || p.Validation.State != "passed" || p.ValidatedHead != result.Head || p.TestLogHash != hash(b) {
					t.Fatal("proof and durable result disagree", err)
				}
			}
			if err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "waiting", Summary: "Validation result examined; continuing existing work."}); err != nil {
				t.Fatal(err)
			}
			p, _ = s.Store.ForkProgress(t.Context(), w.Event.Key)
			if p.Validation.HandledAt == 0 {
				t.Fatal("reported run still awaiting recovery")
			}
			// Rechecking a completed batch must preserve when this run was
			// first handled, regardless of a wall-clock second boundary.
			p.Validation.HandledAt = time.Now().Add(-time.Hour).Unix()
			firstHandled := p.Validation.HandledAt
			if err = s.Store.SaveForkProgress(t.Context(), w.Event.Key, p.Revision, p, nil); err != nil {
				t.Fatal(err)
			}
			if err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", Outcome: "waiting", Summary: "Rechecked the same validation result."}); err != nil {
				t.Fatal(err)
			}
			p, err = s.Store.ForkProgress(t.Context(), w.Event.Key)
			if err != nil || p.Validation.HandledAt != firstHandled {
				t.Fatal("later report changed the first handled timestamp", p.Validation, err)
			}
		})
	}
}

func TestValidationLogPreservesOutputLimit(t *testing.T) {
	var b strings.Builder
	w := &validationLog{dst: &b}
	chunk := []byte(strings.Repeat("x", 1<<20))
	for range 16 {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Write([]byte("overflow")); err == nil || w.Error() == nil || b.Len() != 16<<20 {
		t.Fatal("log limit bypassed")
	}
}

func TestForkValidationExpiredOrReplacedAttemptCannotSaveProof(t *testing.T) {
	for _, change := range []string{"expired heartbeat", "expired deadline", "new attempt"} {
		t.Run(change, func(t *testing.T) {
			s, g, _ := forkFixture(t)
			in, w := currentForkInput(t, s)
			if _, err := s.PrepareWorkspace(t.Context(), in, g.repository); err != nil {
				t.Fatal(err)
			}
			store := s.Store.(*Store)
			s.Store = &expiredTestEvidenceStore{Persistence: store, store: store, afterInvalidation: func() {
				p, err := store.ForkProgress(t.Context(), w.Event.Key)
				if err != nil {
					t.Fatal(err)
				}
				switch change {
				case "expired heartbeat":
					p.Validation.HeartbeatAt = time.Now().Add(-3 * time.Minute).Unix()
					// Model elapsed time directly. Normal progress saves preserve
					// newer heartbeats and cannot move this lease backward.
					if _, err := store.db.ExecContext(t.Context(), "UPDATE ghraft_meta SET value=? WHERE name=?", string(canonical(p)), forkProgressName(w.Event.Key)); err != nil {
						t.Fatal(err)
					}
					return
				case "expired deadline":
					p.Validation.Deadline = time.Now().Add(-time.Minute).Unix()
				case "new attempt":
					p.Validation.ID = strings.Repeat("b", 64)
				}
				if err := store.SaveForkProgress(t.Context(), w.Event.Key, p.Revision, p, nil); err != nil {
					t.Fatal(err)
				}
			}}
			got, err := s.TestWorkspace(t.Context(), in)
			if err == nil || got.Passed {
				t.Fatal("unowned attempt saved proof", got, err)
			}
			p, err := store.ForkProgress(t.Context(), w.Event.Key)
			if err != nil || p.ValidatedHead != "" {
				t.Fatal("proof was persisted", p, err)
			}
			if change == "new attempt" && (p.Validation.ID != strings.Repeat("b", 64) || p.Validation.State != "running") {
				t.Fatal("late failure overwrote replacement attempt")
			}
		})
	}
}
