package app

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestOrdinaryPRUpgradeDoesNotReplayMergeEvidence(t *testing.T) {
	for _, polluted := range []bool{false, true} {
		name := "original-collector"
		if polluted {
			name = "fork-evidence-already-collected"
		}
		t.Run(name, func(t *testing.T) {
			g, fixture := fakeGH(t, strings.Repeat("a", 40), strings.Repeat("b", 40))
			data := readFixture(t, fixture)
			obj(data["repos/example/fixture/issues/1"])["state"] = "closed"
			pull := obj(data["repos/example/fixture/pulls/1"])
			pull["state"], pull["merged"], pull["merge_commit_sha"] = "closed", true, strings.Repeat("c", 40)
			writeFixture(t, fixture, data)
			watch := Watch{"example/fixture", 1}
			body, err := g.Collect(t.Context(), watch)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot Snapshot
			if err = json.Unmarshal(body, &snapshot); err != nil {
				t.Fatal(err)
			}
			if _, exists := snapshot.Objects["pull"]["merge_commit_sha"]; exists {
				t.Fatal("ordinary PR collection exposes fork-only evidence")
			}
			baseline := body
			if polluted {
				snapshot.Objects["pull"]["merge_commit_sha"] = strings.Repeat("c", 40)
				baseline = canonical(snapshot)
			}
			s := freshTestStore(t)
			id, err := s.Observe(t.Context(), watch.Key(), baseline)
			if err != nil || id == "" {
				t.Fatal(id, err)
			}
			version := hash(baseline)
			if _, err = s.BindTask(t.Context(), watch.Key(), WorkInput{EventID: id, EvidenceVersion: version, TaskID: fixtureTask, GuideVersion: "guide"}, "#work"); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if next, err := s.Observe(t.Context(), watch.Key(), body); err != nil || next != "" {
					t.Fatal("upgrade or repeated collection notified historical merge", next, err)
				}
			}
			w, err := s.ReadWork(t.Context(), id)
			if err != nil || w.CurrentEventID != id || w.CurrentVersion != version || !bytes.Equal(w.Snapshot, baseline) || w.Task == nil || w.Task.EvidenceVersion != version {
				t.Fatal("field removal invalidated existing work or evidence", w, err)
			}
			// A later real comment must still produce a current, actionable event.
			data["repos/example/fixture/issues/1/comments?per_page=100"] = []any{[]any{map[string]any{"id": 7, "body": "Please clarify the fix"}}}
			writeFixture(t, fixture, data)
			body, err = g.Collect(t.Context(), watch)
			if err != nil {
				t.Fatal(err)
			}
			next, err := s.Observe(t.Context(), watch.Key(), body)
			if err != nil || next == "" || next == id {
				t.Fatal("real update lost after upgrade", next, err)
			}
			w, err = s.ReadWork(t.Context(), next)
			if err != nil || w.CurrentEventID != next || w.CurrentVersion != hash(body) {
				t.Fatal("real update has no current evidence", w, err)
			}
			var payload struct{ Changes []Change }
			if err = json.Unmarshal(w.Event.Payload, &payload); err != nil || len(payload.Changes) != 1 || payload.Changes[0].Object != "comment:7" {
				t.Fatal("field removal leaked into real update", payload, err)
			}
		})
	}
}

func TestOrdinaryPRStillNotifiesActualMerge(t *testing.T) {
	g, fixture := fakeGH(t, strings.Repeat("a", 40), strings.Repeat("b", 40))
	watch := Watch{"example/fixture", 1}
	s := freshTestStore(t)
	body, err := g.Collect(t.Context(), watch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Observe(t.Context(), watch.Key(), body); err != nil {
		t.Fatal(err)
	}
	data := readFixture(t, fixture)
	obj(data["repos/example/fixture/issues/1"])["state"] = "closed"
	pull := obj(data["repos/example/fixture/pulls/1"])
	pull["state"], pull["merged"], pull["merge_commit_sha"] = "closed", true, strings.Repeat("c", 40)
	writeFixture(t, fixture, data)
	body, err = g.Collect(t.Context(), watch)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Observe(t.Context(), watch.Key(), body)
	if err != nil || id == "" {
		t.Fatal("actual merge was suppressed", id, err)
	}
	w, err := s.ReadWork(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct{ Changes []Change }
	if err = json.Unmarshal(w.Event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	for _, change := range payload.Changes {
		if change.Object == "pull" && obj(change.Before)["merged"] == false && obj(change.After)["merged"] == true {
			return
		}
	}
	t.Fatal("actual merge evidence missing", payload)
}
