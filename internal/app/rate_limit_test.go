package app

import (
	"errors"
	"reflect"
	"testing"
)

func TestForkReportsResumeAfterTemporaryGitHubFailure(t *testing.T) {
	s, g, in, before, _ := testedFork(t, []string{"push", "open-pr"})
	ctx := t.Context()
	result := WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "waiting", Summary: "Tests passed; awaiting remote checks."}
	for _, failure := range []error{ErrGitHubRateLimited, errors.New("temporary GitHub connection failure")} {
		g.accessErr = failure
		if err := s.collectFork(ctx, s.Config.Forks[0]); !errors.Is(err, failure) {
			t.Fatal("collection failure changed", err)
		}
		if _, err := s.LinkTask(ctx, in); !errors.Is(err, failure) {
			t.Fatal("task binding reports a temporary failure as lost authority", err)
		}
		if err := s.RecordWorkResult(ctx, result); !errors.Is(err, failure) {
			t.Fatal("report failure changed", err)
		}
		if rows, err := s.Pending(ctx); !errors.Is(err, failure) || rows != nil {
			t.Fatal("unavailable pending work silently hidden", err)
		}
		stored, err := s.Store.ForkProgress(ctx, before.Event.Key)
		if err != nil || !reflect.DeepEqual(stored, *before.Fork) {
			t.Fatal("failed access changed progress or invalidated tested commit", err)
		}
		g.accessErr = nil
		// No recollection, new event, task or test run is needed for recovery.
		after, err := s.WorkContext(ctx, in.EventID)
		if err != nil || !reflect.DeepEqual(before.Task, after.Task) || after.CurrentVersion != in.EvidenceVersion || !reflect.DeepEqual(before.Fork, after.Fork) {
			t.Fatal("recovery lost the original task or test evidence", err)
		}
	}
	if err := s.RecordWorkResult(ctx, result); err != nil {
		t.Fatal("report did not resume on the same event", err)
	}
	after, err := s.WorkContext(ctx, in.EventID)
	if err != nil || after.Fork.Phase != "waiting" || after.Fork.ValidatedHead != before.Fork.ValidatedHead || after.Fork.TestLogHash != before.Fork.TestLogHash || after.Fork.PR != before.Fork.PR {
		t.Fatal("report recovery lost the PR or test proof", err)
	}
	rows, err := s.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row["event_id"] == in.EventID {
			t.Fatal("successful report remains unprocessed")
		}
	}
	// A real permission loss must still prevent access after quota recovery.
	g.denied = true
	if _, err := s.WorkContext(ctx, in.EventID); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("actual access loss ignored", err)
	}
}
