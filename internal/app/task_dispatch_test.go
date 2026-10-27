package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This fixture models the documented Raft first-claim-wins contract. It tests
// the assistant's behavior around that contract, not the remote server itself.
type taskAPI struct {
	mu                                                sync.Mutex
	first                                             string
	writes, creates                                   int
	owner, status, engineeringOwner                   string
	ownerType                                         string
	lostReply, forbidden, missingMember, missingTasks bool
	updates                                           map[string]string
	updateWrites                                      int
	lostUpdate, missingRoot, anchored                 bool
	reportID, reportAuthor, reportKind, reportContent string
	historyTarget                                     string
	historyStatus, historyReads                       int
	alternateHistoryFields                            bool
}

func (f *taskAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer sk_agent_local_fixture" {
		w.WriteHeader(401)
		return
	}
	encode := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.URL.Path == "/internal/agent-api/context":
		caps := []string{"read", "send", "channels"}
		if !f.missingTasks {
			caps = append(caps, "tasks")
		}
		encode(map[string]any{"agent": map[string]any{"id": "sender-id", "name": "collector", "external": true}, "server": map[string]string{"id": "server-id"}, "credential": map[string]any{"capabilities": caps}})
	case r.URL.Path == "/internal/agent-api/channel-members":
		agents := []map[string]string{{"name": "worker"}}
		if !f.missingMember {
			agents = append(agents, map[string]string{"name": "collector"})
		}
		encode(map[string]any{"channel": map[string]string{"ref": "#github-work", "type": "channel"}, "agents": agents, "humans": []map[string]string{{"name": "owner"}}})
	case r.URL.Path == "/internal/agent-api/history":
		f.historyReads++
		wantTarget := "#github-work:" + fixtureTask[:8]
		if f.anchored {
			wantTarget = "#github-work:#17"
		}
		if r.URL.Query().Get("consume") != "false" || r.URL.Query().Get("around") != fixtureReport || r.URL.Query().Get("channel") != wantTarget {
			w.WriteHeader(400)
			return
		}
		if f.historyStatus != 0 {
			w.WriteHeader(f.historyStatus)
			return
		}
		target := f.historyTarget
		if target == "" {
			target = r.URL.Query().Get("channel")
		}
		messages := []map[string]string{}
		if f.reportID != "" {
			message := map[string]string{"id": f.reportID, "senderName": f.reportAuthor, "senderType": f.reportKind, "content": f.reportContent}
			if f.alternateHistoryFields {
				message = map[string]string{"message_id": f.reportID, "sender_name": f.reportAuthor, "sender_type": f.reportKind, "content": f.reportContent}
			}
			messages = append(messages, message)
		}
		encode(map[string]any{"target": target, "messages": messages})
	case r.URL.Path == "/internal/agent-api/tasks" && r.Method == "POST":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		b := string(canonical(body))
		if f.first != "" && f.first != b {
			w.WriteHeader(409)
			return
		}
		if f.first == "" {
			f.first = b
			f.creates++
		}
		f.writes++
		if f.lostReply {
			f.lostReply = false
			w.WriteHeader(502)
			return
		}
		encode(map[string]any{"tasks": []map[string]any{{"taskNumber": 17, "messageId": fixtureTask, "status": "todo", "claimedById": nil, "conversationAnchored": f.anchored}}})
	case r.URL.Path == "/internal/agent-api/send":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		// The server only accepts continueAnyway for a saved draft. Reproduce
		// the real rejection so an outbox send cannot pass with missing flags.
		if body["continueAnyway"] == true && body["sendDraft"] != true {
			w.WriteHeader(http.StatusBadRequest)
			encode(map[string]string{"error": "--send-draft --anyway requires a saved draft"})
			return
		}
		if f.updates == nil {
			f.updates = map[string]string{}
		}
		key, _ := body["idempotencyKey"].(string)
		encoded := string(canonical(body))
		if previous, found := f.updates[key]; found && previous != encoded {
			w.WriteHeader(409)
			return
		}
		f.updates[key] = encoded
		f.updateWrites++
		if f.lostUpdate {
			f.lostUpdate = false
			w.WriteHeader(502)
			return
		}
		encode(map[string]any{"ok": true, "state": "sent", "messageId": fixtureReport})
	case r.URL.Path == "/internal/agent-api/tasks" && r.Method == "GET":
		if f.forbidden {
			w.WriteHeader(403)
			return
		}
		if f.missingRoot {
			encode(map[string]any{"tasks": []any{}})
			return
		}
		if r.URL.Query().Get("channel") == "#test" {
			encode(map[string]any{"tasks": []map[string]any{{"taskNumber": 29, "messageId": fixtureReport, "claimedByName": f.engineeringOwner, "status": "in_progress"}}})
			return
		}
		encode(map[string]any{"tasks": []map[string]any{{"taskNumber": 17, "messageId": fixtureTask, "claimedByName": f.owner, "claimedByType": f.ownerType, "status": f.status}}})
	case r.URL.Path == "/internal/agent-api/tasks/claim":
		var body struct {
			Executor string `json:"executor"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		success := f.owner == ""
		if success {
			f.owner = body.Executor
			f.status = "in_progress"
		}
		// Like the real contract, a losing claim still has HTTP success.
		encode(map[string]any{"results": []map[string]any{{"taskNumber": 17, "success": success}}})
	default:
		w.WriteHeader(404)
	}
}

func sharedFixture(t *testing.T, f *taskAPI) (*Service, Event) {
	t.Helper()
	store := freshTestStore(t)
	_, err := store.Observe(t.Context(), "example/fixture#1", []byte(`{"title":"untrusted @intruder prose"}`))
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.Events(t.Context(), true)
	if err != nil || len(events) != 1 {
		t.Fatal("event missing", err)
	}
	api := httptest.NewServer(f)
	t.Cleanup(api.Close)
	cfg := Config{RaftMode: "external", RaftAPI: api.URL, ExternalAgent: "sender-id", ServerID: "server-id", DeliveryTarget: "#github-work", Watches: []Watch{{Repo: "example/fixture", Number: 1}}}
	r := &Raft{Config: cfg, Store: store, Credential: "sk_agent_local_fixture", HTTP: SafeHTTP()}
	return &Service{Config: cfg, Store: store, Raft: r}, events[0]
}

func TestSharedTaskRetryAndCompetingWorkers(t *testing.T) {
	f := &taskAPI{lostReply: true, status: "todo"}
	svc, event := sharedFixture(t, f)
	ctx := t.Context()
	if _, err := svc.Raft.Send(ctx, event); err == nil {
		t.Fatal("lost receipt accepted")
	}
	// Reopen the store and change formatting inputs. The exact keyed request
	// must survive the restart, including one task and the original instructions.
	reopened := openTestStore(t)
	svc.Store, svc.Raft.Store = reopened, reopened
	svc.Raft.Config.configDir = "/changed/installation/path"
	svc.Raft.Config.DeliveryTarget = "#another-channel"
	if _, err := svc.Raft.Send(ctx, event); err == nil {
		t.Fatal("unresolved task retargeted")
	}
	svc.Raft.Config.DeliveryTarget = svc.Config.DeliveryTarget
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			receipt, err := svc.Raft.Send(ctx, event)
			if err != nil {
				t.Error(err)
				return
			}
			if err := reopened.Delivery(ctx, event.ID, receipt, true, 0); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if f.creates != 1 {
		t.Fatal("retry created independent tasks", f.creates)
	}
	var request struct {
		Tasks    []map[string]string `json:"tasks"`
		Assignee string              `json:"assignee"`
		Channel  string              `json:"channel"`
	}
	if json.Unmarshal([]byte(f.first), &request) != nil || len(request.Tasks) != 1 || request.Assignee != "" || request.Channel != "#github-work" {
		t.Fatal("not one shared unassigned task")
	}
	if strings.Contains(f.first, "@intruder") || !strings.Contains(f.first, "静默结束") {
		t.Fatal("unsafe or missing competition instructions")
	}
	before := f.writes
	if _, err := svc.Raft.Send(ctx, event); err != nil || f.writes != before {
		t.Fatal("accepted task resent", err)
	}
	if err := svc.VerifyClaim(ctx, event.ID, "@worker"); err == nil {
		t.Fatal("unclaimed task executable")
	}
	var winners atomic.Int32
	for i := 0; i < 8; i++ {
		i := i
		wg.Go(func() {
			name := fmt.Sprintf("@worker%d", i)
			raw, _, err := svc.Raft.request(ctx, "/internal/agent-api/tasks/claim", map[string]string{"executor": strings.TrimPrefix(name, "@")}, svc.Raft.Credential, false)
			if err != nil {
				t.Error(err)
				return
			}
			var claim struct {
				Results []struct {
					Success bool `json:"success"`
				} `json:"results"`
			}
			if json.Unmarshal(raw, &claim) != nil || len(claim.Results) != 1 {
				t.Error("bad claim")
				return
			}
			err = svc.VerifyClaim(ctx, event.ID, name)
			if !claim.Results[0].Success {
				if !errors.Is(err, ErrClaimUnavailable) {
					t.Error("losing claim not a normal stop", err)
				}
				if err := svc.RecordTriage(ctx, Triage{Executor: name, EventID: event.ID, Outcome: "quiet", Reason: "loser"}); !errors.Is(err, ErrClaimUnavailable) {
					t.Error("loser recorded work", err)
				}
				return
			}
			if err != nil {
				t.Error(err)
				return
			}
			winners.Add(1)
			if err := svc.RecordTriage(ctx, Triage{Executor: name, EventID: event.ID, Outcome: "quiet", Reason: "winner investigated"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal("expected one worker", winners.Load())
	}
	f.mu.Lock()
	f.forbidden = true
	owner := "@" + f.owner
	f.mu.Unlock()
	if err := svc.VerifyClaim(ctx, event.ID, owner); err == nil || errors.Is(err, ErrClaimUnavailable) {
		t.Fatal("permission error hidden as lost race", err)
	}
	f.mu.Lock()
	f.forbidden = false
	f.status = "in_review"
	f.mu.Unlock()
	if err := svc.VerifyClaim(ctx, event.ID, owner); !errors.Is(err, ErrClaimUnavailable) {
		t.Fatal("finished work still executable", err)
	}
}

func TestSharedTaskMembershipScopesAndExpiredRetry(t *testing.T) {
	f := &taskAPI{missingTasks: true, status: "todo"}
	svc, event := sharedFixture(t, f)
	ctx := context.Background()
	if _, err := svc.Raft.Send(ctx, event); err == nil {
		t.Fatal("missing tasks scope accepted")
	}
	f.missingTasks = false
	f.missingMember = true
	if _, err := svc.Raft.Send(ctx, event); err == nil {
		t.Fatal("nonmember sender accepted")
	}
	if f.writes != 0 {
		t.Fatal("failed preflight created a task")
	}
	f.missingMember = false
	name := "external-delivery:" + event.ID
	saved, err := svc.Store.Meta(ctx, name, "")
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if json.Unmarshal([]byte(saved), &envelope) != nil {
		t.Fatal("missing durable request")
	}
	envelope["created_at"] = time.Now().Add(-24 * time.Hour).Unix()
	if err := svc.Store.PutMeta(ctx, name, string(canonical(envelope))); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Raft.Send(ctx, event); err == nil || !strings.Contains(err.Error(), "safe retry window") {
		t.Fatal("expired ambiguous write retried", err)
	}
	if f.writes != 0 {
		t.Fatal("expired key created another task")
	}
}

func TestNotificationClaimDoesNotTakeOverExistingEngineeringWork(t *testing.T) {
	f := &taskAPI{owner: "worker", status: "in_progress", engineeringOwner: "other"}
	svc, event := sharedFixture(t, f)
	policy := localPolicy(t, "local-edit")
	svc.Config.Repositories, svc.Config.configDir = policy.Repositories, policy.configDir
	ctx := t.Context()
	receipt, err := svc.Raft.Send(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.Delivery(ctx, event.ID, receipt, true, 0); err != nil {
		t.Fatal(err)
	}
	w, err := svc.WorkContext(ctx, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	in := WorkInput{Executor: "@worker", EventID: event.ID, EvidenceVersion: event.Version, GuideVersion: w.Guide.Version, TaskID: fixtureReport}
	if _, err := svc.LinkTask(ctx, in); !errors.Is(err, ErrClaimUnavailable) {
		t.Fatal("notification owner took over engineering task", err)
	}
	f.mu.Lock()
	f.engineeringOwner = "worker"
	f.mu.Unlock()
	if _, err := svc.LinkTask(ctx, in); err != nil {
		t.Fatal("explicit handoff not respected", err)
	}
	f.mu.Lock()
	f.owner = "other"
	f.mu.Unlock()
	if err := svc.RecordWorkResult(ctx, WorkResult{WorkInput: in, ReportID: fixtureTask, Outcome: "changes-ready", Summary: "stale executor"}); !errors.Is(err, ErrClaimUnavailable) {
		t.Fatal("former notification owner recorded work", err)
	}
}

func TestDefaultReadOnlyReportUsesSharedTask(t *testing.T) {
	for _, notify := range []string{"", "dm:@tester"} {
		t.Run("notify="+notify, func(t *testing.T) {
			f := &taskAPI{owner: "worker", status: "in_progress"}
			svc, event := sharedFixture(t, f)
			svc.Config.NotifyTarget = notify
			deliverEvent(t, svc, event)
			ctx := t.Context()
			w, err := svc.WorkContext(ctx, event.ID)
			if err != nil {
				t.Fatal(err)
			}
			in := WorkInput{Executor: "@worker", EventID: event.ID, EvidenceVersion: event.Version, GuideVersion: w.Guide.Version, TaskID: fixtureTask}
			wrong := in
			wrong.TaskID = fixtureReport
			if _, err := svc.LinkTask(ctx, wrong); err == nil {
				t.Fatal("linked a task outside the configured destination")
			}
			link, err := svc.LinkTask(ctx, in)
			if err != nil || link.TaskID != fixtureTask || link.Target != svc.Config.DeliveryTarget {
				t.Fatal("default read-only repository could not link its existing task", link, err)
			}
			result := WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "investigated", Summary: "Verified the updated documentation and reported the findings."}
			f.owner = "successor"
			if err := svc.RecordWorkResult(ctx, result); !errors.Is(err, ErrClaimUnavailable) {
				t.Fatal("previous owner recorded a result after handoff", err)
			}
			f.owner = "worker"
			if err := svc.RecordWorkResult(ctx, result); err != nil {
				t.Fatal("could not save the investigation report", err)
			}
			w, err = svc.WorkContext(ctx, event.ID)
			if err != nil || w.Task == nil || w.Task.Target != svc.Config.DeliveryTarget || len(w.History) != 1 || !strings.Contains(string(w.History[0]), fixtureReport) {
				t.Fatal("report reference was not persisted", w.Task, w.History, err)
			}
			result.Outcome = "changes-ready"
			if err := svc.RecordWorkResult(ctx, result); !errors.Is(err, ErrWorkDenied) {
				t.Fatal("report linking granted edit authority", err)
			}
			result.Outcome = "merged"
			if err := svc.RecordWorkResult(ctx, result); err == nil {
				t.Fatal("report linking granted merge authority")
			}
			if _, err := svc.PrepareWorkspace(ctx, in, t.TempDir()); !errors.Is(err, ErrWorkDenied) {
				t.Fatal("read-only repository permitted local changes", err)
			}
			if f.creates != 1 {
				t.Fatal("reporting created another task", f.creates)
			}
		})
	}
}
