package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Requires an isolated disposable database. This suite uses real MySQL/seekdb SQL,
// while only the Raft API is a protocol fixture; it is NOT a live Raft acceptance test.
func TestDatabaseAndRaftProtocol(t *testing.T) {
	dsn := []byte(disposableTestDSN(t))
	ctx := context.Background()
	store, e := openTestStore(t, ctx, strings.TrimSpace(string(dsn)), bytes.Repeat([]byte{1}, 32))
	if e != nil {
		t.Fatal(e)
	}
	defer store.db.Close()
	version, e := store.db.Version(ctx)
	if e != nil {
		t.Fatal(e)
	}
	t.Log("database:", version)
	if e = store.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	for _, table := range []string{"ghraft_watch_access", "ghraft_work_links", "ghraft_triage", "ghraft_legacy_history", "ghraft_meta", "ghraft_watches", "ghraft_results", "ghraft_events", "ghraft_snapshots", "ghraft_private"} {
		if _, e = store.db.Exec("DELETE FROM " + table); e != nil {
			t.Fatal(e)
		}
	}
	key := "project-hami/hami#3259"
	id, e := store.Observe(ctx, key, []byte(`{"title":"A"}`))
	if e != nil || id == "" {
		t.Fatalf("observe %q %v", id, e)
	}
	if again, e := store.Observe(ctx, key, []byte(`{"title":"A"}`)); e != nil || again != "" {
		t.Fatalf("duplicate emitted: %s %v", again, e)
	}
	idB, e := store.Observe(ctx, key, []byte(`{"title":"B"}`))
	if e != nil {
		t.Fatal(e)
	}
	idA, e := store.Observe(ctx, key, []byte(`{"title":"A"}`))
	if e != nil || idA == id || idB == id {
		t.Fatal("A->B->A transition lost")
	}
	// Reopen to demonstrate persistent outbox across process/store lifetimes.
	reopened, e := openTestStore(t, ctx, strings.TrimSpace(string(dsn)), bytes.Repeat([]byte{1}, 32))
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.db.Close()
	events, e := reopened.Events(ctx, true)
	if e != nil || len(events) != 3 {
		t.Fatalf("outbox persistence %d %v", len(events), e)
	}
	var mu sync.Mutex
	revoked := false
	failEvent := true
	seen := map[string]bool{}
	requests := 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/oauth/token":
			user, pass, valid := r.BasicAuth()
			if !valid || user != "app" || pass != "secret" {
				t.Error("token exchange lacks Basic auth")
				w.WriteHeader(401)
				return
			}
			var b map[string]string
			json.NewDecoder(r.Body).Decode(&b)
			if b["grant_type"] == "authorization_code" {
				json.NewEncoder(w).Encode(tokenResponse{AccessToken: "identity-token", Expires: 3600})
				return
			}
			if b["resource"] != "urn:raft:server:server:agent-inbound" {
				t.Error("wrong resource binding")
			}
			json.NewEncoder(w).Encode(tokenResponse{AccessToken: "inbound-token", Expires: 3600, Resource: b["resource"], Scope: "agent:event:write"})
		case "/api/oauth/userinfo":
			if revoked || r.Header.Get("Authorization") != "Bearer identity-token" {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(Identity{Subject: "agent", Type: "agent", ServerID: "server", ClientID: "app"})
		case "/api/oauth/requests/agent":
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			if b["agentId"] != "agent" {
				t.Error("wrong target agent")
			}
			json.NewEncoder(w).Encode(map[string]any{"status": "approved", "requestId": "one-time", "agent": map[string]string{"id": "agent", "serverId": "server"}})
		case "/api/oauth/agent-events":
			if r.Header.Get("Authorization") != "Bearer inbound-token" {
				t.Error("wrong inbound token")
			}
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			eid := b["externalEventId"].(string)
			requests++
			if seen[eid] {
				json.NewEncoder(w).Encode(map[string]any{"id": eid, "deduped": true})
				return
			}
			seen[eid] = true
			// Server accepted but response is lost: retry must use the identical key.
			if failEvent {
				failEvent = false
				w.WriteHeader(502)
				return
			}
			w.WriteHeader(202)
			json.NewEncoder(w).Encode(map[string]any{"id": eid, "status": "queued"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer fake.Close()
	cfg := Config{ClientID: "app", ServerID: "server", ServerSlug: "slug", AgentID: "agent", Origin: "http://127.0.0.1", RaftAPI: fake.URL, Watches: []Watch{{"Project-HAMi/HAMi", 3259}}}
	raft := &Raft{Config: cfg, Secret: "secret", HTTP: SafeHTTP(), Store: store}
	if e = store.Delivery(ctx, events[0].ID, nil, false, 0); e != nil {
		t.Fatal(e)
	}
	var retryAt int64
	if e = store.db.QueryRow(`SELECT next_attempt FROM ghraft_events WHERE event_id=?`, events[0].ID).Scan(&retryAt); e != nil || retryAt <= time.Now().Unix() {
		t.Fatal("expected retry backoff before authorization", e)
	}
	if e = raft.AuthorizeEvents(ctx); e != nil {
		t.Fatal(e)
	}
	if e = store.db.QueryRow(`SELECT next_attempt FROM ghraft_events WHERE event_id=?`, events[0].ID).Scan(&retryAt); e != nil || retryAt > time.Now().Unix() {
		t.Fatal("successful authorization did not unblock pending event", e)
	}
	// Token ciphertext must not contain the original; key mismatch cannot read it.
	var encrypted []byte
	store.db.QueryRow(`SELECT ciphertext FROM ghraft_private WHERE secret_key=?`, raft.inboundKey()).Scan(&encrypted)
	if bytes.Contains(encrypted, []byte("inbound-token")) {
		t.Fatal("token stored plaintext")
	}
	receipt, e := raft.Send(ctx, events[0])
	if e == nil {
		t.Fatal("ambiguous failed response accepted")
	}
	receipt, e = raft.Send(ctx, events[0])
	if e != nil || !bytes.Contains(receipt, []byte("deduped")) {
		t.Fatalf("idempotent retry failed %s %v", receipt, e)
	}
	if e = store.Delivery(ctx, events[0].ID, receipt, true, 0); e != nil {
		t.Fatal(e)
	}
	svc := Service{Config: cfg, Store: store, Raft: raft}
	server := httptest.NewServer(svc.Handler())
	defer server.Close()
	res, e := http.Get(server.URL + "/agent/callback?code=test-code")
	if e != nil {
		t.Fatal(e)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || bytes.Contains(body, []byte("identity-token")) {
		t.Fatalf("callback %d %s", res.StatusCode, body)
	}
	cookies := res.Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].Path != "/" {
		t.Fatal("bad session cookie")
	}
	call := func(path, body string, cookie bool) (int, []byte) {
		req, _ := http.NewRequest("POST", server.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cookie {
			req.AddCookie(cookies[0])
		}
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	if status, _ := call("/api/actions/get-context", `{"resource_key":"project-hami/hami#3259"}`, false); status != 401 {
		t.Fatal("unauthenticated action accepted")
	}
	if status, b := call("/api/actions/get-context", `{"resource_key":"project-hami/hami#3259"}`, true); status != 200 || !bytes.Contains(b, []byte(`"title":"A"`)) {
		t.Fatalf("context failed %d %s", status, b)
	}
	if status, _ := call("/api/actions/get-context", `{"resource_key":"private/repo#1"}`, true); status != 403 {
		t.Fatal("unconfigured repo allowed")
	}
	result := map[string]string{"event_id": events[0].ID, "raft_task_message_id": "11111111-1111-1111-1111-111111111111", "raft_report_message_id": "22222222-2222-2222-2222-222222222222", "evidence_version": events[0].Version, "summary": "fixture result"}
	raw, _ := json.Marshal(result)
	for i := 0; i < 2; i++ {
		if status, b := call("/api/actions/record-result", string(raw), true); status != 200 {
			t.Fatalf("result idempotency %d %s", status, b)
		}
	}
	result["summary"] = "changed"
	raw, _ = json.Marshal(result)
	if status, _ := call("/api/actions/record-result", string(raw), true); status != 409 {
		t.Fatal("conflicting result id reused")
	}
	mu.Lock()
	revoked = true
	mu.Unlock()
	if status, _ := call("/api/actions/list-events", `{}`, true); status != 401 {
		t.Fatal("revoked session remained usable")
	}
	// Expiry enforced even when ciphertext and cookie are intact.
	if e = store.SaveSecret(ctx, "expired", []byte("token"), time.Now().Unix()-1); e != nil {
		t.Fatal(e)
	}
	if _, e = store.Secret(ctx, "expired"); e == nil {
		t.Fatal("expired secret accepted")
	}
	mu.Lock()
	if requests != 2 || len(seen) != 1 {
		t.Error("retry did not preserve event identity")
	}
	mu.Unlock()
}
