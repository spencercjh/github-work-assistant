package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func lifecycleProject(t *testing.T) (*Service, *queueSource, *projectAPI, ForkProgress, ForkWorkItem) {
	t.Helper()
	s, q, _, after := newQueueFixtureMode(t, false)
	s.Config.Forks[0].Channel = "#github-work"
	api := newProjectAPI(t, s)
	appendQueue(t, s, after)
	p := publishQueue(t, s, q)
	item := prDiscussionItem(t, s, p)
	state := api.tasks[item.Task.MessageID]
	state.Owner, state.Status = "reviewer", "in_review"
	api.tasks[item.Task.MessageID] = state
	return s, q, api, p, item
}

func TestProjectMergedPRCompletionSurvivesFailureAndArchival(t *testing.T) {
	s, q, api, first, task := lifecycleProject(t)
	appendQueue(t, s, "")
	second := publishQueue(t, s, q)
	other := prDiscussionItem(t, s, second)
	mergeQueue(t, s, q, first.BatchID)
	in, w := queueInput(t, s)
	writes := 0
	if _, err := s.CompleteMergedPRTasks(t.Context(), in, func(context.Context, TaskDispatch, string) error {
		writes++
		return errors.New("unread discussion")
	}); err == nil {
		t.Fatal("failed remote update was reported complete")
	}
	pending, err := s.Store.PendingPRCompletions(t.Context(), w.Event.Key)
	if err != nil || len(pending) != 1 || pending[0].PR != first.PR {
		t.Fatal("verified merge did not retain its completion intent", pending, err)
	}
	// The completed batch can leave the active queue before status delivery is
	// retried. Completion uses the original receipt, not the selected batch.
	p := *w.Fork
	p.Queue.Batches = p.Queue.Batches[1:]
	if err = s.Store.SaveForkProgress(t.Context(), w.Event.Key, p.Revision, p, nil); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t)
	s.Store, s.Raft.Store = store, store
	closed, err := s.FinishWork(t.Context(), in, func(_ context.Context, target TaskDispatch, status string) error {
		writes++
		if target != task.Task || status != "done" {
			t.Fatal("completed another task", target)
		}
		api.mu.Lock()
		defer api.mu.Unlock()
		state := api.tasks[target.MessageID]
		state.Status = status
		api.tasks[target.MessageID] = state
		return errors.New("response lost after successful write")
	})
	if err != nil || closed.Status != "in_progress" || len(closed.PRTasks) != 1 || closed.PRTasks[0].Task != task.Task {
		t.Fatal("lost reply was not recovered through readback", closed, err)
	}
	if api.tasks[fixtureTask].Status != "in_progress" || api.tasks[other.Task.MessageID].Status == "done" {
		t.Fatal("PR closure affected coordinator or another PR")
	}
	// A completed receipt must not overwrite a later manual reopening.
	state := api.tasks[task.Task.MessageID]
	state.Status = "in_progress"
	api.tasks[task.Task.MessageID] = state
	if _, err = s.FinishWork(t.Context(), in, func(context.Context, TaskDispatch, string) error {
		t.Fatal("replayed completed task update")
		return nil
	}); err != nil || writes != 2 {
		t.Fatal(err, writes)
	}
}

func TestProjectPRCompletionRequiresVerifiedMergeAndLiveScope(t *testing.T) {
	s, q, api, p, item := lifecycleProject(t)
	in, w := queueInput(t, s)
	denyWrite := func(context.Context, TaskDispatch, string) error {
		t.Fatal("unexpected task mutation")
		return nil
	}
	if err := s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: projectReport, Outcome: "merged", Summary: "Not actually merged"}); err == nil {
		t.Fatal("unmerged PR accepted")
	}
	if got, err := s.CompleteMergedPRTasks(t.Context(), in, denyWrite); err != nil || len(got) != 0 {
		t.Fatal("unverified merge created a completion intent", got, err)
	}
	mergeQueue(t, s, q, p.BatchID)
	in, w = queueInput(t, s)
	c, err := s.Store.ForkCollaboration(t.Context(), w.Event.Key, p.BatchID, item.Task.Channel)
	if err != nil {
		t.Fatal(err)
	}
	original := string(canonical(c))
	for _, boundary := range []string{"participant", "coordinator handoff", "human", "paused", "closed", "revoked", "different PR", "route"} {
		t.Run(boundary, func(t *testing.T) {
			input := in
			before, root := api.tasks[item.Task.MessageID], api.tasks[fixtureTask]
			defer func() {
				api.tasks[item.Task.MessageID], api.tasks[fixtureTask] = before, root
				s.Config.Forks[0].Channel = "#github-work"
				if err := s.Store.PutMeta(t.Context(), collaborationKey(w.Event.Key, p.BatchID, item.Task.Channel), original); err != nil {
					t.Fatal(err)
				}
			}()
			state := before
			switch boundary {
			case "participant":
				input.TaskID, input.Executor = item.Task.MessageID, "reviewer"
			case "coordinator handoff":
				root.Owner = "successor"
				api.tasks[fixtureTask] = root
			case "human":
				state.OwnerType = "user"
			case "paused":
				state.Status = "todo"
			case "closed":
				state.Status = "closed"
			case "revoked", "different PR":
				var changed ForkCollaboration
				_ = json.Unmarshal([]byte(original), &changed)
				if boundary == "revoked" {
					changed.item(item.Task.MessageID).Revoked = true
				} else {
					changed.item(item.Task.MessageID).PR++
				}
				if err := s.Store.PutMeta(t.Context(), collaborationKey(w.Event.Key, p.BatchID, item.Task.Channel), string(canonical(changed))); err != nil {
					t.Fatal(err)
				}
			case "route":
				s.Config.Forks[0].Channel = "#another-project"
			}
			api.tasks[item.Task.MessageID] = state
			if _, err := s.CompleteMergedPRTasks(t.Context(), input, denyWrite); err == nil {
				t.Fatal("unsafe completion accepted", boundary)
			}
		})
	}
}

func TestProjectStatusDeliveryIsScopedDurableAndPrivate(t *testing.T) {
	s, q, api, first, task := lifecycleProject(t)
	appendQueue(t, s, "")
	second := publishQueue(t, s, q)
	other := prDiscussionItem(t, s, second)
	selectQueue(t, s, first.BatchID)
	in, w := queueInput(t, s)
	result := WorkResult{WorkInput: in, ReportID: projectReport, Outcome: "waiting", Summary: "Check has not run; @owner must decide the policy separately."}
	if err := s.RecordWorkResult(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	w, err := s.WorkContext(t.Context(), in.EventID)
	if err != nil || w.Fork.WorkStatus == nil || w.Fork.WorkStatus.Result.ReportID != projectReport {
		t.Fatal("recorded reason is missing from context", err)
	}
	event, err := s.Store.Event(t.Context(), w.CurrentEventID)
	if err != nil {
		t.Fatal(err)
	}
	api.lostSend, api.lostTarget = true, task.Task.target()
	if err = s.Raft.sendProjectParticipants(t.Context(), event); err == nil {
		t.Fatal("lost notification receipt reported success")
	}
	key := "github-participant:" + event.ID + ":" + task.Task.MessageID
	request := api.sends[key]
	if !strings.Contains(request, "Check has not run") || !strings.Contains(request, projectReport) || strings.Contains(request, "@owner") {
		t.Fatal("reason/reference missing or human was mentioned again", request)
	}
	if api.sends["github-participant:"+event.ID+":"+other.Task.MessageID] != "" {
		t.Fatal("status leaked into another PR")
	}
	store := openTestStore(t)
	s.Store, s.Raft.Store = store, store
	if err = s.Raft.sendProjectParticipants(t.Context(), event); err != nil || api.sends[key] != request {
		t.Fatal("restart changed or lost the notification", err)
	}
	// Private notifications omit the whole summary and report reference.
	private, err := store.RequestWork(t.Context(), w.Event.Key, w.CurrentVersion, "60000000-0000-4000-8000-000000000001", "Private status fixture")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.Unmarshal(event.Payload, &payload)
	payload["data_classification"] = "private"
	private.Payload = canonical(payload)
	s.Config.PrivateNotificationDetails = false
	s.Raft.Config = s.Config
	if err = s.Raft.sendProjectParticipants(t.Context(), private); err != nil {
		t.Fatal(err)
	}
	message := api.sends["github-participant:"+private.ID+":"+task.Task.MessageID]
	if message == "" || strings.Contains(message, "Check has not run") || strings.Contains(message, projectReport) {
		t.Fatal("private report was exposed", message)
	}
}

func TestProjectRepeatedDecisionDoesNotReplaceNewRequest(t *testing.T) {
	s, _, _, _, _ := lifecycleProject(t)
	in, w := queueInput(t, s)
	// Decision-mention validation has separate public-entry regressions. Here
	// exercise the persisted status after that check, including report reuse.
	result := WorkResult{WorkInput: in, ReportID: projectReport, Outcome: "needs-decision", Summary: "Decide missing check coverage before continuing."}
	if err := s.recordForkResult(t.Context(), w, result); err != nil {
		t.Fatal(err)
	}
	in, w = queueInput(t, s)
	if _, err := s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, Count: 1, Reason: "skip unresolved scope"}); err == nil {
		t.Fatal("status notification relaxed the queue gate")
	}
	newRequest, err := s.Store.RequestWork(t.Context(), w.Event.Key, w.CurrentVersion, "60000000-0000-4000-8000-000000000002", "Investigate a newly reported problem before merging")
	if err != nil {
		t.Fatal(err)
	}
	deliverEvent(t, s, newRequest)
	in, w = currentForkInput(t, s)
	result.WorkInput = in
	if err = s.recordForkResult(t.Context(), w, result); err != nil {
		t.Fatal(err)
	}
	latest, err := s.WorkContext(t.Context(), in.EventID)
	if err != nil || latest.CurrentEventID != newRequest.ID {
		t.Fatal("same decision rewound the latest work request", latest.CurrentEventID, err)
	}
}
