package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestExternalIdentityBindingAndSecretFiles(t *testing.T) {
	identity := ExternalIdentity{}
	identity.Agent.ID, identity.Agent.Name, identity.Agent.External = "sender-id", "collector", true
	identity.Server.ID = "server-id"
	identity.Credential.Capabilities = []string{"send", "read"}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" || req.URL.Path != "/internal/agent-api/context" || req.Header.Get("Authorization") != "Bearer sk_agent_local_fixture" {
			t.Error("wrong identity request")
		}
		json.NewEncoder(w).Encode(identity)
	}))
	defer api.Close()
	r := Raft{Config: Config{RaftAPI: api.URL, ExternalAgent: "sender-id", ServerID: "server-id", DeliveryTarget: "dm:@worker~agent"}, Credential: "sk_agent_local_fixture", HTTP: SafeHTTP()}
	if _, err := r.ExternalIdentity(context.Background()); err != nil {
		t.Fatal(err)
	}
	identity.Agent.External = false
	if _, err := r.ExternalIdentity(context.Background()); err == nil {
		t.Fatal("managed credential accepted")
	}
	identity.Agent.External = true
	identity.Server.ID = "wrong-server"
	if _, err := r.ExternalIdentity(context.Background()); err == nil {
		t.Fatal("other server accepted")
	}
	identity.Server.ID = "server-id"
	identity.Credential.Capabilities = []string{"read"}
	if _, err := r.ExternalIdentity(context.Background()); err == nil {
		t.Fatal("missing send scope accepted")
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("sk_agent_local_fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	// chmod establishes the insecure fixture even when the caller uses umask 077.
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateFile(path); err == nil {
		t.Fatal("public token file accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateFile(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, path+".link"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateFile(path + ".link"); err == nil {
		t.Fatal("symlink credential accepted")
	}
}

func TestExternalDeliverySurvivesLostResponseAndRestart(t *testing.T) {
	store := freshTestStore(t)
	ctx := context.Background()
	id, err := store.Observe(ctx, "example/fixture#1", []byte(`{"title":"fixture"}`))
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.Events(ctx, true)
	if err != nil || len(events) != 1 || events[0].ID != id {
		t.Fatal("event missing", err)
	}
	var first string
	attempts := 0
	state := "sent"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer sk_agent_local_fixture" {
			t.Error("wrong credential")
		}
		if req.URL.Path == "/internal/agent-api/context" {
			json.NewEncoder(w).Encode(map[string]any{"agent": map[string]any{"id": "sender-id", "name": "collector", "external": true}, "server": map[string]string{"id": "server-id"}, "credential": map[string]any{"capabilities": []string{"read", "send"}}})
			return
		}
		if req.Method != "POST" || req.URL.Path != "/internal/agent-api/send" {
			t.Error("unexpected write")
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["continueAnyway"] == true && body["sendDraft"] != true {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "--send-draft --anyway requires a saved draft"})
			return
		}
		encoded := string(canonical(body))
		if first == "" {
			first = encoded
		} else if encoded != first {
			t.Error("retry changed keyed write")
		}
		if body["idempotencyKey"] == "" || body["target"] != "dm:@worker~agent" {
			t.Error("delivery binding missing")
		}
		attempts++
		if attempts == 1 {
			w.WriteHeader(502)
			return
		} // committed remotely; response lost
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "state": state, "messageId": "11111111-1111-1111-1111-111111111111"})
	}))
	defer api.Close()
	c := Config{RaftMode: "external", RaftAPI: api.URL, ExternalAgent: "sender-id", ServerID: "server-id", DeliveryTarget: "dm:@worker~agent"}
	r := Raft{Config: c, Credential: "sk_agent_local_fixture", HTTP: SafeHTTP(), Store: store}
	if _, err := r.Send(ctx, events[0]); err == nil {
		t.Fatal("lost response considered delivered")
	}
	omitStoredDraftFlag(t, store, id)
	reopened := openTestStore(t)
	r.Store = reopened
	r.Config.configDir = "/a/new/build/location" // persisted request must win over updated formatting
	if _, err := r.Send(ctx, events[0]); err != nil {
		t.Fatal(err)
	}
	state = "held"
	if _, err := r.Send(ctx, events[0]); err == nil {
		t.Fatal("held response considered delivered")
	}
	state = "not_found"
	if _, err := r.Send(ctx, events[0]); err == nil {
		t.Fatal("unknown result considered delivered")
	}
	state = "committed"
	if _, err := r.Send(ctx, events[0]); err != nil {
		t.Fatal(err)
	}
	r.Config.DeliveryTarget = "dm:@other~agent"
	if _, err := r.Send(ctx, events[0]); err == nil {
		t.Fatal("pending delivery silently retargeted")
	}
	if attempts != 5 {
		t.Fatal("unexpected sends", attempts)
	}
	r.Config.DeliveryTarget = c.DeliveryTarget
	accepted := []byte(`{"ok":true,"state":"committed","messageId":"11111111-1111-1111-1111-111111111111"}`)
	if err := reopened.Delivery(ctx, id, accepted, true, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Send(ctx, events[0]); err != nil || attempts != 5 {
		t.Fatal("accepted event was resent after request cleanup", attempts, err)
	}
}

func omitStoredDraftFlag(t *testing.T, store Persistence, id string) {
	t.Helper()
	key := "external-delivery:" + id
	saved, err := store.Meta(t.Context(), key, "")
	var envelope map[string]any
	if err != nil || json.Unmarshal([]byte(saved), &envelope) != nil {
		t.Fatal("missing frozen request", err)
	}
	delete(envelope["request"].(map[string]any), "sendDraft")
	if err = store.PutMeta(t.Context(), key, string(canonical(envelope))); err != nil {
		t.Fatal(err)
	}
}

func TestLocalTriageKeepsDestinationAndAccessBoundary(t *testing.T) {
	store := freshTestStore(t)
	ctx := context.Background()
	id, err := store.Observe(ctx, "example/fixture#1", []byte(`{"title":"fixture"}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := localPolicy(t, "read-only")
	cfg.Watches = []Watch{{Repo: "example/fixture", Number: 1}}
	cfg.NotifyTarget = "dm:@owner~human"
	svc := Service{Config: cfg, Store: store}
	in := Triage{EventID: id, Outcome: "notified", Reason: "A relevant change was sent.", MessageID: fixtureReport, Target: "dm:@other~human"}
	if err := svc.RecordTriage(ctx, in); err == nil {
		t.Fatal("wrong destination recorded")
	}
	in.Target = cfg.NotifyTarget
	svc.Config.Watches = nil
	if err := svc.RecordTriage(ctx, in); err == nil {
		t.Fatal("revoked event recorded")
	}
	svc.Config.Watches = cfg.Watches
	svc.Config.NotifyTarget = ""
	if err := svc.RecordTriage(ctx, in); err == nil {
		t.Fatal("unconfigured notification recorded")
	}
	svc.Config.NotifyTarget = cfg.NotifyTarget
	if err := svc.RecordTriage(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordTriage(ctx, in); err != nil {
		t.Fatal("repeat of same receipt failed", err)
	}
	pending, err := svc.Pending(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatal("processed notification remains pending", err)
	}
}
