package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func changedEvent(t *testing.T, svc *Service, version int) Event {
	t.Helper()
	id, err := svc.Store.Observe(t.Context(), "example/fixture#1", []byte(fmt.Sprintf(`{"title":"version %d"}`, version)))
	if err != nil || id == "" {
		t.Fatal("new observation", err)
	}
	ev, err := svc.Store.Event(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func deliverEvent(t *testing.T, svc *Service, event Event) TaskDispatch {
	t.Helper()
	b, err := svc.Raft.Send(t.Context(), event)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.Delivery(t.Context(), event.ID, b, true, 0); err != nil {
		t.Fatal(err)
	}
	var receipt TaskDispatch
	if err = json.Unmarshal(b, &receipt); err != nil || !receipt.valid() {
		t.Fatal("invalid receipt", err)
	}
	return receipt
}

func TestConcurrentEventsShareOneDiscussionAcrossConnections(t *testing.T) {
	f := &taskAPI{status: "todo"}
	svc, first := sharedFixture(t, f)
	events := []Event{first, changedEvent(t, svc, 2), changedEvent(t, svc, 3)}
	other := *svc.Raft
	other.Store = openTestStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			sender := svc.Raft
			if i%2 == 0 {
				sender = &other
			}
			event := events[i%len(events)]
			receipt, err := sender.Send(t.Context(), event)
			if err != nil {
				t.Error(err)
				return
			}
			if err = sender.Store.Delivery(t.Context(), event.ID, receipt, true, 0); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if f.creates != 1 || len(f.updates) != 2 {
		t.Fatalf("expected one root and two distinct replies, got %d/%d", f.creates, len(f.updates))
	}
	for _, event := range events {
		w, err := svc.Store.ReadWork(t.Context(), event.ID)
		var receipt TaskDispatch
		if err != nil || w.Event.State != "accepted" || json.Unmarshal(w.Event.Receipt, &receipt) != nil || receipt.MessageID != fixtureTask || receipt.Number != 17 {
			t.Fatal("events diverged into different discussions", err)
		}
	}
}

func TestFixedDiscussionSurvivesCompletionAndManualHandoff(t *testing.T) {
	f := &taskAPI{owner: "worker", status: "in_progress"}
	svc, first := sharedFixture(t, f)
	deliverEvent(t, svc, first)
	second := changedEvent(t, svc, 2)
	receipt := deliverEvent(t, svc, second)
	if receipt.MessageID != fixtureTask || receipt.NotificationMessageID != fixtureReport || f.creates != 1 {
		t.Fatal("new event did not reuse the original root")
	}
	if !strings.Contains(f.updates["github-update:"+second.ID], "@worker") {
		t.Fatal("existing owner not notified")
	}
	f.status = "done"
	third := changedEvent(t, svc, 3)
	deliverEvent(t, svc, third)
	if f.owner != "worker" || f.status != "done" || !strings.Contains(f.updates["github-update:"+third.ID], "恢复为 in_progress") {
		t.Fatal("completion lost ownership or collector changed task state")
	}
	if err := svc.VerifyClaim(t.Context(), third.ID, "@worker"); !errors.Is(err, ErrClaimUnavailable) {
		t.Fatal("owner must first read and reopen the completed task", err)
	}
	f.status = "in_progress" // The same executor explicitly resumes the task.
	if err := svc.VerifyClaim(t.Context(), third.ID, "@worker"); err != nil {
		t.Fatal(err)
	}
	f.owner = "successor" // The operator assigns the original task in Raft.
	if err := svc.VerifyClaim(t.Context(), first.ID, "@worker"); !errors.Is(err, ErrClaimUnavailable) {
		t.Fatal("previous owner retained authority after handoff", err)
	}
	if err := svc.VerifyClaim(t.Context(), third.ID, "@successor"); err != nil {
		t.Fatal("new owner could not resume", err)
	}
	f.status = "todo" // Assigning a successor can also leave the task in todo.
	fourth := changedEvent(t, svc, 4)
	deliverEvent(t, svc, fourth)
	update := f.updates["github-update:"+fourth.ID]
	if f.creates != 1 || !strings.Contains(update, "@successor") || !strings.Contains(update, "raft task claim") {
		t.Fatal("handoff created a new discussion or woke the old owner")
	}
	if err := svc.VerifyClaim(t.Context(), fourth.ID, "@successor"); !errors.Is(err, ErrClaimUnavailable) {
		t.Fatal("assigned todo must be started before executing", err)
	}
	f.status = "in_progress" // Only the assigned successor claims the original task.
	if err := svc.VerifyClaim(t.Context(), fourth.ID, "@successor"); err != nil {
		t.Fatal("assigned successor could not start the original task", err)
	}
	if err := svc.VerifyClaim(t.Context(), fourth.ID, "@worker"); !errors.Is(err, ErrClaimUnavailable) {
		t.Fatal("starting assigned todo restored the previous owner's authority", err)
	}
	f.status = "closed"
	fifth := changedEvent(t, svc, 5)
	deliverEvent(t, svc, fifth)
	if f.status != "closed" || !strings.Contains(f.updates["github-update:"+fifth.ID], "等待用户明确恢复") {
		t.Fatal("closed work automatically resumed")
	}
}

func TestDiscussionReplyIsFrozenAcrossLostReceiptRestartAndHandoff(t *testing.T) {
	f := &taskAPI{owner: "worker", status: "in_progress"}
	svc, first := sharedFixture(t, f)
	deliverEvent(t, svc, first)
	second := changedEvent(t, svc, 2)
	f.lostUpdate = true
	if _, err := svc.Raft.Send(t.Context(), second); err == nil {
		t.Fatal("lost reply acknowledged")
	}
	frozen := f.updates["github-update:"+second.ID]
	// Simulate an outbox written before the required draft protocol flag was
	// added. A retry must still preserve the message body and idempotency key.
	omitStoredDraftFlag(t, svc.Store, second.ID)
	svc.Raft.Store, svc.Store = openTestStore(t), openTestStore(t)
	f.owner = "successor"
	f.status = "done"
	deliverEvent(t, svc, second)
	if f.creates != 1 || len(f.updates) != 1 || f.updates["github-update:"+second.ID] != frozen {
		t.Fatal("ambiguous retry changed the request or duplicated work")
	}
	if err := svc.VerifyClaim(t.Context(), second.ID, "@worker"); !errors.Is(err, ErrClaimUnavailable) {
		t.Fatal("stale mention authorized previous owner", err)
	}
	third := changedEvent(t, svc, 3)
	f.lostUpdate = true
	if _, err := svc.Raft.Send(t.Context(), third); err == nil {
		t.Fatal("lost reply acknowledged")
	}
	name := "external-delivery:" + third.ID
	saved, _ := svc.Store.Meta(t.Context(), name, "")
	var pending map[string]any
	_ = json.Unmarshal([]byte(saved), &pending)
	pending["created_at"] = time.Now().Add(-24 * time.Hour).Unix()
	if err := svc.Store.PutMeta(t.Context(), name, string(canonical(pending))); err != nil {
		t.Fatal(err)
	}
	before := f.updateWrites
	if _, err := svc.Raft.Send(t.Context(), third); err == nil || !strings.Contains(err.Error(), "safe retry window") || f.updateWrites != before {
		t.Fatal("expired update retried as a new write", err)
	}
}

func TestMissingDiscussionNeverForksAndTaskConversationUsesItsRef(t *testing.T) {
	for _, anchored := range []bool{false, true} {
		t.Run(fmt.Sprint(anchored), func(t *testing.T) {
			f := &taskAPI{owner: "worker", status: "in_progress", anchored: anchored}
			svc, first := sharedFixture(t, f)
			deliverEvent(t, svc, first)
			second := changedEvent(t, svc, 2)
			f.missingRoot = true
			if _, err := svc.Raft.Send(t.Context(), second); err == nil || f.creates != 1 || f.updateWrites != 0 {
				t.Fatal("missing root silently recreated", err)
			}
			f.missingRoot = false
			deliverEvent(t, svc, second)
			var request struct{ Target string }
			_ = json.Unmarshal([]byte(f.updates["github-update:"+second.ID]), &request)
			want := "#github-work:" + fixtureTask[:8]
			if anchored {
				want = "#github-work:#17"
			}
			if request.Target != want {
				t.Fatal("wrong conversation", request.Target)
			}
		})
	}
}

func TestLegacyDiscussionAdoptionRequiresAnUnambiguousRoot(t *testing.T) {
	f := &taskAPI{owner: "worker", status: "in_progress"}
	svc, first := sharedFixture(t, f)
	oldReceipt, err := svc.Raft.createSharedTask(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.Delivery(t.Context(), first.ID, oldReceipt, true, 0); err != nil {
		t.Fatal(err)
	}
	second := changedEvent(t, svc, 2)
	otherRoot := TaskDispatch{Channel: "#github-work", Number: 18, MessageID: fixtureReport}
	if err = svc.Store.Delivery(t.Context(), second.ID, canonical(otherRoot), true, 0); err != nil {
		t.Fatal(err)
	}
	third := changedEvent(t, svc, 3)
	if _, err = svc.Raft.Send(t.Context(), third); err == nil || !strings.Contains(err.Error(), "multiple existing tasks") || f.creates != 1 {
		t.Fatal("ambiguous legacy tasks automatically selected or forked", err)
	}
	if _, err = svc.AdoptDiscussion(t.Context(), third.ID, "99999999-9999-4999-8999-999999999999"); err == nil {
		t.Fatal("unrelated task adopted")
	}
	if _, err = svc.AdoptDiscussion(t.Context(), third.ID, fixtureTask); err != nil {
		t.Fatal(err)
	}
	receipt := deliverEvent(t, svc, third)
	if receipt.MessageID != fixtureTask || f.creates != 1 || len(f.updates) != 1 {
		t.Fatal("operator's selected task was not retained")
	}
	// The old second event's receipt points elsewhere, but its authorization
	// must now follow the selected fixed root as well.
	if err = svc.VerifyClaim(t.Context(), second.ID, "@worker"); err != nil {
		t.Fatal("old event did not use the canonical owner", err)
	}
}

func TestSingleLegacyTaskIsReusedAndUnresolvedLegacyWriteStopsUpgrade(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprint(accepted), func(t *testing.T) {
			f := &taskAPI{owner: "worker", status: "in_progress", lostReply: !accepted}
			svc, first := sharedFixture(t, f)
			receipt, err := svc.Raft.createSharedTask(t.Context(), first)
			if accepted {
				if err != nil {
					t.Fatal(err)
				}
				if err = svc.Store.Delivery(t.Context(), first.ID, receipt, true, 0); err != nil {
					t.Fatal(err)
				}
			}
			second := changedEvent(t, svc, 2)
			_, err = svc.Raft.Send(t.Context(), second)
			if accepted {
				if err != nil || len(f.updates) != 1 || f.creates != 1 {
					t.Fatal("single legacy root was not reused", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "unresolved legacy delivery") || f.writes != 1 {
				t.Fatal("legacy ambiguous create was bypassed", err)
			}
		})
	}
}
