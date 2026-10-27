package app

import (
	"bytes"
	"strings"
	"testing"
)

func legacyForkSnapshot(t *testing.T, phase string) (*Store, ForkSpec, ForkProgress, ForkObservation, string) {
	t.Helper()
	s := freshTestStore(t)
	f := ForkSpec{ID: "legacy", Upstream: BranchRef{"source/fixture", "main"}, Target: BranchRef{"example/fixture", "main"}}
	o := ForkObservation{Classification: "public", Upstream: strings.Repeat("a", 40), Target: strings.Repeat("b", 40)}
	if err := s.ObserveFork(t.Context(), f, ForkProgress{}, o); err != nil {
		t.Fatal(err)
	}
	p, err := s.ForkProgress(t.Context(), f.Key())
	if err != nil {
		t.Fatal(err)
	}
	p.Phase, p.PR, p.NextAttempt = phase, 7, 0
	p.LastSuccess, p.ValidatedHead = p.Upstream, strings.Repeat("c", 40)
	if err = s.SaveForkProgress(t.Context(), f.Key(), p.Revision, p, nil); err != nil {
		t.Fatal(err)
	}
	p, err = s.ForkProgress(t.Context(), f.Key())
	if err != nil {
		t.Fatal(err)
	}
	o.PR, o.SyncHead, o.MergeCommit = p.PR, p.ValidatedHead, strings.Repeat("d", 40)
	o.ContainsLatest, o.ContainsPinned, o.ContainsValidated, o.MergeVerified = true, true, true, true
	o.Objects = Objects{"pull": {"state": "closed", "merged": true, "head": o.SyncHead, "merge_commit_sha": o.MergeCommit}, "check:1": {"name": "build", "status": "success"}}
	if err = s.ObserveFork(t.Context(), f, p, o); err != nil {
		t.Fatal(err)
	}
	_, version, err := s.Snapshot(t.Context(), f.Key())
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.CurrentEvent(t.Context(), f.Key(), version)
	if err != nil {
		t.Fatal(err)
	}
	return s, f, p, o, id
}

func TestCompletedForkUpgradePreservesExistingWork(t *testing.T) {
	s, f, p, o, id := legacyForkSnapshot(t, "synced")
	before, version, err := s.Snapshot(t.Context(), f.Key())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindTask(t.Context(), f.Key(), WorkInput{EventID: id, EvidenceVersion: version, TaskID: fixtureTask, GuideVersion: "guide"}, "#work"); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	// New verification rules no longer accept the legacy integration proof.
	o.ContainsValidated, o.MergeVerified, o.MergeCommit = false, false, ""
	for range 2 {
		if err = s.ObserveFork(t.Context(), f, p, o); err != nil {
			t.Fatal(err)
		}
	}
	after, err := s.Events(t.Context(), false)
	if err != nil || len(after) != len(events) {
		t.Fatal("verification-only upgrade created work", len(events), len(after), err)
	}
	w, err := s.ReadWork(t.Context(), id)
	if err != nil || w.CurrentEventID != id || w.CurrentVersion != version || !bytes.Equal(w.Snapshot, before) || w.Task == nil || w.Task.EvidenceVersion != version {
		t.Fatal("upgrade invalidated the completed work association", w, err)
	}
	progress, err := s.ForkProgress(t.Context(), f.Key())
	if err != nil || progress.Phase != "synced" || progress.LastSuccess != p.LastSuccess {
		t.Fatal("upgrade changed completed progress", progress, err)
	}
}

func TestForkUpgradeStillPublishesRelevantChanges(t *testing.T) {
	for _, change := range []string{"active-verification", "check", "pull", "comment", "target", "upstream", "visibility"} {
		t.Run(change, func(t *testing.T) {
			phase := "synced"
			if change == "active-verification" {
				phase = "waiting"
			}
			s, f, p, o, id := legacyForkSnapshot(t, phase)
			o.ContainsValidated, o.MergeVerified, o.MergeCommit = false, false, ""
			switch change {
			case "check":
				o.Objects["check:1"]["status"] = "failure"
			case "pull":
				o.Objects["pull"]["state"], o.Objects["pull"]["merged"] = "open", false
			case "comment":
				o.Objects["comment:1"] = map[string]any{"body": "Please follow up"}
			case "target":
				o.Target = strings.Repeat("e", 40)
			case "upstream":
				o.Upstream, o.ContainsLatest = strings.Repeat("f", 40), false
			case "visibility":
				o.Classification = "private"
			}
			if err := s.ObserveFork(t.Context(), f, p, o); err != nil {
				t.Fatal(err)
			}
			_, version, err := s.Snapshot(t.Context(), f.Key())
			if err != nil {
				t.Fatal(err)
			}
			next, err := s.CurrentEvent(t.Context(), f.Key(), version)
			if err != nil || next == "" || next == id {
				t.Fatal("relevant change was suppressed", next, err)
			}
		})
	}
}
