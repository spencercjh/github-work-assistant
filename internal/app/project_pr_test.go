package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func prDiscussionItem(t *testing.T, s *Service, p ForkProgress) ForkWorkItem {
	t.Helper()
	c, err := s.Store.ForkCollaboration(t.Context(), s.Config.Forks[0].Key(), collaborationID(&p), s.Config.Forks[0].Channel)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range c.Items {
		if item.PRDiscussion && item.PR == p.PR {
			return item
		}
	}
	t.Fatal("published PR has no automatic discussion task")
	return ForkWorkItem{}
}

func TestProjectPRTasksAreDistinctPersistentAndReviewScoped(t *testing.T) {
	s, q, _, after := newQueueFixture(t)
	s.Config.Forks[0].Channel = "#github-work"
	api := newProjectAPI(t, s)
	appendQueue(t, s, after)
	first := publishQueue(t, s, q)
	one := prDiscussionItem(t, s, first)
	appendQueue(t, s, "")
	second := publishQueue(t, s, q)
	two := prDiscussionItem(t, s, second)
	if one.Task.MessageID == two.Task.MessageID || one.Task.MessageID == fixtureTask || len(api.prRoots) != 2 {
		t.Fatal("PRs reused another PR or the coordinator", one, two)
	}
	state := api.tasks[one.Task.MessageID]
	if state.Owner != "" {
		t.Fatal("collector assigned a reviewer")
	}
	state.Owner = "reviewer"
	api.tasks[one.Task.MessageID] = state
	in, w := queueInput(t, s)
	context, err := s.WorkContextFor(t.Context(), in.EventID, "", one.Task.MessageID)
	if err != nil || context.Fork.BatchID != first.BatchID {
		t.Fatal("PR task did not select its own read context", context.Fork, err)
	}
	reviewer := participantInput(t, s, in.EventID, one.Task.MessageID, "@reviewer")
	if _, err = s.PrepareWorkspace(t.Context(), reviewer, q.repository); err == nil {
		t.Fatal("PR discussion granted source-write authority")
	}
	api.reportTask, api.reportAuthor = one.Task.MessageID, "reviewer"
	if err = s.RecordContribution(t.Context(), ContributionInput{WorkInput: reviewer, ReportID: projectReport, Outcome: "approved", Summary: "Invited review in the PR's own thread.", Head: first.ValidatedHead, Base: first.ValidatedBase}); err != nil {
		t.Fatal(err)
	}
	current, err := s.Store.ForkProgress(t.Context(), w.Event.Key)
	if err != nil || current.BatchID != second.BatchID {
		t.Fatal("review changed the writer's queue selection", err)
	}
	q.prs[first.Branch].check = "failure"
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	w, err = s.WorkContext(t.Context(), in.EventID)
	if err != nil {
		t.Fatal(err)
	}
	event, err := s.Store.Event(t.Context(), w.CurrentEventID)
	if err != nil {
		t.Fatal(err)
	}
	deliverEvent(t, s, event)
	key := "github-participant:" + event.ID + ":" + one.Task.MessageID
	var message map[string]any
	if json.Unmarshal([]byte(api.sends[key]), &message) != nil || message["target"] != one.Task.target() {
		t.Fatal("CI update did not return to the earlier PR's thread", api.sends)
	}
	store := openTestStore(t)
	s.Store, s.Raft.Store = store, store
	deliverEvent(t, s, event)
	if len(api.prRoots) != 2 {
		t.Fatal("restart/replay duplicated a PR task")
	}
	if strings.Contains(api.requests[projectPRTaskKey(w.Event.Key, one.Task.Channel, first.PR)], "始终保持 in_progress") {
		t.Fatal("PR task inherited the perpetual coordinator lifecycle")
	}
}

func TestProjectPRTaskLostResponseRetryAndRouteGuard(t *testing.T) {
	s, q, _, after := newQueueFixture(t)
	s.Config.Forks[0].Channel = "#github-work"
	api := newProjectAPI(t, s)
	appendQueue(t, s, after)
	p, ws, _ := portQueue(t, s, q)
	q.prs[p.Branch] = &queueRemote{number: 101, head: ws.Head, base: p.TargetBranch, baseSHA: p.InitialTarget, check: "success"}
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, w := currentForkInputWithoutDelivery(t, s)
	event, err := s.Store.Event(t.Context(), w.CurrentEventID)
	if err != nil {
		t.Fatal(err)
	}
	api.lostPRTask = true
	if _, err = s.Raft.Send(t.Context(), event); err == nil {
		t.Fatal("lost PR creation response was reported accepted")
	}
	key := projectPRTaskKey(event.Key, "#github-work", 101)
	request := api.requests[key]
	if len(api.prRoots) != 1 || request == "" {
		t.Fatal("fixture did not create the task before losing the response")
	}
	s.Config.Forks[0].Channel = "#other-project"
	s.Raft.Config = s.Config
	if _, err = s.Raft.Send(t.Context(), event); err == nil || !strings.Contains(err.Error(), "previous channel") {
		t.Fatal("ambiguous creation moved to another channel", err)
	}
	s.Config.Forks[0].Channel = "#github-work"
	s.Raft.Config = s.Config
	store := openTestStore(t)
	s.Store, s.Raft.Store = store, store
	deliverEvent(t, s, event)
	if len(api.prRoots) != 1 || api.requests[key] != request {
		t.Fatal("restart did not reuse the exact PR creation request")
	}
	w, err = s.WorkContext(t.Context(), event.ID)
	if err != nil {
		t.Fatal(err)
	}
	item := prDiscussionItem(t, s, *w.Fork)
	if item.Task.MessageID != api.prRoots[key] {
		t.Fatal("lost response linked a different task")
	}
	// A finished PR remains the same fixed discussion. Its final queue-removal
	// event must still reach that thread even after its active batch disappears.
	current := *w.Fork
	current.Queue.Batches = nil
	if err = store.SaveForkProgress(t.Context(), event.Key, current.Revision, current, nil); err != nil {
		t.Fatal(err)
	}
	last, err := store.RequestWork(t.Context(), event.Key, w.CurrentVersion, projectReport, "Record archived PR update")
	if err != nil {
		t.Fatal(err)
	}
	last.Payload = canonical(map[string]any{"changes": []Change{{Object: "queue:" + current.BatchID, Before: map[string]any{"pr": float64(101)}, After: nil}}})
	plan, err := s.Raft.projectParticipantPlan(t.Context(), last)
	if err != nil || len(plan) != 1 || plan[0].Task.MessageID != item.Task.MessageID {
		t.Fatal("archived PR lost its final notification", plan, err)
	}
}

func TestProjectPRTaskExpiryAndPrivacy(t *testing.T) {
	s, q, _, after := newQueueFixture(t)
	s.Config.Forks[0].Channel = "#github-work"
	api := newProjectAPI(t, s)
	appendQueue(t, s, after)
	p, ws, _ := portQueue(t, s, q)
	q.prs[p.Branch] = &queueRemote{number: 101, head: ws.Head, base: p.TargetBranch, baseSHA: p.InitialTarget}
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, w := currentForkInputWithoutDelivery(t, s)
	progress := *w.Fork
	for i := range progress.Queue.Batches {
		progress.Queue.Batches[i].Observation.Classification = "private"
	}
	if err := s.Store.SaveForkProgress(t.Context(), w.Event.Key, progress.Revision, progress, nil); err != nil {
		t.Fatal(err)
	}
	s.Config.PrivateNotificationDetails = false
	s.Raft.Config = s.Config
	event, err := s.Store.Event(t.Context(), w.CurrentEventID)
	if err != nil {
		t.Fatal(err)
	}
	api.lostPRTask = true
	if _, err = s.Raft.Send(t.Context(), event); err == nil {
		t.Fatal("expected ambiguous creation")
	}
	key := projectPRTaskKey(event.Key, "#github-work", 101)
	if body := api.requests[key]; strings.Contains(body, s.Config.Forks[0].Target.Repo) || strings.Contains(body, "/pull/101") {
		t.Fatal("private PR metadata escaped into its task")
	}
	store := s.Store.(*Store)
	var raw string
	if err = store.db.QueryRowContext(t.Context(), "SELECT value FROM ghraft_meta WHERE name=?", key).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var pending projectPRTask
	if err = json.Unmarshal([]byte(raw), &pending); err != nil {
		t.Fatal(err)
	}
	q.prs[p.Branch].closed = true
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	pending.CreatedAt = time.Now().Add(-24 * time.Hour).Unix()
	if err = store.PutMeta(t.Context(), key, string(canonical(pending))); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Raft.Send(t.Context(), event); err == nil || !strings.Contains(err.Error(), "safe retry window") {
		t.Fatal("expired ambiguous create was resent", err)
	}
}

func currentForkInputWithoutDelivery(t *testing.T, s *Service) (WorkInput, WorkContext) {
	t.Helper()
	events, err := s.Store.Events(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Key == s.Config.Forks[0].Key() {
			w, err := s.WorkContext(t.Context(), event.ID)
			if err != nil {
				t.Fatal(err)
			}
			return WorkInput{EventID: event.ID}, w
		}
	}
	t.Fatal("no fork event")
	return WorkInput{}, WorkContext{}
}

func TestProjectClosedPRTaskCannotAuthorizeOrReceiveReplacement(t *testing.T) {
	s, q, _, after := newQueueFixture(t)
	s.Config.Forks[0].Channel = "#github-work"
	api := newProjectAPI(t, s)
	appendQueue(t, s, after)
	first := publishQueue(t, s, q)
	old := prDiscussionItem(t, s, first)
	state := api.tasks[old.Task.MessageID]
	state.Owner = "reviewer"
	api.tasks[old.Task.MessageID] = state
	_, before := queueInput(t, s)
	delayed, err := s.Store.RequestWork(t.Context(), before.Event.Key, before.CurrentVersion, projectReport, "Review old PR before replacement")
	if err != nil {
		t.Fatal(err)
	}
	q.prs[first.Branch].closed = true
	in, w := queueInput(t, s)
	if _, err := s.RebuildForkBatch(t.Context(), ForkQueueInput{WorkInput: in, Batch: w.Fork.BatchID}); err != nil {
		t.Fatal(err)
	}
	replacement := publishQueue(t, s, q)
	if replacement.PR == first.PR {
		t.Fatal("fixture did not replace PR")
	}
	newItem := prDiscussionItem(t, s, replacement)
	if newItem.Task.MessageID == old.Task.MessageID {
		t.Fatal("replacement reused old PR task")
	}
	in, _ = queueInput(t, s)
	oldContext, err := s.WorkContextFor(t.Context(), in.EventID, "", old.Task.MessageID)
	if err == nil && oldContext.Fork.PR == replacement.PR {
		t.Errorf("closed PR %d task now authorizes context for replacement PR %d", first.PR, replacement.PR)
	}
	reviewer := in
	reviewer.TaskID, reviewer.Executor = old.Task.MessageID, "@reviewer"
	if _, linkErr := s.LinkTask(t.Context(), reviewer); linkErr == nil {
		t.Error("old PR task linked replacement PR")
	}
	api.reportTask, api.reportAuthor = old.Task.MessageID, "reviewer"
	if err = s.RecordContribution(t.Context(), ContributionInput{WorkInput: reviewer, ReportID: projectReport, Outcome: "approved", Summary: "Old PR task cannot approve its replacement", Head: replacement.ValidatedHead, Base: replacement.ValidatedBase}); err == nil {
		t.Error("old PR task approved replacement PR")
	}
	newState := api.tasks[newItem.Task.MessageID]
	newState.Owner = "reviewer"
	api.tasks[newItem.Task.MessageID] = newState
	newReviewer := participantInput(t, s, in.EventID, newItem.Task.MessageID, "@reviewer")
	api.reportTask = newItem.Task.MessageID
	if err = s.RecordContribution(t.Context(), ContributionInput{WorkInput: newReviewer, ReportID: projectReport, Outcome: "approved", Summary: "Review the replacement in its own task", Head: replacement.ValidatedHead, Base: replacement.ValidatedBase}); err != nil {
		t.Fatal("replacement task could not approve its own PR", err)
	}
	q.prs[replacement.Branch].check = "failure"
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	w, err = s.WorkContext(t.Context(), in.EventID)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := s.Store.Event(t.Context(), w.CurrentEventID)
	if err != nil {
		t.Fatal(err)
	}
	deliverEvent(t, s, ev)
	if api.sends["github-participant:"+ev.ID+":"+newItem.Task.MessageID] == "" {
		t.Error("replacement did not receive its CI update")
	}
	if msg := api.sends["github-participant:"+ev.ID+":"+old.Task.MessageID]; msg != "" {
		t.Errorf("replacement PR CI update was delivered to closed PR task: %s", msg)
	}
	deliverEvent(t, s, delayed)
	if api.sends["github-participant:"+delayed.ID+":"+newItem.Task.MessageID] != "" {
		t.Error("delayed old PR event reached replacement task")
	}
	if api.sends["github-participant:"+delayed.ID+":"+old.Task.MessageID] == "" {
		t.Error("old PR lost its delayed notification")
	}
}

func TestProjectLostPRTaskResponseRecoversAfterClosure(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(fmt.Sprintf("archived=%t", archived), func(t *testing.T) {
			s, q, _, after := newQueueFixture(t)
			s.Config.Forks[0].Channel = "#github-work"
			api := newProjectAPI(t, s)
			appendQueue(t, s, after)
			p, ws, _ := portQueue(t, s, q)
			q.prs[p.Branch] = &queueRemote{number: 101, head: ws.Head, base: p.TargetBranch, baseSHA: p.InitialTarget, check: "success"}
			if err := s.Collect(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, w := currentForkInputWithoutDelivery(t, s)
			ev, err := s.Store.Event(t.Context(), w.CurrentEventID)
			if err != nil {
				t.Fatal(err)
			}
			api.lostPRTask = true
			if _, err = s.Raft.Send(t.Context(), ev); err == nil {
				t.Fatal("expected lost response")
			}
			key := projectPRTaskKey(ev.Key, "#github-work", 101)
			if api.prRoots[key] == "" {
				t.Fatal("task was not created")
			}
			q.prs[p.Branch].closed = true
			if err = s.Collect(t.Context()); err != nil {
				t.Fatal(err)
			}
			if archived {
				current, err := s.Store.ForkProgress(t.Context(), ev.Key)
				if err != nil {
					t.Fatal(err)
				}
				current.Queue.Batches = nil
				if err = s.Store.SaveForkProgress(t.Context(), ev.Key, current.Revision, current, nil); err != nil {
					t.Fatal(err)
				}
			}

			deliverEvent(t, s, ev)
			var raw string
			if err = s.Store.(*Store).db.QueryRowContext(t.Context(), "SELECT value FROM ghraft_meta WHERE name=?", key).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var saved projectPRTask
			if err = json.Unmarshal([]byte(raw), &saved); err != nil {
				t.Fatal(err)
			}
			if len(api.prRoots) != 1 {
				t.Fatal("recovery duplicated task")
			}
			if saved.Task == nil {
				t.Fatal("event accepted but remotely created PR task receipt remains unresolved after PR closes")
			}
			c, err := s.Store.ForkCollaboration(t.Context(), ev.Key, saved.Batch, saved.Channel)
			if err != nil || c.item(saved.Task.MessageID) == nil || c.item(saved.Task.MessageID).PR != 101 {
				t.Fatal("confirmed receipt lost its PR-scoped binding", c, err)
			}
		})
	}
}
