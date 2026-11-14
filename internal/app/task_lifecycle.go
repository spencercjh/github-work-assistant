package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Only ordinary GitHub work has a terminal state. A merged synchronization PR
// does not finish a long-lived fork maintenance rule.
func githubEnded(body json.RawMessage) bool {
	var s Snapshot
	if json.Unmarshal(body, &s) != nil || s.Objects["fork"] != nil {
		return false
	}
	if p := s.Objects["pull"]; p != nil {
		return p["merged"] == true || p["state"] == "closed"
	}
	return s.Objects["issue"]["state"] == "closed"
}

func explicitWork(ev Event) bool {
	var p struct{ Kind string }
	return json.Unmarshal(ev.Payload, &p) == nil && p.Kind == "work.requested"
}

// A human can finish tracking an ordinary PR/Issue while GitHub remains open.
// The owner may acknowledge an update as quiet without gaining permission to
// execute work. Actual reopening and explicit work requests still require the
// owner to read the discussion and resume the existing task.
func quietDoneUpdate(w WorkContext) bool {
	var snapshot Snapshot
	if json.Unmarshal(w.Snapshot, &snapshot) != nil || w.Fork != nil || snapshot.Objects["fork"] != nil {
		return false
	}
	item := snapshot.Objects["pull"]
	if item == nil {
		item = snapshot.Objects["issue"]
	}
	if item["state"] != "open" && item["state"] != "closed" {
		return false
	}
	var p struct {
		Kind    string   `json:"kind"`
		Changes []Change `json:"changes"`
	}
	if json.Unmarshal(w.Event.Payload, &p) != nil || p.Kind != "" {
		return false
	}
	for _, change := range p.Changes {
		if (change.Object == "pull" || change.Object == "issue") && obj(change.Before)["state"] == "closed" && obj(change.After)["state"] == "open" {
			return false
		}
	}
	return true
}

func finishKey(key string) string { return "task-finish:" + hash([]byte(key)) }

// CompletionReady requires delivery AND semantic handling of every event. A
// waiting/decision report acknowledges receipt but cannot finish terminal work.
func (s *Store) CompletionReady(ctx context.Context, key, currentEvent string, terminal bool) (bool, error) {
	var pending int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ghraft_events e WHERE e.resource_key=? AND (e.state<>? OR (NOT EXISTS(SELECT 1 FROM ghraft_triage t WHERE t.event_id=e.event_id) AND NOT EXISTS(SELECT 1 FROM ghraft_results r WHERE r.event_id=e.event_id)))`, key, "accepted").Scan(&pending)
	if err != nil || pending != 0 {
		return false, err
	}
	if !terminal {
		return true, nil
	}
	var triaged int
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ghraft_triage WHERE event_id=?`, currentEvent).Scan(&triaged); err != nil || triaged > 0 {
		return triaged > 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM ghraft_results WHERE event_id=?`, currentEvent)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	ready := false
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return false, err
		}
		var result WorkResult
		if json.Unmarshal([]byte(body), &result) == nil && (result.Outcome == "investigated" || result.Outcome == "merged") {
			ready = true
		}
	}
	return ready, rows.Err()
}

type TaskFinish struct {
	Task   TaskDispatch `json:"task"`
	Status string       `json:"status"`
}

// FinishWork is invoked by the executor. Its update callback uses that
// executor's normal Raft CLI, preserving unread-message holds and identity.
// The shared external sender never impersonates the task owner to change status.
func (s *Service) FinishWork(ctx context.Context, in WorkInput, update func(context.Context, TaskDispatch, string) error) (TaskFinish, error) {
	var result TaskFinish
	if !s.Config.SharedTasks() || !agentHandleRE.MatchString(in.Executor) {
		return result, errors.New("finish-work requires shared delivery and your own --executor")
	}
	w, err := s.WorkContext(ctx, in.EventID)
	if err != nil {
		return result, err
	}
	root, err := s.fixedTask(ctx, w)
	if err != nil {
		return result, err
	}
	if in.TaskID != root.MessageID {
		return result, errors.New("finish-work must target the original fixed task")
	}
	state, err := s.Raft.readTask(ctx, root.Channel, root.Number, root.MessageID)
	if err != nil {
		return result, err
	}
	if !sameExecutor(state, in.Executor) || (state.Status != "in_progress" && state.Status != "in_review" && state.Status != "done") {
		return result, ErrClaimUnavailable
	}
	if w.Fork != nil && state.Status != "in_progress" {
		return result, ErrClaimUnavailable
	}
	w, err = s.RefreshWorkContext(ctx, in.EventID)
	if err != nil {
		return result, err
	}
	if w.CurrentEventID != in.EventID || w.CurrentVersion != in.EvidenceVersion || w.Guide.Version != in.GuideVersion {
		return result, ErrStaleWork
	}
	if state.Status == "done" && !githubEnded(w.Snapshot) {
		return result, ErrClaimUnavailable
	}
	ended := githubEnded(w.Snapshot)
	ready, err := s.Store.CompletionReady(ctx, w.Event.Key, in.EventID, ended)
	if err != nil {
		return result, err
	}
	if !ready {
		if w.Fork != nil {
			return result, errors.New("finish-work requires every event to be delivered and handled; inspect pending events for this maintenance work and explicitly record superseded when a current report covers an older event")
		}
		return result, errors.New("finish-work requires all delivered events to be handled and a final result; waiting is not completion")
	}
	if w.Fork != nil {
		// A batch result is not the lifecycle of continuous maintenance. Never
		// change its task status, including after a human pauses/stops it.
		state, err = s.Raft.readTask(ctx, root.Channel, root.Number, root.MessageID)
		if err != nil {
			return result, err
		}
		if !sameExecutor(state, in.Executor) || state.Status != "in_progress" {
			return result, ErrClaimUnavailable
		}
		return TaskFinish{Task: root, Status: "in_progress"}, nil
	}
	result = TaskFinish{Task: root, Status: "in_review"}
	if ended {
		result.Status = "done"
	}
	// This is an intent, not a completion receipt. It lets the collector recover
	// a concurrent reopen even if the executor crashes after the remote write.
	marker := ""
	if ended {
		marker = root.MessageID
	}
	if err = s.Store.PutMeta(ctx, finishKey(w.Event.Key), marker); err != nil {
		return result, err
	}
	state, err = s.Raft.readTask(ctx, root.Channel, root.Number, root.MessageID)
	if err != nil {
		return result, err
	}
	if !sameExecutor(state, in.Executor) || (state.Status != "in_progress" && state.Status != "in_review" && state.Status != result.Status) {
		return result, ErrClaimUnavailable
	}
	if state.Status != result.Status {
		writeErr := update(ctx, root, result.Status)
		state, err = s.Raft.readTask(ctx, root.Channel, root.Number, root.MessageID)
		if err != nil {
			return result, errors.Join(writeErr, err)
		}
		if !sameExecutor(state, in.Executor) || state.Status != result.Status {
			return result, errors.Join(writeErr, errors.New("task status was not confirmed; read the original discussion before retrying"))
		}
	}
	latest, err := s.WorkContext(ctx, in.EventID)
	if err != nil {
		return result, err
	}
	if result.Status == "done" && (!githubEnded(latest.Snapshot) || (latest.CurrentEventID != in.EventID && currentExplicitWork(ctx, s.Store, latest))) {
		// Restore active work if a reopen/request crossed the final write. If
		// this write is held or fails, the durable intent also triggers recovery.
		state, err := s.Raft.readTask(ctx, root.Channel, root.Number, root.MessageID)
		if err != nil || !sameExecutor(state, in.Executor) || state.Status != "done" {
			return result, errors.Join(ErrStaleWork, err)
		}
		return result, errors.Join(ErrStaleWork, update(ctx, root, "in_progress"))
	}
	return result, nil
}

func currentExplicitWork(ctx context.Context, store Persistence, w WorkContext) bool {
	ev, err := store.Event(ctx, w.CurrentEventID)
	return err != nil || explicitWork(ev)
}

func sameExecutor(state taskState, executor string) bool {
	return state.OwnerType != "user" && strings.TrimPrefix(state.Owner, "@") == strings.TrimPrefix(executor, "@")
}

// Quiet triage of post-completion updates does not grant execution authority.
func (s *Service) verifyDoneTriage(ctx context.Context, w WorkContext, executor string) error {
	if !agentHandleRE.MatchString(executor) || !quietDoneUpdate(w) {
		return ErrClaimUnavailable
	}
	root, err := s.fixedTask(ctx, w)
	if err != nil {
		return err
	}
	state, err := s.Raft.readTask(ctx, root.Channel, root.Number, root.MessageID)
	if err != nil {
		return err
	}
	if state.Status != "done" || !sameExecutor(state, executor) {
		return ErrClaimUnavailable
	}
	return nil
}

// Recover already handled terminal work after upgrade/restart, and reopen work
// that raced with an automatic finish. Requests reuse the existing discussion;
// their deterministic IDs prevent repeated wake-ups for the same transition.
func (s *Service) ReconcileTaskCompletion(ctx context.Context) error {
	if !s.Config.SharedTasks() {
		return nil
	}
	watches, err := s.Store.Watches(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, watch := range watches {
		key := watch.Key()
		if !s.watchConfigured(ctx, key) || !s.Config.SharedTasksFor(key) {
			continue
		}
		d, err := s.Store.Discussion(ctx, key)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if d.Task == nil {
			continue
		}
		identity, err := s.Raft.ExternalIdentity(ctx)
		if err != nil {
			return err
		}
		if d.Identity != identity.Agent.ID || d.Server != identity.Server.ID || d.Channel != s.Config.DeliveryTarget || !d.Task.valid() || d.Task.Channel != d.Channel {
			errs = append(errs, errors.New("completion discussion belongs to another configuration"))
			continue
		}
		body, version, err := s.Store.Snapshot(ctx, key)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		observed, err := s.Store.ObservedAt(ctx, key)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		ended := githubEnded(body)
		marker, err := s.Store.Meta(ctx, finishKey(key), "")
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// Open work only needs recovery after an interrupted automatic finish.
		if !ended && marker != d.Task.MessageID {
			continue
		}
		state, err := s.Raft.readTask(ctx, d.Task.Channel, d.Task.Number, d.Task.MessageID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			continue
		}
		if state.Owner == "" || state.OwnerType == "user" {
			continue
		}
		if state.Status != "in_review" && state.Status != "done" {
			continue
		}
		id, err := s.Store.CurrentEvent(ctx, key, version)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ready, err := s.Store.CompletionReady(ctx, key, id, ended)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// Unhandled/new events already provide the next wake-up. Do not add a
		// second request just because reopening and delivery crossed a finish.
		if !ready {
			continue
		}
		ev, err := s.Store.Event(ctx, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var request struct {
			Kind string `json:"kind"`
			ID   string `json:"request_id"`
		}
		if json.Unmarshal(ev.Payload, &request) == nil && request.Kind == "work.requested" && (strings.HasPrefix(request.ID, "finish:") || strings.HasPrefix(request.ID, "resume:")) {
			continue // The existing request already covers this transition.
		}
		reason, prefix := "", ""
		if ended && state.Status == "in_review" {
			reason, prefix = "GitHub PR/Issue 已结束。核对原讨论并保存本次收尾判断后，使用 finish-work 自动完成原任务，无需再次请求用户确认。", "finish:"
		} else if !ended && state.Status == "done" {
			reason, prefix = "GitHub 在任务收尾时重新打开。恢复原任务，读取最新状态后继续处理。", "resume:"
		}
		if reason != "" {
			if err := s.checkAccess(ctx, key); err != nil {
				if errors.Is(err, ErrGitHubRateLimited) {
					return errors.Join(append(errs, err)...)
				}
				if !errors.Is(err, ErrWorkDenied) {
					errs = append(errs, err)
				}
				continue
			}
			// Suspended terminal snapshots can be old after an upgrade. Refresh
			// only an eligible, already handled task that lacks a recovery request.
			// Existing requests and completed tasks never resume periodic scans.
			if observed < time.Now().Add(-10*time.Minute).Unix() {
				if err := s.collectWatch(ctx, watch); err != nil {
					errs = append(errs, err)
					if errors.Is(err, ErrGitHubRateLimited) {
						return errors.Join(errs...)
					}
					continue
				}
				_, current, err := s.Store.Snapshot(ctx, key)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				if current != version {
					continue
				} // Fresh changes provide their own wake-up.
			}
			if _, err := s.Store.RequestWork(ctx, key, version, prefix+id, reason); err != nil && !errors.Is(err, ErrStaleWork) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
