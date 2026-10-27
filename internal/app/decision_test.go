package app

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func decisionFixture(t *testing.T) (*Service, *taskAPI, WorkResult) {
	t.Helper()
	f := &taskAPI{owner: "worker", status: "in_progress", reportID: fixtureReport, reportAuthor: "worker", reportKind: "agent", reportContent: "@owner Should we preserve the old API? I recommend preserving it until clients migrate."}
	s, ev := sharedFixture(t, f)
	s.Config.DecisionMaker, s.Raft.Config.DecisionMaker = "@owner", "@owner"
	deliverEvent(t, s, ev)
	in := lifecycleInput(t, s, ev.ID)
	if _, err := s.LinkTask(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	return s, f, WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "needs-decision", Summary: "Choose whether to preserve the old API; recommend preserving it during migration."}
}

func TestDecisionRequiresPublishedMentionInOriginalDiscussion(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*taskAPI)
	}{
		{"missing mention", func(f *taskAPI) { f.reportContent = "Please decide whether to preserve the old API." }},
		{"quoted mention", func(f *taskAPI) { f.reportContent = "> @owner Please decide." }},
		{"inline code", func(f *taskAPI) { f.reportContent = "`@owner` Please decide." }},
		{"fenced code", func(f *taskAPI) { f.reportContent = "```\n@owner Please decide.\n```" }},
		{"escaped mention", func(f *taskAPI) { f.reportContent = "\\@owner Please decide." }},
		{"wrong human", func(f *taskAPI) { f.reportContent = "@someone Please decide." }},
		{"shared handle prefix", func(f *taskAPI) { f.reportContent = "@owner-other Please decide." }},
		{"no question", func(f *taskAPI) { f.reportContent = "@owner" }},
		{"wrong executor", func(f *taskAPI) { f.reportAuthor = "someone" }},
		{"human author", func(f *taskAPI) { f.reportKind = "user" }},
		{"different discussion", func(f *taskAPI) { f.historyTarget = "#github-work:00000000" }},
		{"no published report", func(f *taskAPI) { f.reportID = "" }},
		{"different message", func(f *taskAPI) { f.reportID = fixtureTask }},
		{"history unavailable", func(f *taskAPI) { f.historyStatus = 503 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, in := decisionFixture(t)
			tc.edit(f)
			if err := s.RecordWorkResult(t.Context(), in); err == nil {
				t.Fatal("unaddressed decision accepted")
			}
			w, err := s.WorkContext(t.Context(), in.EventID)
			if err != nil || len(w.History) != 0 {
				t.Fatal("failed request was recorded as handled", err)
			}
		})
	}
}

func TestDecisionReportReuseDoesNotSendOrMentionAgain(t *testing.T) {
	s, f, in := decisionFixture(t)
	for _, alternate := range []bool{false, true} {
		f.alternateHistoryFields = alternate
		if err := s.RecordWorkResult(t.Context(), in); err != nil {
			t.Fatal(err)
		}
	}
	// A new event still blocked on the same unanswered question can reuse the
	// report. The collector must not generate another human notification.
	if _, err := s.Store.Observe(t.Context(), "example/fixture#1", []byte(`{"title":"new checks"}`)); err != nil {
		t.Fatal(err)
	}
	work, err := s.WorkContext(t.Context(), in.EventID)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := s.Store.Event(t.Context(), work.CurrentEventID)
	if err != nil {
		t.Fatal(err)
	}
	deliverEvent(t, s, ev)
	in.WorkInput = lifecycleInput(t, s, ev.ID)
	if _, err := s.LinkTask(t.Context(), in.WorkInput); err != nil {
		t.Fatal(err)
	}
	writes := f.updateWrites
	if err := s.RecordWorkResult(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if f.updateWrites != writes || f.creates != 1 || f.historyReads != 3 {
		t.Fatal("recording a decision sent another message or omitted verification")
	}
	if strings.Contains(f.first, "@owner") || !strings.Contains(f.first, "guide.decision_maker") {
		t.Fatal("routine task did not give decision instructions or mentioned the human")
	}
	for _, raw := range f.updates {
		if strings.Contains(raw, "@owner") || !strings.Contains(raw, "guide.decision_maker") {
			t.Fatal("routine update unexpectedly mentioned the human or lost instructions")
		}
	}
}

func TestDecisionConfigurationAndOwnershipRemainRequired(t *testing.T) {
	s, f, in := decisionFixture(t)
	// A changed decision-maker is trusted guidance: the old input must be
	// refreshed/rebound, and an arbitrary or agent handle is not a human member.
	s.Config.DecisionMaker, s.Raft.Config.DecisionMaker = "@worker", "@worker"
	if err := s.RecordWorkResult(t.Context(), in); !errors.Is(err, ErrStaleWork) {
		t.Fatal("decision configuration did not invalidate the old guide", err)
	}
	for _, who := range []string{"@worker", "@missing", ""} {
		s.Config.DecisionMaker, s.Raft.Config.DecisionMaker = who, who
		in.WorkInput = lifecycleInput(t, s, in.EventID)
		if _, err := s.LinkTask(t.Context(), in.WorkInput); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordWorkResult(t.Context(), in); err == nil {
			t.Fatal("missing human decision-maker accepted", who)
		}
	}
	s.Config.DecisionMaker, s.Raft.Config.DecisionMaker = "@owner", "@owner"
	in.WorkInput = lifecycleInput(t, s, in.EventID)
	if _, err := s.LinkTask(t.Context(), in.WorkInput); err != nil {
		t.Fatal(err)
	}
	f.owner = "successor"
	if err := s.RecordWorkResult(t.Context(), in); !errors.Is(err, ErrClaimUnavailable) {
		t.Fatal("old owner recorded a decision after handoff", err)
	}
	if f.historyReads != 0 {
		t.Fatal("denied caller read a report")
	}
}

func TestWaitingDoesNotRequireDecisionOrHistoryAccess(t *testing.T) {
	s, f, in := decisionFixture(t)
	s.Config.DecisionMaker, s.Raft.Config.DecisionMaker = "", ""
	in.WorkInput = lifecycleInput(t, s, in.EventID)
	if _, err := s.LinkTask(t.Context(), in.WorkInput); err != nil {
		t.Fatal(err)
	}
	in.Outcome, in.Summary = "waiting", "Waiting for CI; no human action needed."
	f.historyStatus = 403
	if err := s.RecordWorkResult(t.Context(), in); err != nil || f.historyReads != 0 {
		t.Fatal("ordinary waiting required a human mention", err)
	}
	w, _ := s.WorkContext(t.Context(), in.EventID)
	if len(w.History) != 1 || !json.Valid(w.History[0]) {
		t.Fatal("waiting result was not preserved")
	}
}
