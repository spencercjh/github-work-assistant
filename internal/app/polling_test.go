package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func terminalPollFixture(t *testing.T, pull bool) (*Service, string, func() int) {
	t.Helper()
	g, fixture := fakeGH(t, strings.Repeat("a", 40), strings.Repeat("b", 40))
	data := readFixture(t, fixture)
	issue := obj(data["repos/example/fixture/issues/1"])
	issue["state"], issue["updated_at"] = "closed", "2026-10-01T00:00:00Z"
	if pull {
		pr := obj(data["repos/example/fixture/pulls/1"])
		pr["state"], pr["merged"] = "closed", true
	} else {
		delete(issue, "pull_request")
	}
	writeFixture(t, fixture, data)
	log := filepath.Join(t.TempDir(), "requests")
	t.Setenv("GHRAFT_REQUEST_LOG", log)
	s := &Service{Config: Config{Watches: []Watch{{"example/fixture", 1}}, PollSeconds: 120}, GH: g, Store: freshTestStore(t)}
	if err := s.reconcileWatches(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s, fixture, func() int {
		b, err := os.ReadFile(log)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return strings.Count(string(b), "\n")
	}
}

func TestTerminalPollingStopsUntilNotificationOrExplicitRefresh(t *testing.T) {
	for _, pull := range []bool{false, true} {
		name, fullRequests := "issue", 4
		if pull {
			name, fullRequests = "pull", 10
		}
		t.Run(name, func(t *testing.T) {
			s, fixture, requests := terminalPollFixture(t, pull)
			ctx, w := t.Context(), s.Config.Watches[0]
			// Seed an old collector snapshot with no polling metadata. Upgrades
			// must also stop existing terminal watches immediately.
			if err := s.collectWatch(ctx, w); err != nil {
				t.Fatal(err)
			}
			body, version, _ := s.Store.Snapshot(ctx, w.Key())
			observed, _ := s.Store.ObservedAt(ctx, w.Key())
			for range 30 {
				if attempted, err := s.pollWatch(ctx, w); err != nil || attempted {
					t.Fatal("terminal work was polled without a trigger", attempted, err)
				}
			}
			s = &Service{Config: s.Config, GH: s.GH, Store: openTestStore(t)}
			if attempted, err := s.pollWatch(ctx, w); err != nil || attempted || requests() != fullRequests {
				t.Fatal("restart resumed terminal polling", err, requests())
			}
			after, afterVersion, _ := s.Store.Snapshot(ctx, w.Key())
			afterObserved, _ := s.Store.ObservedAt(ctx, w.Key())
			events, _ := s.Store.Events(ctx, true)
			if string(body) != string(after) || version != afterVersion || observed != afterObserved || len(events) != 1 {
				t.Fatal("suspending polling changed evidence or discarded pending work")
			}
			// Reopening without a personal notification is intentionally not
			// detected in the background. An explicit refresh resumes polling.
			data := readFixture(t, fixture)
			obj(data["repos/example/fixture/issues/1"])["state"] = "open"
			pr := obj(data["repos/example/fixture/pulls/1"])
			pr["state"], pr["merged"] = "open", false
			writeFixture(t, fixture, data)
			if attempted, err := s.pollWatch(ctx, w); err != nil || attempted {
				t.Fatal(err)
			}
			if err := s.collectWatch(ctx, w); err != nil {
				t.Fatal(err)
			}
			if attempted, err := s.pollWatch(ctx, w); err != nil || !attempted || requests() != 3*fullRequests {
				t.Fatal("explicitly discovered reopen did not resume active polling", err, requests())
			}
		})
	}
}

type pollingSource struct {
	GH
	notifications []Notification
	failure       error
	discoveryErr  error
	polled        []Watch
}

func (g *pollingSource) Discover(context.Context, string) ([]Notification, string, error) {
	return g.notifications, time.Now().UTC().Format(time.RFC3339), g.discoveryErr
}
func (g *pollingSource) Collect(ctx context.Context, w Watch) ([]byte, error) {
	g.polled = append(g.polled, w)
	if g.failure != nil {
		return nil, g.failure
	}
	return g.GH.Collect(ctx, w)
}

func TestNotificationWakeSurvivesOverlapFailureAndRestart(t *testing.T) {
	s, fixture, _ := terminalPollFixture(t, true)
	ctx, w := t.Context(), s.Config.Watches[0]
	g := &pollingSource{GH: s.GH.(GH), notifications: []Notification{{Watch: w, UpdatedAt: 100, Version: "first"}}}
	s.GH, s.Config.Discover = g, true
	if err := s.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Collect(ctx); err != nil || len(g.polled) != 1 {
		t.Fatal("overlapping notification response retriggered a terminal refresh", err)
	}
	// Persist a fresh trigger, fail its refresh, then return no notifications.
	// The cursor has advanced, but the work must survive and retry on restart.
	g.notifications[0].UpdatedAt, g.notifications[0].Version = 101, "second"
	g.failure = ErrGitHubRateLimited
	s.Config.Watches = append(s.Config.Watches, Watch{w.Repo, 2})
	if err := s.Collect(ctx); !errors.Is(err, ErrGitHubRateLimited) || len(g.polled) != 2 {
		t.Fatal("rate limit did not stop the rest of the watch scan", err)
	}
	applied, _ := s.Store.Meta(ctx, appliedNotificationKey(w.Key()), "")
	if applied != "first" {
		t.Fatal("failed refresh consumed its durable trigger")
	}
	s.Config.Watches = s.Config.Watches[:1]
	g.notifications, g.failure = nil, nil
	data := readFixture(t, fixture)
	data["repos/example/fixture/issues/1/comments?per_page=100"] = []any{[]any{map[string]any{"id": 9, "body": "new after close"}}}
	writeFixture(t, fixture, data)
	// Use a fresh DB handle as well as a fresh service instance.
	s = &Service{Config: s.Config, GH: g, Store: openTestStore(t)}
	// The explicit second watch is no longer authorized after configuration removal.
	if err := s.Collect(ctx); err != nil || len(g.polled) != 3 {
		t.Fatal("restart did not retry the unsatisfied notification", err)
	}
	body, _, _ := s.Store.Snapshot(ctx, w.Key())
	if !strings.Contains(string(body), "new after close") {
		t.Fatal("notification-triggered update was lost")
	}
	// Old overlap data may not roll the watermark back and trigger another read.
	g.notifications = []Notification{{Watch: w, UpdatedAt: 100, Version: "first"}}
	if err := s.Collect(ctx); err != nil || len(g.polled) != 3 {
		t.Fatal("old notification replaced a newer watermark", err)
	}
	// A new notification discovers reopening, after which independent CI
	// changes must still be collected even without another notification.
	g.notifications = []Notification{{Watch: w, UpdatedAt: 102, Version: "third"}}
	data = readFixture(t, fixture)
	obj(data["repos/example/fixture/issues/1"])["state"] = "open"
	pr := obj(data["repos/example/fixture/pulls/1"])
	pr["state"], pr["merged"] = "open", false
	writeFixture(t, fixture, data)
	if err := s.Collect(ctx); err != nil || len(g.polled) != 4 {
		t.Fatal("notification did not discover reopening", err)
	}
	g.notifications = nil
	data["repos/example/fixture/commits/"+strings.Repeat("a", 40)+"/check-runs?per_page=100"] = []any{map[string]any{"check_runs": []any{map[string]any{"id": 7, "name": "regression", "status": "completed", "conclusion": "failure"}}}}
	writeFixture(t, fixture, data)
	if err := s.Collect(ctx); err != nil || len(g.polled) != 5 {
		t.Fatal("reopening did not resume active polling", err)
	}
	body, _, _ = s.Store.Snapshot(ctx, w.Key())
	if !strings.Contains(string(body), "regression") || !strings.Contains(string(body), "failure") {
		t.Fatal("CI changes after reopening were not collected")
	}
	g.discoveryErr = ErrGitHubRateLimited
	if err := s.Collect(ctx); !errors.Is(err, ErrGitHubRateLimited) || len(g.polled) != 5 {
		t.Fatal("discovery rate limit did not stop subsequent resource queries", err)
	}
}
