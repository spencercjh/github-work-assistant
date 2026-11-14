package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const (
	projectSyncTask   = "10000000-0000-4000-8000-000000000001"
	projectTestTask   = "20000000-0000-4000-8000-000000000002"
	projectReviewTask = "30000000-0000-4000-8000-000000000003"
	projectReport     = "40000000-0000-4000-8000-000000000004"
)

type projectTask struct {
	taskState
	Channel string
}

// Models live task ownership, independent thread reports and keyed writes.
// All requests go to a local test server, never to an actual Raft workspace.
type projectAPI struct {
	mu                       sync.Mutex
	tasks                    map[string]projectTask
	roots                    map[string]string
	requests                 map[string]string
	sends                    map[string]string
	reportTask, reportAuthor string
	missingChannel           string
	lostSend                 bool
	lostTarget               string
}

func newProjectAPI(t *testing.T, s *Service) *projectAPI {
	t.Helper()
	a := &projectAPI{tasks: map[string]projectTask{}, roots: map[string]string{}, requests: map[string]string{}, sends: map[string]string{}}
	a.add("#github-work", fixtureTask, 17, "worker")
	a.roots["#github-work"] = fixtureTask
	a.add("#github-work", projectSyncTask, 18, "syncer")
	a.add("#github-work", projectTestTask, 19, "tester")
	a.add("#github-work", projectReviewTask, 20, "reviewer")
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	s.Config.RaftAPI = server.URL
	s.Raft.Config = s.Config
	s.Raft.HTTP = server.Client()
	return a
}

func (a *projectAPI) add(channel, id string, number int, owner string) {
	a.tasks[id] = projectTask{taskState{Number: number, MessageID: id, Owner: owner, OwnerType: "agent", Status: "in_progress"}, channel}
}

func (a *projectAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	write := func(value any) { _ = json.NewEncoder(w).Encode(value) }
	if r.Header.Get("Authorization") != "Bearer sk_agent_local_fixture" {
		w.WriteHeader(401)
		return
	}
	switch {
	case r.URL.Path == "/internal/agent-api/context":
		write(map[string]any{"agent": map[string]any{"id": "sender-id", "name": "collector", "external": true}, "server": map[string]string{"id": "server-id"}, "credential": map[string]any{"capabilities": []string{"read", "send", "channels", "tasks"}}})
	case r.URL.Path == "/internal/agent-api/channel-members":
		channel := r.URL.Query().Get("channel")
		agents := []map[string]string{}
		if channel != a.missingChannel {
			agents = append(agents, map[string]string{"name": "collector"})
		}
		write(map[string]any{"channel": map[string]string{"ref": channel}, "agents": agents, "humans": []map[string]string{{"name": "owner"}}})
	case r.URL.Path == "/internal/agent-api/tasks" && r.Method == "GET":
		tasks := []taskState{}
		for _, task := range a.tasks {
			if task.Channel == r.URL.Query().Get("channel") {
				tasks = append(tasks, task.taskState)
			}
		}
		write(map[string]any{"tasks": tasks})
	case r.URL.Path == "/internal/agent-api/tasks" && r.Method == "POST":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		channel, _ := body["channel"].(string)
		id, ok := a.roots[channel]
		if !ok {
			w.WriteHeader(403)
			return
		}
		key, _ := body["idempotencyKey"].(string)
		encoded := string(canonical(body))
		if previous := a.requests[key]; previous != "" && previous != encoded {
			w.WriteHeader(409)
			return
		}
		a.requests[key] = encoded
		write(map[string]any{"tasks": []taskState{a.tasks[id].taskState}})
	case r.URL.Path == "/internal/agent-api/history":
		task := a.tasks[a.reportTask]
		if task.MessageID == "" {
			w.WriteHeader(404)
			return
		}
		target := (TaskDispatch{Channel: task.Channel, Number: task.Number, MessageID: task.MessageID, ConversationAnchored: task.ConversationAnchored}).target()
		if r.URL.Query().Get("consume") != "false" || r.URL.Query().Get("channel") != target {
			w.WriteHeader(400)
			return
		}
		write(map[string]any{"target": target, "messages": []map[string]string{{"id": r.URL.Query().Get("around"), "senderName": a.reportAuthor, "senderType": "agent"}}})
	case r.URL.Path == "/internal/agent-api/send":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["sendDraft"] != true || body["continueAnyway"] != true {
			w.WriteHeader(400)
			return
		}
		key, _ := body["idempotencyKey"].(string)
		encoded := string(canonical(body))
		if previous := a.sends[key]; previous != "" && previous != encoded {
			w.WriteHeader(409)
			return
		}
		a.sends[key] = encoded
		if a.lostSend && (a.lostTarget == "" || body["target"] == a.lostTarget) {
			a.lostSend = false
			w.WriteHeader(502)
			return
		}
		write(map[string]any{"ok": true, "state": "sent", "messageId": projectReport})
	default:
		w.WriteHeader(404)
	}
}

func projectFixture(t *testing.T) (*Service, *forkFixtureSource, *projectAPI, WorkInput, WorkContext, Workspace) {
	t.Helper()
	s, g, _ := forkFixture(t)
	s.Config.Forks[0].Channel = "#github-work"
	a := newProjectAPI(t, s)
	in, w := currentForkInput(t, s)
	ws, err := s.PrepareWorkspace(t.Context(), in, g.repository)
	if err != nil {
		t.Fatal(err)
	}
	return s, g, a, in, w, ws
}

func participantInput(t *testing.T, s *Service, event, task, owner string) WorkInput {
	t.Helper()
	w, err := s.WorkContext(t.Context(), event)
	if err != nil {
		t.Fatal(err)
	}
	in := WorkInput{EventID: event, TaskID: task, Executor: owner, EvidenceVersion: w.CurrentVersion, GuideVersion: w.Guide.Version}
	if _, err = s.LinkTask(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	return in
}

func TestProjectCollaborationIndependentTasksAndBoundResults(t *testing.T) {
	s, g, api, coordinator, _, ws := projectFixture(t)
	for _, item := range []struct{ id, kind string }{{projectSyncTask, "sync"}, {projectTestTask, "test"}, {projectReviewTask, "review"}} {
		if _, err := s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: coordinator, ParticipantTask: item.id, Kind: item.kind}); err != nil {
			t.Fatal(err)
		}
	}
	syncer := participantInput(t, s, coordinator.EventID, projectSyncTask, "@syncer")
	tester := participantInput(t, s, coordinator.EventID, projectTestTask, "@tester")
	reviewer := participantInput(t, s, coordinator.EventID, projectReviewTask, "@reviewer")
	if _, err := s.PrepareWorkspace(t.Context(), coordinator, g.repository); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("coordinator raced delegated writer", err)
	}
	if _, err := s.PrepareWorkspace(t.Context(), syncer, g.repository); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareWorkspace(t.Context(), tester, g.repository); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("tester received write access", err)
	}
	if _, err := s.TestWorkspace(t.Context(), reviewer); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("reviewer ran validator", err)
	}
	if _, err := s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: tester, Action: "push", Head: ws.Head, Base: g.target}); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("tester received publish access", err)
	}
	proof, err := s.TestWorkspace(t.Context(), tester)
	if err != nil || !proof.Passed {
		t.Fatal(proof, err)
	}
	api.reportTask, api.reportAuthor = projectReviewTask, "reviewer"
	report := ContributionInput{WorkInput: reviewer, ReportID: projectReport, Outcome: "approved", Summary: "Independent source and validation review.", Head: proof.Head, Base: proof.Base}
	if err := s.RecordContribution(t.Context(), report); err != nil {
		t.Fatal(err)
	}
	// A second process sees all three tasks and the exact review, without
	// replacing the synchronization or test task's own evidence binding.
	store := openTestStore(t)
	s.Store, s.Raft.Store = store, store
	w, err := s.WorkContext(t.Context(), coordinator.EventID)
	if err != nil || len(w.Collaboration.Items) != 4 || !w.Collaboration.item(projectReviewTask).Result.Current {
		t.Fatal(w.Collaboration, err)
	}
	if w.Collaboration.item(projectTestTask).Link.TaskID != projectTestTask {
		t.Fatal("participant link overwritten")
	}
	// A moved target invalidates the review even though the PR head stays put.
	tree := fixtureGit(t, g.repository, "", "rev-parse", g.target+"^{tree}")
	g.target = syntheticCommit(t, g.repository, tree, g.target)
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	w, err = s.WorkContext(t.Context(), coordinator.EventID)
	if err != nil || w.Collaboration.item(projectReviewTask).Result.Current {
		t.Fatal("old target approval survived", err)
	}
	if err = s.RecordContribution(t.Context(), report); !errors.Is(err, ErrStaleWork) {
		t.Fatal("stale result accepted", err)
	}
}

func TestProjectCollaborationLiveOwnerRevocationAndScope(t *testing.T) {
	s, g, api, coordinator, _, _ := projectFixture(t)
	if _, err := s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: coordinator, ParticipantTask: projectTestTask, Kind: "test"}); err != nil {
		t.Fatal(err)
	}
	tester := participantInput(t, s, coordinator.EventID, projectTestTask, "@tester")
	wrong := tester
	wrong.Executor = "@stranger"
	if err := s.VerifyWorkItemClaim(t.Context(), wrong); !errors.Is(err, ErrClaimUnavailable) {
		t.Fatal(err)
	}
	state := api.tasks[projectTestTask]
	state.Owner = "successor"
	api.tasks[projectTestTask] = state
	if _, err := s.TestWorkspace(t.Context(), tester); !errors.Is(err, ErrClaimUnavailable) {
		t.Fatal("former owner ran test", err)
	}
	tester.Executor = "@successor"
	if err := s.VerifyWorkItemClaim(t.Context(), tester); err != nil {
		t.Fatal(err)
	}
	root := api.tasks[fixtureTask]
	root.Status = "closed"
	api.tasks[fixtureTask] = root
	if _, err := s.TestWorkspace(t.Context(), tester); !errors.Is(err, ErrClaimUnavailable) {
		t.Fatal("project pause ignored", err)
	}
	root.Status = "in_progress"
	api.tasks[fixtureTask] = root
	if _, err := s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: coordinator, ParticipantTask: projectTestTask, Kind: "test", Revoke: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TestWorkspace(t.Context(), tester); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("revoked work still authorized", err)
	}
	state = api.tasks[projectReviewTask]
	state.Channel = "#elsewhere"
	api.tasks[projectReviewTask] = state
	if _, err := s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: coordinator, ParticipantTask: projectReviewTask, Kind: "review"}); err == nil {
		t.Fatal("cross-channel task registered")
	}
	if _, err := s.PrepareWorkspace(t.Context(), coordinator, g.repository); err != nil {
		t.Fatal("unrelated coordinator authority lost", err)
	}
}

func TestProjectRoutesForksToDifferentChannels(t *testing.T) {
	s, _, _ := forkFixture(t)
	s.Config.DeliveryTarget = ""
	s.Config.Forks[0].Channel = "#project-one"
	second := s.Config.Forks[0]
	second.ID = "second"
	second.Channel = "#project-two"
	second.Target.Branch = "next"
	s.Config.Forks = append(s.Config.Forks, second)
	api := newProjectAPI(t, s)
	api.add("#project-one", fixtureTask, 17, "worker")
	api.roots["#project-one"] = fixtureTask
	api.add("#project-two", projectSyncTask, 18, "syncer")
	api.roots["#project-two"] = projectSyncTask
	if err := s.Config.ValidateDeliveryTarget(); err != nil {
		t.Fatal(err)
	}
	if err := s.Config.validateForks(); err != nil {
		t.Fatal(err)
	}
	if err := s.Raft.CheckTaskChannel(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	events, err := s.Store.Events(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if !strings.HasPrefix(ev.Key, "fork:") {
			continue
		}
		deliverEvent(t, s, ev)
		w, err := s.WorkContext(t.Context(), ev.ID)
		if err != nil {
			t.Fatal(err)
		}
		if w.Discussion == nil || w.Discussion.Channel != s.Config.DeliveryTargetFor(ev.Key) || w.Guide.TaskTarget != w.Discussion.Channel {
			t.Fatal("wrong project route", w.Discussion, w.Guide.TaskTarget)
		}
		owner := "@worker"
		if strings.Contains(ev.Key, "second") {
			owner = "@syncer"
		}
		if err := s.VerifyClaim(t.Context(), ev.ID, owner); err != nil {
			t.Fatal(err)
		}
	}
	if len(api.requests) != 2 {
		t.Fatal("did not create one coordination task per project", len(api.requests))
	}
	api.missingChannel = "#project-two"
	if err := s.Raft.CheckTaskChannel(t.Context()); err == nil {
		t.Fatal("sender absent from project channel accepted")
	}
}

func TestProjectNotificationsFreezeRecipientsAndRetry(t *testing.T) {
	s, _, api, in, _, _ := projectFixture(t)
	anchored := api.tasks[projectTestTask]
	anchored.ConversationAnchored = true
	api.tasks[projectTestTask] = anchored
	for _, item := range []struct{ id, kind string }{{projectSyncTask, "sync"}, {projectTestTask, "test"}} {
		if _, err := s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: in, ParticipantTask: item.id, Kind: item.kind}); err != nil {
			t.Fatal(err)
		}
	}
	w, err := s.WorkContext(t.Context(), in.EventID)
	if err != nil {
		t.Fatal(err)
	}
	event, err := s.Store.Event(t.Context(), w.CurrentEventID)
	if err != nil {
		t.Fatal(err)
	}
	if event.State != "pending" || !strings.Contains(string(event.Payload), "collaboration.updated") {
		t.Fatal(event)
	}
	api.lostSend, api.lostTarget = true, "#github-work:#19"
	if _, err = s.Raft.Send(t.Context(), event); err == nil {
		t.Fatal("lost participant response reported delivered")
	}
	firstCount := len(api.sends)
	if firstCount != 3 {
		t.Fatal("expected coordinator and two work-item threads", firstCount)
	}
	state := api.tasks[projectTestTask]
	state.Owner = "successor"
	api.tasks[projectTestTask] = state
	if _, err = s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: in, ParticipantTask: projectReviewTask, Kind: "review"}); err != nil {
		t.Fatal(err)
	}
	// Separate database connection models process restart; the request must not
	// change its mention or add the newly registered task to the older event.
	store := openTestStore(t)
	s.Store, s.Raft.Store = store, store
	deliverEvent(t, s, event)
	if len(api.sends) != firstCount {
		t.Fatal("retry changed the frozen recipient set", len(api.sends))
	}
	deliverEvent(t, s, event)
	if len(api.sends) != firstCount {
		t.Fatal("accepted event replayed")
	}
	var count int
	if err = store.db.QueryRowContext(t.Context(), "SELECT count(*) FROM ghraft_meta WHERE name=? OR name LIKE ?", "participant-plan:"+event.ID, "participant:"+event.ID+":%").Scan(&count); err != nil || count != 0 {
		t.Fatal("completed fanout metadata leaked", count, err)
	}
	w, err = s.WorkContext(t.Context(), in.EventID)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := s.Store.Event(t.Context(), w.CurrentEventID)
	if err != nil {
		t.Fatal(err)
	}
	deliverEvent(t, s, newer)
	if len(api.sends) != firstCount+4 {
		t.Fatal("new registration did not notify all current work items", len(api.sends))
	}
}

func TestProjectContributionRequiresOwnReportAndCurrentProof(t *testing.T) {
	s, _, api, in, _, ws := projectFixture(t)
	for _, item := range []struct{ id, kind string }{{projectTestTask, "test"}, {projectReviewTask, "review"}} {
		if _, err := s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: in, ParticipantTask: item.id, Kind: item.kind}); err != nil {
			t.Fatal(err)
		}
	}
	reviewer := participantInput(t, s, in.EventID, projectReviewTask, "@reviewer")
	report := ContributionInput{WorkInput: reviewer, ReportID: projectReport, Outcome: "approved", Summary: "Reviewed exact source and proof.", Head: ws.Head, Base: ws.Base}
	if err := s.RecordContribution(t.Context(), report); err == nil {
		t.Fatal("untested approval accepted")
	}
	tester := participantInput(t, s, in.EventID, projectTestTask, "@tester")
	proof, err := s.TestWorkspace(t.Context(), tester)
	if err != nil || !proof.Passed {
		t.Fatal(proof, err)
	}
	report.Head, report.Base = proof.Head, proof.Base
	api.reportTask, api.reportAuthor = projectReviewTask, "stranger"
	if err = s.RecordContribution(t.Context(), report); err == nil {
		t.Fatal("another author's report accepted")
	}
	api.reportTask, api.reportAuthor = projectTestTask, "reviewer"
	if err = s.RecordContribution(t.Context(), report); err == nil {
		t.Fatal("another thread's report accepted")
	}
	api.reportTask, api.reportAuthor = projectReviewTask, "reviewer"
	state := api.tasks[fixtureTask]
	state.Owner = "reviewer"
	api.tasks[fixtureTask] = state
	if err = s.RecordContribution(t.Context(), report); err == nil {
		t.Fatal("coordinator self approval accepted")
	}
	state.Owner = "worker"
	api.tasks[fixtureTask] = state
	if err = s.RecordContribution(t.Context(), report); err != nil {
		t.Fatal(err)
	}
	before, err := s.WorkContext(t.Context(), in.EventID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordContribution(t.Context(), report); err != nil {
		t.Fatal("report retry failed", err)
	}
	after, err := s.WorkContext(t.Context(), in.EventID)
	if err != nil {
		t.Fatal(err)
	}
	if before.CurrentEventID != after.CurrentEventID || before.Collaboration.Revision != after.Collaboration.Revision {
		t.Fatal("same report created another revision/wake")
	}
	// A result notification links all other threads, without waking its author.
	event, err := s.Store.Event(t.Context(), after.CurrentEventID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.Raft.projectParticipantPlan(t.Context(), event)
	if err != nil || len(plan) != 1 || plan[0].Task.MessageID != projectTestTask {
		t.Fatal(plan, err)
	}
	stale := *after.Collaboration
	if _, err = s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: in, ParticipantTask: projectReviewTask, Kind: "review", Revoke: true}); err != nil {
		t.Fatal(err)
	}
	if err = s.Store.SaveForkCollaboration(t.Context(), after.Event.Key, after.Fork.Round, after.CurrentVersion, after.Fork.Revision, stale, nil); !errors.Is(err, ErrStaleWork) {
		t.Fatal("revocation raced past old report", err)
	}
}

func TestProjectQueueReviewDoesNotSelectAnotherPR(t *testing.T) {
	s, q, _, after := newQueueFixture(t)
	s.Config.Forks[0].Channel = "#github-work"
	s.Config.Repositories[0].GitHubActions = append(s.Config.Repositories[0].GitHubActions, "review")
	api := newProjectAPI(t, s)
	appendQueue(t, s, after)
	first := publishQueue(t, s, q)
	in, _ := queueInput(t, s)
	if _, err := s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: in, ParticipantTask: projectReviewTask, Kind: "review"}); err != nil {
		t.Fatal(err)
	}
	appendQueue(t, s, "")
	second := publishQueue(t, s, q)
	in, w := queueInput(t, s)
	if first.BatchID == second.BatchID {
		t.Fatal("fixture did not advance")
	}
	reviewContext, err := s.WorkContextFor(t.Context(), in.EventID, "", projectReviewTask)
	if err != nil || reviewContext.Fork.BatchID != first.BatchID || str(forkFacts(reviewContext)["target_sha"]) != first.ValidatedBase {
		t.Fatal(reviewContext.Fork, err)
	}
	reviewer := participantInput(t, s, in.EventID, projectReviewTask, "@reviewer")
	api.reportTask, api.reportAuthor = projectReviewTask, "reviewer"
	check, err := s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: reviewer, Action: "review", Ref: first.Branch, Head: first.ValidatedHead, Base: first.ValidatedBase})
	if err != nil || !check.Permitted || check.Number != first.PR {
		t.Fatal("independent delegated PR review rejected", check, err)
	}
	if err = s.RecordContribution(t.Context(), ContributionInput{WorkInput: reviewer, ReportID: projectReport, Outcome: "approved", Summary: "Reviewed earlier PR while synchronization continues.", Head: first.ValidatedHead, Base: first.ValidatedBase}); err != nil {
		t.Fatal(err)
	}
	current, err := s.Store.ForkProgress(t.Context(), w.Event.Key)
	if err != nil || current.BatchID != second.BatchID || current.Revision != w.Fork.Revision {
		t.Fatal("report changed source selection", current.BatchID, err)
	}
	if _, err = s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: in, ParticipantTask: projectReviewTask, Kind: "review"}); err == nil {
		t.Fatal("one task associated with two PRs")
	}
	if _, err = s.WorkContextFor(t.Context(), in.EventID, second.BatchID, projectReviewTask); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("participant selected another PR", err)
	}
	// Coordinators can associate an extra investigator with the old PR without
	// changing which branch the source writer is preparing.
	in.BatchID = first.BatchID
	if _, err = s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: in, ParticipantTask: projectTestTask, Kind: "investigate"}); err != nil {
		t.Fatal(err)
	}
	current, err = s.Store.ForkProgress(t.Context(), w.Event.Key)
	if err != nil || current.BatchID != second.BatchID {
		t.Fatal("registration changed queue selection", err)
	}
}

func TestProjectRevocationRecoversPendingDeletedRecipient(t *testing.T) {
	s, g, api, in, _, _ := projectFixture(t)
	if _, err := s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: in, ParticipantTask: projectTestTask, Kind: "test"}); err != nil {
		t.Fatal(err)
	}
	tree := fixtureGit(t, g.repository, "", "rev-parse", g.target+"^{tree}")
	g.target = syntheticCommit(t, g.repository, tree, g.target)
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	w, err := s.WorkContext(t.Context(), in.EventID)
	if err != nil || !w.Stale {
		t.Fatal("expected newer source evidence", err)
	}
	next, err := s.Store.Event(t.Context(), w.CurrentEventID)
	if err != nil {
		t.Fatal(err)
	}
	delete(api.tasks, projectTestTask)
	if _, err = s.Raft.Send(t.Context(), next); err == nil {
		t.Fatal("missing recipient ignored before explicit revocation")
	}
	// The source has changed and the newest delivery is pending. The original
	// coordinator can still revoke the deleted task using the current versions.
	in.EvidenceVersion, in.GuideVersion = w.CurrentVersion, w.Guide.Version
	if _, err = s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: in, ParticipantTask: projectTestTask, Kind: "test", Revoke: true}); err != nil {
		t.Fatal(err)
	}
	deliverEvent(t, s, next)
	if _, err = s.PrepareWorkspace(t.Context(), in, g.repository); !errors.Is(err, ErrStaleWork) {
		t.Fatal("administrative exception authorized stale source work", err)
	}
}

func TestProjectChannelChangePreservesExistingDiscussionAndPendingRoute(t *testing.T) {
	s, _, _ := forkFixture(t)
	api := newProjectAPI(t, s)
	in, _ := currentForkInput(t, s)
	if len(api.requests) != 1 {
		t.Fatal("expected original coordination task")
	}
	s.Config.Forks[0].Channel = "#github-work"
	s.Raft.Config = s.Config
	event, err := s.RequestWork(t.Context(), in.EventID, projectReport, "Enable explicit project collaboration")
	if err != nil {
		t.Fatal(err)
	}
	deliverEvent(t, s, event)
	if len(api.requests) != 1 {
		t.Fatal("project mode duplicated the existing coordination task")
	}
	w, err := s.WorkContext(t.Context(), event.ID)
	if err != nil || w.Discussion == nil || w.Discussion.MessageID != fixtureTask {
		t.Fatal(w.Discussion, err)
	}
	pending, err := s.RequestWork(t.Context(), event.ID, projectTestTask, "A later project event")
	if err != nil {
		t.Fatal(err)
	}
	api.lostSend = true
	if _, err = s.Raft.Send(t.Context(), pending); err == nil {
		t.Fatal("expected lost original delivery")
	}
	s.Config.Forks[0].Channel = "#new-project"
	s.Raft.Config = s.Config
	if _, err = s.Raft.Send(t.Context(), pending); err == nil {
		t.Fatal("pending notification moved to a new channel")
	}
	if len(api.requests) != 1 {
		t.Fatal("unresolved old delivery created a new root")
	}
	if err = s.VerifyWorkItemClaim(t.Context(), WorkInput{EventID: event.ID, TaskID: fixtureTask, Executor: "@worker"}); err == nil {
		t.Fatal("old channel task retained authority")
	}
}

func TestProjectFormerWriterCannotApproveAfterHandoff(t *testing.T) {
	s, _, api, in, _, _ := projectFixture(t)
	old := api.tasks[projectSyncTask]
	old.Owner = "reviewer"
	api.tasks[projectSyncTask] = old
	for _, item := range []struct{ id, kind string }{{projectSyncTask, "sync"}, {projectTestTask, "test"}, {projectReviewTask, "review"}} {
		if _, err := s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: in, ParticipantTask: item.id, Kind: item.kind}); err != nil {
			t.Fatal(err)
		}
	}
	participantInput(t, s, in.EventID, projectSyncTask, "@reviewer")
	old.Owner = "successor"
	api.tasks[projectSyncTask] = old
	if _, err := s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: in, ParticipantTask: projectSyncTask, Kind: "sync", Revoke: true}); err != nil {
		t.Fatal(err)
	}
	tester := participantInput(t, s, in.EventID, projectTestTask, "@tester")
	proof, err := s.TestWorkspace(t.Context(), tester)
	if err != nil || !proof.Passed {
		t.Fatal(proof, err)
	}
	reviewer := participantInput(t, s, in.EventID, projectReviewTask, "@reviewer")
	api.reportTask, api.reportAuthor = projectReviewTask, "reviewer"
	err = s.RecordContribution(t.Context(), ContributionInput{WorkInput: reviewer, ReportID: projectReport, Outcome: "approved", Summary: "Former source writer claiming an independent review.", Head: proof.Head, Base: proof.Base})
	if err == nil || !strings.Contains(err.Error(), "handoff") {
		t.Fatal("former writer self-approved", err)
	}
}

func TestProjectPassedResultRequiresTheExecutingTask(t *testing.T) {
	s, _, api, in, _, _ := projectFixture(t)
	if _, err := s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: in, ParticipantTask: projectTestTask, Kind: "test"}); err != nil {
		t.Fatal(err)
	}
	tester := participantInput(t, s, in.EventID, projectTestTask, "@tester")
	proof, err := s.TestWorkspace(t.Context(), tester)
	if err != nil || !proof.Passed {
		t.Fatal(proof, err)
	}
	api.reportTask, api.reportAuthor = projectTestTask, "tester"
	report := ContributionInput{WorkInput: tester, ReportID: projectReport, Outcome: "passed", Summary: "Trusted full validation completed.", Head: proof.Head, Base: proof.Base}
	if err = s.RecordContribution(t.Context(), report); err != nil {
		t.Fatal(err)
	}
	w, err := s.WorkContextFor(t.Context(), in.EventID, "", projectTestTask)
	if err != nil || !w.Collaboration.item(projectTestTask).Result.Current || w.Collaboration.item(projectTestTask).Result.TestAttempt != proof.Attempt {
		t.Fatal(w.Collaboration, err)
	}
	state := api.tasks[projectTestTask]
	state.Owner = "successor"
	api.tasks[projectTestTask] = state
	report.Executor = "@successor"
	api.reportAuthor = "successor"
	if err = s.RecordContribution(t.Context(), report); err == nil || !strings.Contains(err.Error(), "own completed validation") {
		t.Fatal("successor claimed predecessor's test run", err)
	}
}

func TestProjectHandoffRecordsSourceAuthorityBeforeWork(t *testing.T) {
	for _, kind := range []string{"coordinate", "sync"} {
		t.Run(kind, func(t *testing.T) {
			s, g, api, coord, _, _ := projectFixture(t)
			for _, item := range []struct{ id, kind string }{{projectTestTask, "test"}, {projectReviewTask, "review"}} {
				if _, err := s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: coord, ParticipantTask: item.id, Kind: item.kind}); err != nil {
					t.Fatal(err)
				}
			}
			writer := coord
			if kind == "sync" {
				if _, err := s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: coord, ParticipantTask: projectSyncTask, Kind: kind}); err != nil {
					t.Fatal(err)
				}
				writer = participantInput(t, s, coord.EventID, projectSyncTask, "@syncer")
			}
			state := api.tasks[writer.TaskID]
			state.Owner = "reviewer"
			api.tasks[writer.TaskID] = state
			writer.Executor = "@reviewer"
			// The successor reuses the predecessor's valid link instead of linking
			// again. The permission check itself must preserve this writer.
			if _, err := s.PrepareWorkspace(t.Context(), writer, g.repository); err != nil {
				t.Fatal(err)
			}
			state.Owner = "successor"
			api.tasks[writer.TaskID] = state
			tester := participantInput(t, s, coord.EventID, projectTestTask, "@tester")
			proof, err := s.TestWorkspace(t.Context(), tester)
			if err != nil || !proof.Passed {
				t.Fatal(proof, err)
			}
			reviewer := participantInput(t, s, coord.EventID, projectReviewTask, "@reviewer")
			api.reportTask, api.reportAuthor = projectReviewTask, "reviewer"
			err = s.RecordContribution(t.Context(), ContributionInput{WorkInput: reviewer, ReportID: projectReport, Outcome: "approved", Summary: "Review after a source-task handoff.", Head: proof.Head, Base: proof.Base})
			if err == nil || !strings.Contains(err.Error(), "handoff") {
				t.Fatal("unrecorded successor approved its own work", err)
			}
		})
	}
}

func TestProjectDeletedRevokedTaskRetainsOnlyHistoricalActors(t *testing.T) {
	s, _, api, coord, _, _ := projectFixture(t)
	for _, item := range []struct{ id, kind string }{{projectSyncTask, "sync"}, {projectTestTask, "test"}, {projectReviewTask, "review"}} {
		if _, err := s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: coord, ParticipantTask: item.id, Kind: item.kind}); err != nil {
			t.Fatal(err)
		}
	}
	participantInput(t, s, coord.EventID, projectSyncTask, "@syncer")
	if _, err := s.RegisterForkWorkItem(t.Context(), ForkWorkItemInput{WorkInput: coord, ParticipantTask: projectSyncTask, Kind: "sync", Revoke: true}); err != nil {
		t.Fatal(err)
	}
	delete(api.tasks, projectSyncTask)
	tester := participantInput(t, s, coord.EventID, projectTestTask, "@tester")
	proof, err := s.TestWorkspace(t.Context(), tester)
	if err != nil || !proof.Passed {
		t.Fatal(proof, err)
	}
	reviewer := participantInput(t, s, coord.EventID, projectReviewTask, "@reviewer")
	api.reportTask, api.reportAuthor = projectReviewTask, "reviewer"
	report := ContributionInput{WorkInput: reviewer, ReportID: projectReport, Outcome: "approved", Summary: "Independent review after revocation.", Head: proof.Head, Base: proof.Base}
	if err = s.RecordContribution(t.Context(), report); err != nil {
		t.Fatal("deleted revoked task blocked an independent reviewer", err)
	}
	state := api.tasks[projectReviewTask]
	state.Owner = "syncer"
	api.tasks[projectReviewTask] = state
	report.WorkInput = participantInput(t, s, coord.EventID, projectReviewTask, "@syncer")
	api.reportAuthor = "syncer"
	if err = s.RecordContribution(t.Context(), report); err == nil || !strings.Contains(err.Error(), "handoff") {
		t.Fatal("deleting the task erased the original writer", err)
	}
}
