package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const deliveryOne = "12345678-1234-4234-8234-123456789abc"
const hookBody = `{"repository":{"full_name":"example/fixture","private":false},"number":1,"pull_request":{"body":"untrusted payload text"}}`

func webhookFixture(t *testing.T) (*Service, *webhookSource) {
	t.Helper()
	g := &webhookSource{failures: map[string]error{}}
	s := &Service{Config: Config{RaftMode: "external", GitHubSource: "webhook", Watches: []Watch{{"example/fixture", 1}}, PollSeconds: 120}, Store: freshTestStore(t), GH: g, WebhookSecret: bytes.Repeat([]byte("s"), 32)}
	return s, g
}

type webhookSource struct {
	identityCalls int
	identityError error
	calls         []string
	failures      map[string]error
}

func (g *webhookSource) CheckIdentity(context.Context) error {
	g.identityCalls++
	return g.identityError
}
func (g *webhookSource) Collect(_ context.Context, w Watch) ([]byte, error) {
	g.calls = append(g.calls, w.Key())
	return canonical(Snapshot{Key: w.Key(), Classification: "public", Objects: Objects{"issue": {"state": "open", "body": "fresh API text"}}}), g.failures[w.Key()]
}

func signedWebhook(s *Service, id, event, body string) *http.Request {
	r := httptest.NewRequest("POST", "/webhooks/github", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-GitHub-Delivery", id)
	r.Header.Set("X-GitHub-Event", event)
	mac := hmac.New(sha256.New, s.WebhookSecret)
	mac.Write([]byte(body))
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return r
}

func sendWebhook(t *testing.T, s *Service, id, event, body string, want int) {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, signedWebhook(s, id, event, body))
	if w.Code != want {
		t.Fatalf("webhook response: %d %s; want %d", w.Code, w.Body.String(), want)
	}
}

func dueWebhooks(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), "UPDATE ghraft_webhooks SET next_attempt = 0 WHERE finished_at = 0"); err != nil {
		t.Fatal(err)
	}
}

func TestWebhookAcknowledgesOnlyDurableAuthenticatedDelivery(t *testing.T) {
	s, g := webhookFixture(t)
	for _, path := range []string{"/api/actions/list-events", "/oauth/callback", "/"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatal("external webhook listener exposed an App route", path, w.Code)
		}
	}
	for _, tc := range []struct {
		name string
		body string
		edit func(*http.Request)
		want int
	}{
		{"bad signature", hookBody, func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", "sha256=00") }, 401},
		{"body changed after signing", hookBody, func(r *http.Request) { r.Body = http.NoBody }, 401},
		{"missing delivery", hookBody, func(r *http.Request) { r.Header.Del("X-GitHub-Delivery") }, 400},
		{"wrong content type", hookBody, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"invalid JSON", "{", nil, 400},
		{"null JSON", " \nnull\t", nil, 400},
		{"too large", strings.Repeat("x", maxWebhookBody+1), nil, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := signedWebhook(s, deliveryOne, "pull_request", tc.body)
			if tc.edit != nil {
				tc.edit(r)
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d, want %d", w.Code, tc.want)
			}
		})
	}
	var count int64
	if err := s.Store.(*Store).db.Session(t.Context()).Model(&webhookRecord{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("rejected input was persisted", count, err)
	}
	sendWebhook(t, s, deliveryOne, "ping", `{}`, 200)
	sendWebhook(t, s, deliveryOne, "pull_request", hookBody, 202)
	sendWebhook(t, s, deliveryOne, "pull_request", hookBody, 202)
	sendWebhook(t, s, deliveryOne, "issues", hookBody, 409)
	sendWebhook(t, s, deliveryOne, "pull_request", hookBody+" ", 409)
	if g.identityCalls != 0 || len(g.calls) != 0 {
		t.Fatal("HTTP acknowledgement performed GitHub reads")
	}
	deliveries, err := s.Store.PendingWebhooks(t.Context(), time.Now().Add(time.Minute).Unix())
	if err != nil || len(deliveries) != 1 || !reflect.DeepEqual(deliveries[0].Keys, []string{"example/fixture#1"}) || strings.Contains(string(canonical(deliveries)), "untrusted") {
		t.Fatal("missing durable routing record or persisted payload", deliveries, err)
	}
	s.Store = &failedWebhookStore{Persistence: s.Store}
	sendWebhook(t, s, "22345678-1234-4234-8234-123456789abc", "pull_request", hookBody, 503)
	s.Config.GitHubSource = "polling"
	sendWebhook(t, s, deliveryOne, "pull_request", hookBody, 404)
}

type failedWebhookStore struct{ Persistence }

func (*failedWebhookStore) AcceptWebhook(context.Context, WebhookDelivery) (bool, error) {
	return false, errors.New("storage unavailable")
}

func TestWebhookRestartCoalescesAndReadsFreshStateWithoutIdlePolling(t *testing.T) {
	s, g := webhookFixture(t)
	for i := 1; i <= 3; i++ {
		sendWebhook(t, s, fmt.Sprintf("%08d-1234-4234-8234-123456789abc", i), "pull_request", hookBody, 202)
	}
	// A new service and connection have no memory of the HTTP wake signal.
	s = &Service{Config: s.Config, Store: openTestStore(t), GH: g}
	dueWebhooks(t, s.Store.(*Store))
	if active, err := s.collectScheduled(t.Context()); err != nil || !active {
		t.Fatal(active, err)
	}
	if g.identityCalls != 1 || len(g.calls) != 1 {
		t.Fatal("deliveries for one resource were not coalesced", g)
	}
	body, _, err := s.Store.Snapshot(t.Context(), "example/fixture#1")
	if err != nil || !strings.Contains(string(body), "fresh API text") || strings.Contains(string(body), "untrusted") {
		t.Fatal("payload was used as evidence instead of fresh source state", err)
	}
	for range 3 {
		if active, err := s.collectScheduled(t.Context()); err != nil || active {
			t.Fatal("idle webhook mode did work", active, err)
		}
	}
	if g.identityCalls != 1 || len(g.calls) != 1 {
		t.Fatal("webhook mode polled GitHub without an event")
	}
}

func TestWebhookRetryPreservesPartialSuccessAndRateLimitAcrossRestart(t *testing.T) {
	s, g := webhookFixture(t)
	w2 := Watch{"example/fixture", 2}
	s.Config.Watches = append(s.Config.Watches, w2)
	g.failures[w2.Key()] = ErrGitHubRateLimited
	sendWebhook(t, s, deliveryOne, "repository", hookBody, 202)
	dueWebhooks(t, s.Store.(*Store))
	if active, err := s.ProcessWebhooks(t.Context()); !active || !errors.Is(err, ErrGitHubRateLimited) {
		t.Fatal(active, err)
	}
	queued, err := s.Store.PendingWebhooks(t.Context(), time.Now().Add(time.Hour).Unix())
	if err != nil || len(queued) != 1 || !reflect.DeepEqual(queued[0].Keys, []string{w2.Key()}) || queued[0].Attempts != 1 {
		t.Fatal("successful resource was not removed from retry", queued, err)
	}
	// Re-delivery must not reset the partial progress or retry counter.
	sendWebhook(t, s, deliveryOne, "repository", hookBody, 202)
	sendWebhook(t, s, "22345678-1234-4234-8234-123456789abc", "pull_request", hookBody, 202)
	s = &Service{Config: s.Config, Store: openTestStore(t), GH: g}
	dueWebhooks(t, s.Store.(*Store))
	if active, err := s.ProcessWebhooks(t.Context()); active || !errors.Is(err, ErrGitHubRateLimited) || g.identityCalls != 1 || len(g.calls) != 2 {
		t.Fatal("new events or restart bypassed the persisted cooldown", active, err, g)
	}
	if err = s.Store.PutMeta(t.Context(), "webhook-rate-until", "0"); err != nil {
		t.Fatal(err)
	}
	delete(g.failures, w2.Key())
	// Drop the first watch while its newly accepted event is still queued.
	s.Config.Watches = []Watch{w2}
	if _, err = s.ProcessWebhooks(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(g.calls) != 3 || g.calls[2] != w2.Key() {
		t.Fatal("removed scope was queried or successful work repeated", g.calls)
	}
	queued, err = s.Store.PendingWebhooks(t.Context(), time.Now().Add(time.Hour).Unix())
	if err != nil || len(queued) != 0 {
		t.Fatal("successful retry remained pending", queued, err)
	}
}

func TestWebhookRoutesOnlyConfiguredResources(t *testing.T) {
	s, _ := webhookFixture(t)
	ctx := t.Context()
	w := s.Config.Watches[0]
	head := strings.Repeat("a", 40)
	if _, err := s.Store.Observe(ctx, w.Key(), canonical(Snapshot{Key: w.Key(), Classification: "public", Objects: Objects{"pull": {"head": head, "base": "main", "head_ref": "feature", "head_repo": "another/fork"}}})); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		event, extra string
		want         bool
	}{
		{"pull_request", `"number":1`, true},
		{"issue_comment", `"issue":{"number":1}`, true},
		{"pull_request_review", `"pull_request":{"number":1}`, true},
		{"pull_request", `"number":2`, false},
		{"check_run", `"check_run":{"head_sha":"` + head + `"}`, true},
		// GitHub can omit PR associations. A new head's CI event can arrive
		// while the push-triggered refresh still holds the old snapshot.
		{"check_run", `"check_run":{"head_sha":"` + strings.Repeat("b", 40) + `"}`, true},
		{"check_run", `"check_run":{"head_sha":"` + head + `","pull_requests":[{"number":2}]}`, false},
		{"workflow_run", `"workflow_run":{"pull_requests":[{"number":1}]}`, true},
		{"status", `"sha":"` + head + `"`, true},
		{"push", `"ref":"refs/heads/main"`, true},
		{"push", `"ref":"refs/heads/feature"`, false},
		{"push", `"ref":"refs/tags/main"`, false},
		{"release", `"number":1`, false},
	} {
		var p webhookPayload
		if err := json.Unmarshal([]byte(`{"repository":{"full_name":"example/fixture","private":false},`+tc.extra+`}`), &p); err != nil {
			t.Fatal(err)
		}
		keys, err := s.webhookTargets(ctx, tc.event, p)
		if err != nil || (len(keys) == 1) != tc.want {
			t.Fatalf("%s %s: keys=%v err=%v", tc.event, tc.extra, keys, err)
		}
		isPrivate := true
		p.Repository.Private = &isPrivate
		keys, err = s.webhookTargets(ctx, tc.event, p)
		if err != nil || len(keys) != 0 {
			t.Fatal("private repository outside scope was queued", keys, err)
		}
	}
}

func TestWebhookCanRefreshClosedWorkWithoutResumingPolling(t *testing.T) {
	s, fixture, requests := terminalPollFixture(t, true)
	s.Config.GitHubSource, s.Config.RaftMode = "webhook", "external"
	s.WebhookSecret = bytes.Repeat([]byte("s"), 32)
	if err := s.collectWatch(t.Context(), s.Config.Watches[0]); err != nil {
		t.Fatal(err)
	}
	before := requests()
	data := readFixture(t, fixture)
	obj(data["repos/example/fixture/issues/1"])["state"] = "open"
	obj(data["repos/example/fixture/pulls/1"])["state"] = "open"
	writeFixture(t, fixture, data)
	if _, err := s.collectScheduled(t.Context()); err != nil || requests() != before {
		t.Fatal("webhook mode polled closed work", err)
	}
	sendWebhook(t, s, deliveryOne, "pull_request", hookBody, 202)
	dueWebhooks(t, s.Store.(*Store))
	if _, err := s.collectScheduled(t.Context()); err != nil || requests() <= before {
		t.Fatal("reopen delivery did not refresh closed work", err)
	}
	after := requests()
	if _, err := s.collectScheduled(t.Context()); err != nil || requests() != after {
		t.Fatal("webhook refresh enabled background polling", err)
	}
}

func TestWebhookLocalValidationRecoveryPreservesSourceAge(t *testing.T) {
	s, _, _ := forkFixture(t)
	s.Config.GitHubSource = "webhook"
	p := abandonedValidation(t, s, "running")
	key := s.Config.Forks[0].Key()
	old := time.Now().Add(-time.Hour).Unix()
	if _, err := s.Store.(*Store).db.ExecContext(t.Context(), "UPDATE ghraft_snapshots SET observed_at = ? WHERE resource_key = ?", old, key); err != nil {
		t.Fatal(err)
	}
	g := &webhookSource{}
	s.GH, s.Store = g, openTestStore(t)
	if active, err := s.collectScheduled(t.Context()); err != nil || !active {
		t.Fatal(active, err)
	}
	next, err := s.Store.ForkProgress(t.Context(), key)
	observed, ageErr := s.Store.ObservedAt(t.Context(), key)
	if err != nil || ageErr != nil || observed != old || next.Wake != p.Wake+1 || next.ValidatedHead != p.ValidatedHead || next.LastSuccess != p.LastSuccess || next.Validation.State != "interrupted" {
		t.Fatal("local deadline changed source evidence or lost recovery", next, observed, err, ageErr)
	}
	s.Store = openTestStore(t)
	if active, err := s.collectScheduled(t.Context()); err != nil || active || g.identityCalls != 0 || len(g.calls) != 0 {
		t.Fatal("restart repeated recovery or queried GitHub", active, err)
	}
}

func TestWebhookForkRoutingIncludesUpstreamAndCurrentSyncPR(t *testing.T) {
	s, _, _ := forkFixture(t)
	f := s.Config.Forks[0]
	p, err := s.Store.ForkProgress(t.Context(), f.Key())
	if err != nil {
		t.Fatal(err)
	}
	p.PR = 25
	if err = s.Store.SaveForkProgress(t.Context(), f.Key(), p.Revision, p, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		repo, event, extra string
		want               bool
	}{
		{f.Upstream.Repo, "push", `"ref":"refs/heads/` + f.Upstream.Branch + `"`, true},
		{f.Upstream.Repo, "push", `"ref":"refs/heads/unrelated"`, false},
		{f.Target.Repo, "push", `"ref":"refs/heads/` + p.Branch + `"`, true},
		{f.Target.Repo, "issue_comment", `"issue":{"number":25}`, true},
		{f.Target.Repo, "issue_comment", `"issue":{"number":26}`, false},
		{f.Target.Repo, "pull_request", `"pull_request":{"number":26,"head":{"ref":"` + p.Branch + `","repo":{"full_name":"` + f.Target.Repo + `"}}}`, true},
		{f.Target.Repo, "pull_request", `"pull_request":{"number":26,"head":{"ref":"` + p.Branch + `","repo":{"full_name":"another/fork"}}}`, false},
		{f.Target.Repo, "check_suite", `"check_suite":{"pull_requests":[{"number":25}]}`, true},
		{f.Target.Repo, "check_run", `"check_run":{"head_sha":"` + strings.Repeat("a", 40) + `"}`, true},
	} {
		var payload webhookPayload
		if err := json.Unmarshal([]byte(`{"repository":{"full_name":"`+tc.repo+`","private":false},`+tc.extra+`}`), &payload); err != nil {
			t.Fatal(err)
		}
		keys, err := s.webhookTargets(t.Context(), tc.event, payload)
		if err != nil || (len(keys) == 1 && keys[0] == f.Key()) != tc.want {
			t.Fatalf("%s %s %s: keys=%v err=%v", tc.repo, tc.event, tc.extra, keys, err)
		}
	}
}

func TestWebhookConcurrentAcceptAndCleanupRetainPending(t *testing.T) {
	s := freshTestStore(t)
	other := openTestStore(t)
	d := WebhookDelivery{ID: deliveryOne, Hash: hash([]byte("body")), Event: "push", Keys: []string{"example/fixture#1"}}
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, store := range []*Store{s, other} {
		wg.Go(func() {
			accepted, err := store.AcceptWebhook(t.Context(), d)
			results <- accepted
			errs <- err
		})
	}
	wg.Wait()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if a, b := <-results, <-results; a == b {
		t.Fatal("concurrent delivery was lost or enqueued twice", a, b)
	}
	d.ID = "22345678-1234-4234-8234-123456789abc"
	if _, err := s.AcceptWebhook(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishWebhook(t.Context(), d, nil, "", 0); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).Unix()
	if _, err := s.db.ExecContext(t.Context(), "UPDATE ghraft_webhooks SET received_at = ?, finished_at = CASE WHEN finished_at > 0 THEN ? ELSE 0 END", old, old); err != nil {
		t.Fatal(err)
	}
	preview, err := s.Cleanup(t.Context(), 1, true)
	if err != nil || preview.WebhookDeliveries != 1 {
		t.Fatal("cleanup preview included unfinished delivery", preview, err)
	}
	if _, err = s.Cleanup(t.Context(), 1, false); err != nil {
		t.Fatal(err)
	}
	queued, err := s.PendingWebhooks(t.Context(), time.Now().Add(time.Minute).Unix())
	if err != nil || len(queued) != 1 || queued[0].ID != deliveryOne {
		t.Fatal("cleanup lost unfinished delivery", queued, err)
	}
}
