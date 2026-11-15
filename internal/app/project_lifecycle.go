package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github-raft-app/internal/database"
)

// WorkStatus describes the last recorded report, not a current permission or
// proof. Its event freezes the PR scope even if the batch is later replaced.
type ForkWorkStatus struct {
	Channel string     `json:"channel"`
	Batch   string     `json:"batch"`
	PR      int        `json:"pr"`
	Result  WorkResult `json:"result"`
}

type projectPRCompletion struct {
	Resource string         `json:"resource"`
	Channel  string         `json:"channel"`
	Batch    string         `json:"batch"`
	PR       int            `json:"pr"`
	ReportID string         `json:"report_id"`
	Done     bool           `json:"done"`
	PRTask   *projectPRTask `json:"-"`
}

func prCompletionKey(c projectPRCompletion) string {
	return "project-pr-finish:" + hash([]byte(fmt.Sprintf("%s:%s:%d", c.Resource, c.Channel, c.PR)))
}

// Commit the report, follow-up event and completion intent together. A failed
// remote task update must not require recording the merge or running tests again.
func saveProjectWorkStatus(ctx context.Context, tx *database.Tx, key string, p ForkProgress, result WorkResult) error {
	status := p.WorkStatus
	if status == nil || status.PR <= 0 || !slices.Contains([]string{"waiting", "needs-decision", "changes-ready", "merged"}, result.Outcome) {
		return nil
	}
	if status.Batch != collaborationID(&p) || status.PR != p.PR || string(canonical(status.Result)) != string(canonical(result)) {
		return errors.New("project work status does not match the recorded result")
	}
	var body string
	if err := tx.QueryRowContext(ctx, "SELECT body FROM ghraft_snapshots WHERE resource_key=?", key).Scan(&body); err != nil {
		return err
	}
	var snapshot Snapshot
	if json.Unmarshal([]byte(body), &snapshot) != nil {
		return errors.New("invalid project snapshot")
	}
	// Reusing an unanswered decision report must not send another question or
	// move current-event backwards to its already delivered status event.
	id := hash(canonical([]any{"fork-result", key, status.Batch, status.PR, result.ReportID, result.Outcome, result.ReviewOnly, result.Summary}))
	payload := canonical(map[string]any{"event_id": id, "resource_key": key, "version": result.EvidenceVersion, "data_classification": snapshot.Classification, "subject": subjectOf(snapshot), "kind": "fork.result", "fork_batch": status.Batch, "fork_pr": status.PR, "collaboration_id": status.Batch, "origin_task": result.TaskID, "work_status": status, "changes": []Change{}})
	var existing string
	if err := tx.QueryRowContext(ctx, "SELECT event_id FROM ghraft_events WHERE event_id=?", id).Scan(&existing); errors.Is(err, ErrNotFound) {
		if err = tx.Upsert(ctx, &eventRecord{EventID: id, ResourceKey: key, VersionHash: result.EvidenceVersion, Payload: string(payload), State: "pending", CreatedAt: time.Now().Unix()}, []string{"event_id"}, nil); err != nil {
			return err
		}
		if err = tx.Upsert(ctx, &metaRecord{"current-event:" + key, id}, []string{"name"}, []string{"value"}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if result.Outcome == "merged" && !result.ReviewOnly && p.Phase == "synced" {
		c := projectPRCompletion{Resource: key, Channel: status.Channel, Batch: status.Batch, PR: status.PR, ReportID: result.ReportID}
		return tx.Upsert(ctx, &metaRecord{prCompletionKey(c), string(canonical(c))}, []string{"name"}, nil)
	}
	return nil
}

func (s *Store) PendingPRCompletions(ctx context.Context, key string) ([]projectPRCompletion, error) {
	var records []metaRecord
	if err := s.db.Session(ctx).Where("name LIKE ?", "project-pr-finish:%").Find(&records).Error; err != nil {
		return nil, err
	}
	var pending []projectPRCompletion
	for _, record := range records {
		var c projectPRCompletion
		if json.Unmarshal([]byte(record.Value), &c) != nil {
			return nil, errors.New("invalid PR completion intent")
		}
		if c.Resource != key || c.Done {
			continue
		}
		var raw string
		err := s.db.QueryRowContext(ctx, "SELECT value FROM ghraft_meta WHERE name=?", projectPRTaskKey(key, c.Channel, c.PR)).Scan(&raw)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if err == nil {
			if json.Unmarshal([]byte(raw), &c.PRTask) != nil || c.PRTask == nil {
				return nil, errors.New("invalid PR task receipt")
			}
		}
		pending = append(pending, c)
	}
	return pending, nil
}

func (s *Store) ConfirmPRCompletion(ctx context.Context, completed projectPRCompletion) error {
	tx, err := s.db.BeginWrite(ctx, "resource:"+completed.Resource)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	if err = tx.QueryRowContext(ctx, "SELECT value FROM ghraft_meta WHERE name=?", prCompletionKey(completed)).Scan(&raw); err != nil {
		return err
	}
	var saved projectPRCompletion
	if json.Unmarshal([]byte(raw), &saved) != nil {
		return errors.New("invalid PR completion intent")
	}
	saved.Done, completed.Done = false, false
	if string(canonical(saved)) != string(canonical(completed)) {
		return ErrStaleWork
	}
	saved.Done = true
	if err = tx.Upsert(ctx, &metaRecord{prCompletionKey(saved), string(canonical(saved))}, []string{"name"}, []string{"value"}); err != nil {
		return err
	}
	return tx.Commit()
}

// CompleteMergedPRTasks is an executor action. Only the live project
// coordinator can close automatically generated PR tasks backed by a recorded,
// verified merge. The collector and participant reports cannot grant this power.
// Archived batches remain finishable through their immutable PR task receipt.
func (s *Service) CompleteMergedPRTasks(ctx context.Context, in WorkInput, update func(context.Context, TaskDispatch, string) error) ([]TaskFinish, error) {
	w, err := s.WorkContext(ctx, in.EventID)
	if err != nil {
		return nil, err
	}
	if !s.Config.Project(w.Event.Key) {
		return nil, nil
	}
	root, err := s.fixedTask(ctx, w)
	if err != nil {
		return nil, err
	}
	if in.TaskID != root.MessageID || !agentHandleRE.MatchString(in.Executor) {
		return nil, errors.New("PR completion requires the current project coordinator; the merge report remains saved")
	}
	pending, err := s.Store.PendingPRCompletions(ctx, w.Event.Key)
	if err != nil {
		return nil, err
	}
	identity, err := s.Raft.ExternalIdentity(ctx)
	if err != nil {
		return nil, err
	}
	var finished []TaskFinish
	for _, c := range pending {
		if err = s.Raft.verifyTaskOwner(ctx, root.Channel, root.Number, root.MessageID, in.Executor); err != nil {
			return finished, err
		}
		r := c.PRTask
		if c.Channel != root.Channel || r == nil || r.Task == nil || !r.Task.valid() || r.Resource != c.Resource || r.Batch != c.Batch || r.PR != c.PR || r.Channel != c.Channel || r.Task.Channel != c.Channel || r.Identity != identity.Agent.ID || r.Server != identity.Server.ID || r.Task.MessageID == root.MessageID {
			return finished, errors.New("PR completion has no matching task receipt in the current project; inspect pending creation or routing")
		}
		check := func() (taskState, error) {
			collab, err := s.Store.ForkCollaboration(ctx, c.Resource, c.Batch, c.Channel)
			if err != nil {
				return taskState{}, err
			}
			item := collab.item(r.Task.MessageID)
			if item == nil || item.Revoked || !item.PRDiscussion || item.PR != c.PR || item.Task != *r.Task {
				return taskState{}, ErrWorkDenied
			}
			return s.Raft.readTask(ctx, r.Task.Channel, r.Task.Number, r.Task.MessageID)
		}
		state, err := check()
		if err != nil {
			return finished, err
		}
		if state.Status != "done" {
			if state.OwnerType == "user" || !slices.Contains([]string{"in_progress", "in_review"}, state.Status) {
				return finished, errors.New("PR task is paused, unclaimed or human-owned; inspect the discussion before completing it")
			}
			before := state
			if err = s.Raft.verifyTaskOwner(ctx, root.Channel, root.Number, root.MessageID, in.Executor); err != nil {
				return finished, err
			}
			state, err = check()
			if err != nil || state != before {
				return finished, errors.Join(err, ErrClaimUnavailable)
			}
			writeErr := update(ctx, *r.Task, "done")
			state, err = check()
			if err != nil || state.Status != "done" || state.Owner != before.Owner || state.OwnerType != before.OwnerType {
				return finished, errors.Join(writeErr, err, errors.New("PR task completion was not confirmed; retry finish-work as coordinator after reading the discussion"))
			}
		}
		if err = s.Store.ConfirmPRCompletion(ctx, c); err != nil {
			return finished, err
		}
		finished = append(finished, TaskFinish{Task: *r.Task, Status: "done"})
	}
	return finished, nil
}

func projectStatusMessage(status ForkWorkStatus) string {
	r := status.Result
	state := map[string]string{"waiting": "等待检查或外部条件，当前报告不允许后续批次继续。", "needs-decision": "等待用户决定，当前报告阻止后续批次继续。", "changes-ready": "准备已完成；后续批次仍须满足全部前序批次的验证、检查和容量要求。", "merged": "合入结果已核验并登记；协调执行者将收尾本 PR 任务，项目协调任务继续保留。"}[r.Outcome]
	if r.ReviewOnly {
		state = "仅等待人工审阅或合入；后续批次仍须满足全部前序批次的验证、检查和容量要求。"
	}
	if state == "" {
		return ""
	}
	// These are recorded facts, not a fresh claim or instructions embedded in
	// the summary. Link the report without repeatedly waking its human addressee.
	return fmt.Sprintf("\n\n已登记状态：%s\n负责续接：%s\n报告：%s msg=%s\n报告摘要：\n%s\n以上为登记时状态；执行前仍须读取当前上下文和报告中的决定。", state, strings.TrimPrefix(r.Executor, "@"), status.Channel, r.ReportID, strings.ReplaceAll(r.Summary, "@", "＠"))
}
