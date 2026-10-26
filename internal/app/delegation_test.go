package app

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

func currentEvent(t *testing.T, svc *Service) Event {
	t.Helper()
	_, version, err := svc.Store.Snapshot(t.Context(), "example/fixture#1")
	if err != nil {
		t.Fatal(err)
	}
	events, err := svc.Store.Events(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Version == version {
			return ev
		}
	}
	t.Fatal("current event missing")
	return Event{}
}

func delegatedFixture(t *testing.T) (*Service, *taskAPI, string, GitHubActionInput) {
	t.Helper()
	f := &taskAPI{owner: "worker", status: "in_progress"}
	svc, _ := sharedFixture(t, f)
	c := localPolicy(t, "local-edit")
	c.Repositories[0].GitHubActions = []string{"merge"}
	c.Repositories[0].TaskTarget = svc.Config.DeliveryTarget
	svc.Config.configDir = c.configDir
	svc.Config.Repositories = c.Repositories
	svc.Raft.Config = svc.Config
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	gh, path := fakeGH(t, head, base)
	svc.GH = gh
	if err := svc.collectWatch(t.Context(), svc.Config.Watches[0]); err != nil {
		t.Fatal(err)
	}
	ev := currentEvent(t, svc)
	deliverEvent(t, svc, ev)
	w, err := svc.WorkContext(t.Context(), ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	in := GitHubActionInput{WorkInput: WorkInput{Executor: "@worker", EventID: ev.ID, EvidenceVersion: w.CurrentVersion, GuideVersion: w.Guide.Version, TaskID: fixtureTask}, Action: "merge", Head: head, Base: base}
	if _, err := svc.LinkTask(t.Context(), in.WorkInput); err != nil {
		t.Fatal(err)
	}
	return svc, f, path, in
}

func TestDelegatedActionChecksAuthorityAndCurrentRevisions(t *testing.T) {
	svc, f, path, in := delegatedFixture(t)
	checked, err := svc.CheckGitHubAction(t.Context(), in)
	if err != nil || !checked.Permitted || checked.Repository != "example/fixture" {
		t.Fatal("standing merge delegation unavailable", checked, err)
	}
	// Fixture gh rejects every non-GET call. A permission check must not merge.
	if !strings.Contains(checked.Verification, "must assess") {
		t.Fatal("permission check misrepresented as merge readiness")
	}
	other := in
	other.Action = "comment" // The hostile fixture PR body asks for a review.
	if _, err = svc.CheckGitHubAction(t.Context(), other); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("undelegated write allowed", err)
	}
	other = in
	other.Head = strings.Repeat("c", 40)
	if _, err = svc.CheckGitHubAction(t.Context(), other); !errors.Is(err, ErrStaleWork) {
		t.Fatal("unreviewed head allowed", err)
	}
	f.owner = "successor"
	if _, err = svc.CheckGitHubAction(t.Context(), in); !errors.Is(err, ErrClaimUnavailable) {
		t.Fatal("old owner kept write authority", err)
	}
	f.owner = "worker"
	data := readFixture(t, path)
	obj(obj(data["repos/example/fixture/pulls/1"])["base"])["sha"] = strings.Repeat("d", 40)
	writeFixture(t, path, data)
	if _, err = svc.CheckGitHubAction(t.Context(), in); !errors.Is(err, ErrStaleWork) {
		t.Fatal("base movement did not invalidate reviewed evidence", err)
	}
}

func TestRevokingDelegationInvalidatesPendingPlan(t *testing.T) {
	svc, _, _, in := delegatedFixture(t)
	svc.Config.Repositories[0].GitHubActions = nil
	if _, err := svc.CheckGitHubAction(t.Context(), in); !errors.Is(err, ErrStaleWork) {
		t.Fatal("old guide survived revocation", err)
	}
	w, err := svc.WorkContext(t.Context(), in.EventID)
	if err != nil {
		t.Fatal(err)
	}
	in.GuideVersion = w.Guide.Version
	if _, err = svc.LinkTask(t.Context(), in.WorkInput); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.CheckGitHubAction(t.Context(), in); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("revoked write allowed after rebinding", err)
	}
}

func TestMergedResultRequiresObservedOutcome(t *testing.T) {
	svc, _, path, in := delegatedFixture(t)
	result := WorkResult{WorkInput: in.WorkInput, ReportID: fixtureReport, Outcome: "merged", Summary: "Merge verified."}
	if err := svc.RecordWorkResult(t.Context(), result); err == nil {
		t.Fatal("open PR recorded as merged")
	}
	result.Outcome = "waiting"
	result.Summary = "Waiting for the current commit's build; resume on checks changing."
	if err := svc.RecordWorkResult(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	data := readFixture(t, path)
	obj(data["repos/example/fixture/pulls/1"])["merged"] = true
	obj(data["repos/example/fixture/pulls/1"])["state"] = "closed"
	writeFixture(t, path, data)
	if _, err := svc.RefreshWorkContext(t.Context(), in.EventID); err != nil {
		t.Fatal(err)
	}
	ev := currentEvent(t, svc)
	deliverEvent(t, svc, ev)
	w, err := svc.WorkContext(t.Context(), ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	result.WorkInput.EventID, result.WorkInput.EvidenceVersion = ev.ID, w.CurrentVersion
	result.Outcome, result.Summary = "merged", "Observed actual merged PR state after the write."
	if _, err = svc.LinkTask(t.Context(), result.WorkInput); err != nil {
		t.Fatal(err)
	}
	if err = svc.RecordWorkResult(t.Context(), result); err != nil {
		t.Fatal(err)
	}
}

func TestWorkRequestDoesNotReplayArchivedReceipt(t *testing.T) {
	store := freshTestStore(t)
	gh, _ := fakeGH(t, strings.Repeat("a", 40), strings.Repeat("b", 40))
	c := localPolicy(t, "local-edit")
	c.Watches = []Watch{{Repo: "example/fixture", Number: 1}}
	svc := &Service{Config: c, Store: store, GH: gh}
	if err := svc.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	old := currentEvent(t, svc)
	archived := []byte(`{"archived_task":{"taskNumber":9}}`)
	if err := store.Delivery(t.Context(), old.ID, archived, true, 0); err != nil {
		t.Fatal(err)
	}
	ev, err := svc.RequestWork(t.Context(), old.ID, fixtureTask, "Apply the newly delegated work scope.")
	if err != nil || ev.ID == old.ID || ev.State != "pending" || ev.Version != old.Version {
		t.Fatal("request failed to create a distinct current consideration", ev, err)
	}
	var payload map[string]any
	if json.Unmarshal(ev.Payload, &payload) != nil || payload["kind"] != "work.requested" {
		t.Fatal("operator request disguised as GitHub activity")
	}
	other := openTestStore(t)
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			again, err := other.RequestWork(t.Context(), ev.Key, ev.Version, fixtureTask, "Apply the newly delegated work scope.")
			if err != nil || again.ID != ev.ID {
				t.Error("request retry duplicated work", err)
			}
		})
	}
	wg.Wait()
	before, err := store.ReadWork(t.Context(), old.ID)
	if err != nil || string(before.Event.Receipt) != string(archived) {
		t.Fatal("archived history changed", err)
	}
	events, _ := store.Events(t.Context(), false)
	if len(events) != 2 {
		t.Fatal("request replay created extra events", len(events))
	}
	svc.Config.Watches = nil
	if _, err = svc.RequestWork(t.Context(), old.ID, fixtureReport, "Try a revoked watch."); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("request bypassed removed repository access", err)
	}
}
