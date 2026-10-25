package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRetentionPreservesRecoveryAndImmutableResults(t *testing.T) {
	s := freshTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()
	old := now - 60*86400
	keys := map[string]string{}
	bodies := map[string][]byte{}
	seed := func(number int, accepted bool, acceptedAt int64, triaged bool, processedAt int64) (string, Triage) {
		t.Helper()
		key := fmt.Sprintf("example/fixture#%d", number)
		body := canonical(Snapshot{key, "public", Objects{"issue": {"body": strings.Repeat("issue content ", 3000)}}})
		id, err := s.Observe(ctx, key, body)
		if err != nil || id == "" {
			t.Fatal(err)
		}
		keys[id], bodies[id] = key, body
		if accepted {
			if err := s.Delivery(ctx, id, []byte(`{"messageId":"delivered"}`), true, 0); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.db.ExecContext(ctx, "UPDATE ghraft_events SET created_at=?,accepted_at=? WHERE event_id=?", old, acceptedAt, id); err != nil {
			t.Fatal(err)
		}
		triage := Triage{EventID: id, Outcome: "quiet", Reason: strings.Repeat("reason ", 1000)}
		if triaged {
			if err := s.RecordTriage(ctx, triage); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, "UPDATE ghraft_triage SET created_at=?,body_hash='' WHERE event_id=?", processedAt, id); err != nil {
				t.Fatal(err)
			}
		}
		return id, triage
	}
	completed, triage := seed(1, true, old, true, old)
	pending, _ := seed(2, false, 0, true, old) // processed but still needs delivery confirmation
	unprocessed, _ := seed(3, true, old, false, 0)
	recentDelivery, _ := seed(4, true, now, true, old)
	recentReport, _ := seed(5, true, old, true, old)
	newEvent, _ := seed(6, false, 0, false, 0)

	// Simulate an old report that embedded a full guide and had no fingerprint.
	report := canonical(map[string]any{"event_id": completed, "raft_report_message_id": fixtureReport, "raft_task_message_id": fixtureTask, "summary": strings.Repeat("summary ", 1000), "guide_version": hash([]byte("guide")), "guide": map[string]string{"body": strings.Repeat("guidance ", 5000)}})
	if err := s.db.Session(ctx).Create(&resultRecord{EventID: completed, ReportID: fixtureReport, Body: string(report), CreatedAt: old}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Result(ctx, recentReport, "recent", []byte(`{"summary":"just finished"}`)); err != nil {
		t.Fatal(err)
	}
	_, version, _ := s.Snapshot(ctx, keys[completed])
	if _, err := s.BindTask(ctx, keys[completed], WorkInput{EventID: completed, EvidenceVersion: version, GuideVersion: hash([]byte("guide")), TaskID: fixtureTask}, "#work"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutMeta(ctx, "notifications-cursor", "durable-cursor"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{pending, completed} {
		if err := s.PutMeta(ctx, "external-delivery:"+id, "immutable-request"); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		key     string
		expires int64
	}{{"expired", now - 1}, {"active", now + 3600}} {
		if err := s.SaveSecret(ctx, item.key, []byte("fixture-secret"), item.expires); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.Session(ctx).Create(&legacyRecord{Marker: "old-import", CreatedAt: old, Body: `{"marker":"old-import","watch":"example/fixture#1","outcome":"notified","message":"legacy-ref","reason":"discard this"}`}).Error; err != nil {
		t.Fatal(err)
	}
	before, _ := s.Event(ctx, completed)
	preview, err := s.Cleanup(ctx, 30, true)
	if err != nil || preview.Events != 1 || preview.Triages != 1 || preview.Reports != 1 || preview.DeliveryRequests != 1 || preview.ExpiredSecrets != 1 || preview.LegacyEntries != 1 {
		t.Fatal("wrong cleanup candidates", preview, err)
	}
	unchanged, _ := s.Event(ctx, completed)
	if !bytes.Equal(before.Payload, unchanged.Payload) {
		t.Fatal("preview changed history")
	}
	if v, _ := s.Meta(ctx, "external-delivery:"+completed, ""); v == "" {
		t.Fatal("preview deleted a request")
	}
	actual, err := s.Cleanup(ctx, 30, false)
	preview.DryRun = false
	if err != nil || actual != preview {
		t.Fatal("cleanup did not match preview", actual, preview, err)
	}
	after, _ := s.Event(ctx, completed)
	if !bytes.Contains(after.Payload, []byte(`"body_pruned":true`)) || bytes.Contains(after.Payload, []byte("issue content")) || len(after.Payload) >= len(before.Payload)/10 {
		t.Fatal("completed event body was not compacted")
	}
	t.Logf("synthetic completed event payload: %d -> %d bytes", len(before.Payload), len(after.Payload))
	for _, id := range []string{pending, unprocessed, recentDelivery, recentReport, newEvent} {
		event, err := s.Event(ctx, id)
		if err != nil || !bytes.Contains(event.Payload, []byte("issue content")) {
			t.Fatal("unresolved or recent evidence was pruned", id, err)
		}
	}
	for id, body := range bodies {
		snapshot, _, err := s.Snapshot(ctx, keys[id])
		if err != nil || !bytes.Equal(snapshot, body) {
			t.Fatal("latest snapshot changed", err)
		}
	}
	if _, err := s.Secret(ctx, "expired"); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired secret remains", err)
	}
	if _, err := s.Secret(ctx, "active"); err != nil {
		t.Fatal("active credential removed", err)
	}
	if v, _ := s.Meta(ctx, "external-delivery:"+pending, ""); v != "immutable-request" {
		t.Fatal("ambiguous delivery request lost")
	}
	if v, _ := s.Meta(ctx, "notifications-cursor", ""); v != "durable-cursor" {
		t.Fatal("discovery cursor lost")
	}
	// Reopen before replaying acknowledgements: fingerprints must be durable.
	reopened := openTestStore(t)
	if err := reopened.RecordTriage(ctx, triage); err != nil {
		t.Fatal("compacted triage retry failed", err)
	}
	triage.Reason = "changed after cleanup"
	if err := reopened.RecordTriage(ctx, triage); err == nil {
		t.Fatal("compacted triage accepted conflicting content")
	}
	if err := reopened.Result(ctx, completed, fixtureReport, report); err != nil {
		t.Fatal("compacted legacy report retry failed", err)
	}
	if err := reopened.Result(ctx, completed, fixtureReport, []byte(`{"summary":"different"}`)); err == nil {
		t.Fatal("compacted report accepted conflicting content")
	}
	work, err := reopened.ReadWork(ctx, completed)
	if err != nil || work.Task == nil || work.Task.TaskID != fixtureTask {
		t.Fatal("task association lost", err)
	}
	history, err := reopened.History(ctx, keys[completed])
	if err != nil || len(history) != 3 {
		t.Fatal("processing references lost", err, history)
	}
	for _, h := range history {
		if !bytes.Contains(h, []byte(`"body_pruned":true`)) || bytes.Contains(h, []byte(`"summary":`)) || bytes.Contains(h, []byte(`"reason":`)) || bytes.Contains(h, []byte(`"guide":`)) {
			t.Fatal("history still retains prose", string(h))
		}
	}
	remaining, err := reopened.UnprocessedPage(ctx, 0)
	if err != nil || len(remaining) != 2 {
		t.Fatal("processed events reappeared", err, remaining)
	}
	if id, err := reopened.Observe(ctx, keys[completed], bodies[completed]); err != nil || id != "" {
		t.Fatal("restart/cleanup replayed the same observation", id, err)
	}
	if id, err := reopened.Observe(ctx, keys[completed], []byte(`{"title":"new change"}`)); err != nil || id == "" {
		t.Fatal("cleanup suppressed a new change", id, err)
	}
	repeat, err := reopened.Cleanup(ctx, 30, false)
	if err != nil || repeat != (CleanupStats{RetentionDays: 30}) {
		t.Fatal("cleanup was not idempotent", repeat, err)
	}
}

func TestAcceptedDeliveryDiscardsRequestWithoutResending(t *testing.T) {
	s := freshTestStore(t)
	ctx := context.Background()
	id, err := s.Observe(ctx, "example/fixture#1", []byte(`{"title":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{"first immutable request", "different request"} {
		request, receipt, err := s.PrepareDelivery(ctx, id, candidate)
		if err != nil || request != "first immutable request" || receipt != nil {
			t.Fatal("pending request was replaced", request, err)
		}
	}
	accepted := []byte(`{"messageId":"confirmed"}`)
	if err := s.Delivery(ctx, id, accepted, true, 0); err != nil {
		t.Fatal(err)
	}
	if request, _ := s.Meta(ctx, "external-delivery:"+id, ""); request != "" {
		t.Fatal("accepted request retained")
	}
	if err := s.Delivery(ctx, id, nil, false, 1); err != nil {
		t.Fatal(err)
	}
	request, receipt, err := s.PrepareDelivery(ctx, id, "must not recreate")
	if err != nil || request != "" || !bytes.Equal(receipt, accepted) {
		t.Fatal("accepted delivery prepared a new send", request, string(receipt), err)
	}
	if pending, err := s.Events(ctx, true); err != nil || len(pending) != 0 {
		t.Fatal("late failed response undid acceptance", pending, err)
	}
}

func TestRetentionUpgradeDoesNotImmediatelyDiscardOldHistory(t *testing.T) {
	s := freshTestStore(t)
	ctx := context.Background()
	id, _ := s.Observe(ctx, "example/fixture#1", []byte(`{"title":"old"}`))
	if err := s.Delivery(ctx, id, []byte(`{"status":"queued"}`), true, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTriage(ctx, Triage{EventID: id, Outcome: "quiet", Reason: "old reason"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE ghraft_triage SET created_at=1 WHERE event_id=?", id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE ghraft_events SET created_at=1,accepted_at=0 WHERE event_id=?", id); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Session(ctx).Create(&legacyRecord{Marker: "legacy", Body: `{"watch":"example/fixture#1"}`}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err := s.Cleanup(ctx, 30, false)
	if err != nil || stats.Events != 0 || stats.Triages != 0 || stats.LegacyEntries != 0 {
		t.Fatal("unknown completion time guessed from capture time", stats, err)
	}
	// Malformed stored evidence rolls back all body changes for that event.
	if _, err := s.db.ExecContext(ctx, "UPDATE ghraft_events SET accepted_at=1,payload='invalid' WHERE event_id=?", id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cleanup(ctx, 30, false); err == nil {
		t.Fatal("malformed evidence silently removed")
	}
	var triage triageRecord
	if err := s.db.Session(ctx).Where("event_id = ?", id).Take(&triage).Error; err != nil || triage.BodyPruned || !json.Valid([]byte(triage.Body)) {
		t.Fatal("partial event cleanup committed", err)
	}
}

func TestCleanupPaginationAndConcurrentReports(t *testing.T) {
	s := freshTestStore(t)
	ctx := context.Background()
	old := time.Now().Add(-60 * 24 * time.Hour).Unix()
	ids := make([]string, 101)
	for i := range ids {
		id, err := s.Observe(ctx, fmt.Sprintf("example/fixture#%d", i+1), []byte(`{"title":"old"}`))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
		if err = s.Delivery(ctx, id, []byte(`{"status":"queued"}`), true, 0); err != nil {
			t.Fatal(err)
		}
		if err = s.RecordTriage(ctx, Triage{EventID: id, Outcome: "quiet", Reason: "old reason"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE ghraft_events SET accepted_at=?", old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE ghraft_triage SET created_at=?", old); err != nil {
		t.Fatal(err)
	}
	preview, err := s.Cleanup(ctx, 30, true)
	if err != nil || preview.Events != 101 {
		t.Fatal("cleanup stopped at first page", preview, err)
	}
	other := openTestStore(t)
	done := make(chan error, 2)
	go func() {
		_, err := s.Cleanup(ctx, 30, false)
		done <- err
	}()
	go func() {
		for _, id := range ids {
			if err := other.Result(ctx, id, "new-report", []byte(`{"summary":"fresh evidence"}`)); err != nil {
				done <- err
				return
			}
			if err := other.RecordTriage(ctx, Triage{EventID: id, Outcome: "quiet", Reason: "old reason"}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	var reports []resultRecord
	if err := s.db.Session(ctx).Find(&reports).Error; err != nil || len(reports) != 101 {
		t.Fatal("concurrent reports lost", err, len(reports))
	}
	for _, report := range reports {
		if report.BodyPruned || !strings.Contains(report.Body, "fresh evidence") {
			t.Fatal("fresh report raced with cleanup", report.EventID)
		}
	}
	if stats, err := s.Cleanup(ctx, 30, false); err != nil || stats.Events != 0 || stats.Reports != 0 || stats.Triages != 0 {
		t.Fatal("fresh processing did not restart retention window", stats, err)
	}
}
