package app

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrivateRepositoryConfiguration(t *testing.T) {
	for _, tc := range []struct {
		patterns       []string
		valid, allowed bool
	}{
		{nil, true, false},
		{[]string{"*"}, true, true},
		{[]string{"Example/*"}, true, true},
		{[]string{"EXAMPLE/FIXTURE"}, true, true},
		{[]string{"example/other", "other/*"}, true, false},
		{[]string{"*/fixture"}, false, false},
		{[]string{"example/fi*"}, false, false},
		{[]string{"example/../fixture"}, false, false},
		{[]string{" example/*"}, false, false},
		{[]string{"example/*", "EXAMPLE/*"}, false, false},
	} {
		c := Config{RaftMode: "external", ServerID: "server", ExternalAgent: "sender", GitHubLogin: "fixture-user", DeliveryTarget: "#work", Discover: true, PrivateRepositories: tc.patterns}
		file := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(file, canonical(c), 0600); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadConfig(file)
		if (err == nil) != tc.valid {
			t.Fatalf("scope %v: %v", tc.patterns, err)
		}
		if tc.valid && privateRepositoryAllowed(loaded.PrivateRepositories, "example/fixture") != tc.allowed {
			t.Fatalf("wrong authorization for %v", tc.patterns)
		}
	}
	if err := (Config{PrivateNotificationsSince: "yesterday"}).validateRepositoryAccess(); err == nil {
		t.Fatal("invalid notification boundary accepted")
	}
}

func TestPrivateDiscoveryCollectionAndReferences(t *testing.T) {
	g, fixture := fakeGH(t, strings.Repeat("a", 40), strings.Repeat("b", 40))
	g.PrivateRepositories = []string{"example/*"}
	g.PrivateNotificationsSince = "2026-10-25T16:00:00Z"
	notification := func(repo, updated string, private any) map[string]any {
		return map[string]any{"subject": map[string]any{"type": "PullRequest", "url": "https://api.github.com/repos/" + repo + "/pulls/1"}, "repository": map[string]any{"full_name": repo, "private": private}, "updated_at": updated}
	}
	data := readFixture(t, fixture)
	data["notifications?all=true&per_page=50&since=2026-10-25T15%3A00%3A00Z"] = []any{
		[]any{notification("example/old", "2026-10-25T15:59:59Z", true), notification("other/denied", "2026-10-25T16:01:00Z", true)},
		[]any{notification("example/fixture", "2026-10-25T16:01:00Z", true), notification("public/repo", "2026-10-25T15:01:00Z", false), notification("example/unknown", "2026-10-25T16:01:00Z", nil)},
	}
	obj(data["repos/example/fixture"])["private"] = true
	reference := func(id int, repo string, private any) map[string]any {
		return map[string]any{"id": id, "event": "cross-referenced", "source": map[string]any{"type": "issue", "issue": map[string]any{"number": 2, "body": "do not copy source body", "repository": map[string]any{"full_name": repo, "private": private}}}}
	}
	data["repos/example/fixture/issues/1/timeline?per_page=100"] = []any{[]any{reference(1, "example/allowed", true), reference(2, "other/denied", true), reference(3, "example/unknown", nil)}}
	writeFixture(t, fixture, data)
	watches, _, err := g.Discover(t.Context(), "2026-10-25T15:00:00Z")
	if err != nil || len(watches) != 2 || watches[0].Key() != "example/fixture#1" || watches[1].Key() != "public/repo#1" {
		t.Fatal("discovery scope, cutoff or pagination failed", watches, err)
	}
	body, err := g.Collect(t.Context(), Watch{"example/fixture", 1})
	var snapshot Snapshot
	if err != nil || json.Unmarshal(body, &snapshot) != nil || snapshot.Classification != "private" {
		t.Fatal("private collection failed", err)
	}
	if snapshot.Objects["timeline:1"] != nil || snapshot.Objects["timeline:2"] != nil || snapshot.Objects["timeline:3"] != nil || strings.Contains(string(body), "do not copy source body") {
		t.Fatal("reference boundary failed")
	}
	obj(data["repos/example/fixture"])["private"] = false
	writeFixture(t, fixture, data)
	body, err = g.Collect(t.Context(), Watch{"example/fixture", 1})
	if err != nil || strings.Contains(string(body), "timeline:1") {
		t.Fatal("private reference entered public context", err)
	}
	obj(data["repos/example/fixture"])["private"] = true
	writeFixture(t, fixture, data)
	g.PrivateRepositories = nil
	if _, err = g.Collect(t.Context(), Watch{"example/fixture", 1}); !errors.Is(err, ErrRepositoryNotAllowed) {
		t.Fatal("removed private scope still collects", err)
	}
}

func TestPrivateHistoryAccessAndTaskDelivery(t *testing.T) {
	ctx := t.Context()
	store := freshTestStore(t)
	g, fixture := fakeGH(t, strings.Repeat("a", 40), strings.Repeat("b", 40))
	g.PrivateRepositories = []string{"example/*"}
	cutoff := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	store.PrivateSince = cutoff
	data := readFixture(t, fixture)
	obj(data["repos/example/fixture"])["private"] = true
	obj(data["repos/example/fixture/issues/1"])["body"] = "confidential fixture body"
	data["repos/example/fixture/issues/1/comments?per_page=100"] = []any{[]any{
		map[string]any{"id": 1, "body": "older private comment", "created_at": cutoff.Add(-time.Hour).Format(time.RFC3339)},
		map[string]any{"id": 2, "body": "recent private comment", "created_at": cutoff.Add(time.Second).Format(time.RFC3339)},
	}}
	writeFixture(t, fixture, data)
	f := &taskAPI{status: "todo"}
	api := httptest.NewServer(f)
	defer api.Close()
	cfg := Config{GitHubLogin: g.Login, PrivateRepositories: g.PrivateRepositories, Watches: []Watch{{"example/fixture", 1}}, RaftMode: "external", RaftAPI: api.URL, ExternalAgent: "sender-id", ServerID: "server-id", DeliveryTarget: "#github-work"}
	svc := &Service{Config: cfg, Store: store, GH: g, Raft: &Raft{Config: cfg, Store: store, Credential: "sk_agent_local_fixture", HTTP: SafeHTTP()}}
	if err := svc.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	events, err := svc.Events(ctx)
	if err != nil || len(events) != 1 {
		t.Fatal("private event missing", err)
	}
	event := events[0]
	if !strings.Contains(string(event.Payload), `"data_classification":"private"`) || strings.Contains(string(event.Payload), "older private comment") || !strings.Contains(string(event.Payload), "recent private comment") {
		t.Fatal("classification or first-observation cutoff lost")
	}
	if _, err := svc.WorkContext(ctx, event.ID); err != nil {
		t.Fatal("authorized private context denied", err)
	}
	// Scope removal applies immediately, before any collection or process restart.
	svc.Config.PrivateRepositories = nil
	if err := svc.Deliver(ctx); err != nil || f.writes != 0 {
		t.Fatal("revoked private event delivered", err)
	}
	if _, err := svc.WorkContext(ctx, event.ID); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("revoked context readable", err)
	}
	if rows, err := svc.Events(ctx); err != nil || len(rows) != 0 {
		t.Fatal("events CLI bypasses scope", err)
	}
	svc.Config.PrivateRepositories = g.PrivateRepositories
	// Limits must preserve queued work and scope, while withholding private
	// context until GitHub can verify access again. Exercise gh's real exit path.
	for _, response := range []map[string]any{
		{"_http_status": 403, "_http_message": "API rate limit exceeded for 192.0.2.1"},
		{"_http_status": 403, "_body": map[string]any{"message": "You have exceeded a secondary rate limit."}},
		{"_http_status": 429},
	} {
		data["repos/example/fixture"] = response
		writeFixture(t, fixture, data)
		if err := svc.collectWatch(ctx, cfg.Watches[0]); !errors.Is(err, ErrGitHubRateLimited) {
			t.Fatal("rate limit classified as access loss", err)
		}
		if blocked, err := store.WatchBlocked(ctx, event.Key); err != nil || blocked {
			t.Fatal("rate limit persisted a revoked scope", blocked, err)
		}
		if w, err := svc.WorkContext(ctx, event.ID); !errors.Is(err, ErrGitHubRateLimited) || len(w.Snapshot) != 0 {
			t.Fatal("rate limit not surfaced or cached private context exposed", err)
		}
		if rows, err := svc.Pending(ctx); !errors.Is(err, ErrGitHubRateLimited) || rows != nil {
			t.Fatal("unavailable pending list reported as empty success", rows, err)
		}
		if rows, err := svc.Events(ctx); !errors.Is(err, ErrGitHubRateLimited) || rows != nil {
			t.Fatal("unavailable event list reported as empty success", err)
		}
		if err := svc.Deliver(ctx); !errors.Is(err, ErrGitHubRateLimited) || f.writes != 0 {
			t.Fatal("temporary delivery failure was hidden or event sent", err)
		}
		recorder := httptest.NewRecorder()
		if svc.checkHTTPAccess(recorder, httptest.NewRequest("GET", "/", nil), event.Key) || recorder.Code != 503 || !strings.Contains(recorder.Body.String(), "GITHUB_RATE_LIMITED") {
			t.Fatal("HTTP context reports a limit as authorization loss", recorder.Code, recorder.Body.String())
		}
		// Restart the persistence connection while still limited: no work was lost.
		reopened := openTestStore(t)
		queued, err := reopened.Events(ctx, true)
		if err != nil || len(queued) != 1 || queued[0].ID != event.ID {
			t.Fatal("rate limit consumed queued work", queued, err)
		}
	}
	// Real gh exit paths must revoke cached reads and pending sends for lost access.
	for _, status := range []int{401, 403, 404} {
		data["repos/example/fixture"] = map[string]any{"_http_status": status}
		writeFixture(t, fixture, data)
		if _, err := g.Collect(ctx, Watch{"example/fixture", 1}); !errors.Is(err, ErrGitHubAccessDenied) {
			t.Fatal("access loss not classified", status, err)
		}
		if _, err := svc.WorkContext(ctx, event.ID); !errors.Is(err, ErrWorkDenied) {
			t.Fatal("permission loss exposes cache", status, err)
		}
		if err := svc.Deliver(ctx); err != nil || f.writes != 0 {
			t.Fatal("permission loss permits delivery", status, err)
		}
	}
	data["repos/example/fixture"] = map[string]any{"full_name": "example/fixture", "private": true}
	writeFixture(t, fixture, data)
	if err := svc.Deliver(ctx); err != nil || f.creates != 1 {
		t.Fatal("private task not delivered after access recovery", err)
	}
	if strings.Contains(f.first, "example/fixture") || strings.Contains(f.first, "confidential") || !strings.Contains(f.first, "私有事项") {
		t.Fatal("private metadata leaked into shared task")
	}
	if err := svc.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.Deliver(ctx); err != nil || f.creates != 1 {
		t.Fatal("unchanged private observation replayed", err)
	}
	// A later public classification must not open previously private history.
	obj(data["repos/example/fixture"])["private"] = false
	obj(data["repos/example/fixture/issues/1"])["body"] = "now public body"
	writeFixture(t, fixture, data)
	if err := svc.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err := store.Events(ctx, true)
	if err != nil || len(pending) != 1 || !strings.Contains(string(pending[0].Payload), `"data_classification":"private"`) {
		t.Fatal("private previous values lost their classification", err)
	}
	svc.Config.PrivateRepositories = nil
	if _, err := svc.WorkContext(ctx, event.ID); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("public transition exposed private history", err)
	}
}
