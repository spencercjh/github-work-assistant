package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github-raft-app/internal/database"
)

// The project coordinator advances the queue. Each published PR has a separate
// unassigned review task, so inviting a reviewer never transfers the writer's
// task. Raft owns assignment; this record only fixes the PR's discussion.
type projectPRTask struct {
	Resource  string        `json:"resource"`
	Identity  string        `json:"identity"`
	Creator   string        `json:"creator"`
	Server    string        `json:"server"`
	Channel   string        `json:"channel"`
	Batch     string        `json:"batch"`
	PR        int           `json:"pr"`
	CreatedAt int64         `json:"created_at"`
	Request   prTaskRequest `json:"request"`
	Task      *TaskDispatch `json:"task,omitempty"`
}

type prTaskRequest struct {
	Channel string              `json:"channel"`
	Tasks   []map[string]string `json:"tasks"`
	Key     string              `json:"idempotencyKey"`
}

func projectPRTaskKey(key, channel string, pr int) string {
	return "project-pr:" + hash([]byte(fmt.Sprintf("%s:%s:%d", key, channel, pr)))
}

func snapshotPRScope(snapshot Snapshot) (string, any) {
	facts := snapshot.Objects["fork"]
	id := str(facts["batch_id"])
	if id == "" {
		id = str(facts["round"])
	}
	return id, facts["pr"]
}

// A persistent request, independent of the event that first observed the PR,
// prevents two polls or a lost response from creating two tasks for one PR.
func (s *Store) ProjectPRTask(ctx context.Context, key string, next projectPRTask, confirm bool) (projectPRTask, error) {
	tx, err := s.db.BeginWrite(ctx, "resource:"+key)
	if err != nil {
		return projectPRTask{}, err
	}
	defer tx.Rollback()
	name := projectPRTaskKey(key, next.Channel, next.PR)
	if !confirm {
		var deliveries []metaRecord
		if err = tx.Session(ctx).Raw(`SELECT m.name,m.value FROM ghraft_meta m JOIN ghraft_events e ON SUBSTR(m.name,19)=e.event_id WHERE m.name LIKE ? AND e.resource_key=? AND e.state=?`, "external-delivery:%", key, "pending").Scan(&deliveries).Error; err != nil {
			return projectPRTask{}, err
		}
		for _, record := range deliveries {
			var envelope struct {
				Identity string
				Request  struct{ Channel, Target string }
			}
			if json.Unmarshal([]byte(record.Value), &envelope) != nil {
				return projectPRTask{}, errors.New("invalid pending project delivery")
			}
			channel := envelope.Request.Channel
			if channel == "" {
				channel, _, _ = strings.Cut(envelope.Request.Target, ":")
			}
			if channel != next.Channel || envelope.Identity != next.Identity {
				return projectPRTask{}, errors.New("unresolved project delivery in the previous channel or identity; reconcile it before creating PR tasks")
			}
		}
		var requests []metaRecord
		if err = tx.Session(ctx).Where("name LIKE ?", "project-pr:%").Find(&requests).Error; err != nil {
			return projectPRTask{}, err
		}
		for _, record := range requests {
			var pending projectPRTask
			if json.Unmarshal([]byte(record.Value), &pending) != nil {
				return projectPRTask{}, errors.New("invalid stored PR task")
			}
			if pending.Resource == key && pending.Task == nil && pending.Channel != next.Channel {
				return projectPRTask{}, errors.New("unresolved PR task creation in the previous channel; reconcile it before changing routing")
			}
		}
	}
	var value string
	err = tx.QueryRowContext(ctx, "SELECT value FROM ghraft_meta WHERE name=?", name).Scan(&value)
	if errors.Is(err, ErrNotFound) && !confirm {
		if err = tx.Upsert(ctx, &metaRecord{name, string(canonical(next))}, []string{"name"}, nil); err != nil {
			return projectPRTask{}, err
		}
		return next, tx.Commit()
	}
	if err != nil {
		return projectPRTask{}, err
	}
	var saved projectPRTask
	if json.Unmarshal([]byte(value), &saved) != nil || saved.Resource != key || saved.Identity != next.Identity || saved.Server != next.Server || saved.Channel != next.Channel || saved.Batch != next.Batch || saved.PR != next.PR {
		return projectPRTask{}, errors.New("PR task belongs to another sender, channel or batch; inspect the original task")
	}
	if confirm {
		before, after := saved, next
		before.Task, after.Task = nil, nil
		if string(canonical(before)) != string(canonical(after)) || next.Task == nil || !next.Task.valid() || next.Task.Channel != saved.Channel || saved.Task != nil && *saved.Task != *next.Task {
			return projectPRTask{}, errors.New("PR task request or receipt changed during delivery")
		}
		saved = next
		if err = tx.Upsert(ctx, &metaRecord{name, string(canonical(saved))}, []string{"name"}, []string{"value"}); err != nil {
			return projectPRTask{}, err
		}
	}
	if saved.Task != nil {
		if err = attachProjectPRTask(ctx, tx, saved); err != nil {
			return projectPRTask{}, err
		}
	}
	return saved, tx.Commit()
}

// Receipt and task binding commit together, including recovery after closure
// or archival. The PR number fences that historical task from a replacement PR
// that happens to reuse the same batch identity.
func attachProjectPRTask(ctx context.Context, tx *database.Tx, saved projectPRTask) error {
	name := collaborationKey(saved.Resource, saved.Batch, saved.Channel)
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT value FROM ghraft_meta WHERE name=?", name).Scan(&raw)
	c := ForkCollaboration{ID: saved.Batch, Channel: saved.Channel}
	if err == nil {
		if json.Unmarshal([]byte(raw), &c) != nil || c.ID != saved.Batch || c.Channel != saved.Channel {
			return errors.New("invalid PR collaboration binding")
		}
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	if item := c.item(saved.Task.MessageID); item != nil {
		if !item.PRDiscussion || item.PR != saved.PR {
			return errors.New("PR task already belongs to another work item")
		}
		return nil // Revocation remains effective; never create a replacement.
	}
	if len(c.Items) >= 32 {
		return errors.New("PR task cannot be linked: collaboration has reached its 32-item limit")
	}
	binding := "collaboration-task:" + hash([]byte(saved.Channel+":"+saved.Task.MessageID))
	if err = tx.Upsert(ctx, &metaRecord{binding, saved.Resource + ":" + saved.Batch}, []string{"name"}, nil); err != nil {
		return err
	}
	var scope string
	if err = tx.QueryRowContext(ctx, "SELECT value FROM ghraft_meta WHERE name=?", binding).Scan(&scope); err != nil {
		return err
	}
	if scope != saved.Resource+":"+saved.Batch {
		return errors.New("PR task belongs to another project or batch")
	}
	c.Items = append(c.Items, ForkWorkItem{Task: *saved.Task, Kind: "review", Registered: saved.Creator, PRDiscussion: true, PR: saved.PR})
	c.Revision++
	return tx.Upsert(ctx, &metaRecord{name, string(canonical(c))}, []string{"name"}, []string{"value"})
}

func (s *Store) PendingProjectPRTasks(ctx context.Context, key string) ([]projectPRTask, error) {
	var records []metaRecord
	if err := s.db.Session(ctx).Where("name LIKE ?", "project-pr:%").Find(&records).Error; err != nil {
		return nil, err
	}
	var pending []projectPRTask
	for _, record := range records {
		var saved projectPRTask
		if json.Unmarshal([]byte(record.Value), &saved) != nil {
			return nil, errors.New("invalid stored PR task request")
		}
		if saved.Resource == key && saved.Task == nil {
			pending = append(pending, saved)
		}
	}
	return pending, nil
}

func (r *Raft) finishProjectPRTask(ctx context.Context, saved projectPRTask, identity ExternalIdentity) error {
	if saved.Identity != identity.Agent.ID || saved.Server != identity.Server.ID || saved.Channel != r.Config.DeliveryTargetFor(saved.Resource) {
		return errors.New("unresolved PR task creation in the previous channel or identity; restore its routing before retrying")
	}
	if saved.CreatedAt <= 0 || time.Since(time.Unix(saved.CreatedAt, 0)) >= 23*time.Hour {
		return errors.New("PR task creation exceeded the safe retry window; inspect the original request")
	}
	if err := r.checkTaskChannel(ctx, saved.Channel); err != nil {
		return err
	}
	raw, _, err := r.request(ctx, "/internal/agent-api/tasks", saved.Request, r.Credential, false)
	if err != nil {
		return err
	}
	var result struct {
		Tasks []TaskDispatch `json:"tasks"`
	}
	if json.Unmarshal(raw, &result) != nil || len(result.Tasks) != 1 {
		return errors.New("Raft did not confirm one PR task")
	}
	result.Tasks[0].Channel = saved.Channel
	saved.Task = &result.Tasks[0]
	_, err = r.Store.ProjectPRTask(ctx, saved.Resource, saved, true)
	return err
}

func (r *Raft) ensureProjectPRTasks(ctx context.Context, ev Event) error {
	if !r.Config.Project(ev.Key) {
		return nil
	}
	current, err := r.Store.ReadWork(ctx, ev.ID)
	if err != nil || current.Event.State == "accepted" {
		return err
	}
	p, err := r.Store.ForkProgress(ctx, ev.Key)
	if err != nil {
		return err
	}
	identity, err := r.ExternalIdentity(ctx)
	if err != nil {
		return err
	}
	if discussion, e := r.Store.Discussion(ctx, r.Config.discussionResource(ev.Key)); e == nil {
		if discussion.Identity != identity.Agent.ID || discussion.Server != identity.Server.ID {
			return errors.New("project discussion belongs to another sender or server")
		}
	} else if !errors.Is(e, ErrNotFound) {
		return e
	}
	pending, err := r.Store.PendingProjectPRTasks(ctx, ev.Key)
	if err != nil {
		return err
	}
	for _, saved := range pending {
		if err = r.finishProjectPRTask(ctx, saved, identity); err != nil {
			return err
		}
	}
	var snapshot Snapshot
	if err = json.Unmarshal(current.Snapshot, &snapshot); err != nil {
		return err
	}
	var payload struct {
		Classification string `json:"data_classification"`
	}
	if err = json.Unmarshal(ev.Payload, &payload); err != nil {
		return err
	}
	batches := []ForkQueuedBatch{{Progress: p, Observation: ForkObservation{Classification: snapshot.Classification, Objects: snapshot.Objects}}}
	if p.Queue != nil {
		batches = p.Queue.Batches
	}
	for _, batch := range batches {
		b := batch.Progress
		// Do not resurrect completed historical PRs when enabling this feature.
		if b.PR <= 0 || b.MergedAt > 0 || b.Phase == "synced" || batch.Observation.Objects["pull"]["state"] == "closed" {
			continue
		}
		if !idRE.MatchString(collaborationID(&b)) {
			return errors.New("published PR has no stable batch identity")
		}
		spec, _ := r.Config.Fork(ev.Key)
		channel := r.Config.DeliveryTargetFor(ev.Key)
		subject := fmt.Sprintf("同步 PR %d · %s", b.PR, spec.Target.Repo)
		body := fmt.Sprintf("[查看同步 PR](https://github.com/%s/pull/%d)", spec.Target.Repo, b.PR)
		if (snapshot.Classification == "private" || payload.Classification == "private" || batch.Observation.Classification == "private") && !r.Config.PrivateNotificationDetails {
			subject, body = "私有同步 PR", "PR 详情请从本机工作上下文读取。"
		}
		body += fmt.Sprintf("\n\n这是本 PR 的固定任务和讨论区，提交、CI 和协作更新会回到这里。用户可以在本 thread 邀请其他 agent 做 review，受邀者直接在这里讨论，不接管项目同步任务。需要保存正式评审结果时，由本任务的实际负责人认领后，使用自己的 --task、--executor 运行 verify-claim、work-context 和 link-task，再 record-contribution；其他评审者可由协调者关联独立工作项。此任务只授予评审工作项范围，不授予同步写入或合并权限，原作者不能自审。\n\nPR 结束后保留本任务与讨论，不为同一 PR 另建任务，也不自动恢复已完成任务。项目原有暂停安排继续生效。\n\n事件 ID：%s\n证据版本：%s\n批次：%s", ev.ID, ev.Version, collaborationID(&b))
		next := projectPRTask{Resource: ev.Key, Identity: identity.Agent.ID, Creator: "@" + identity.Agent.Name, Server: identity.Server.ID, Channel: channel, Batch: collaborationID(&b), PR: b.PR, CreatedAt: time.Now().Unix()}
		next.Request = prTaskRequest{Channel: channel, Tasks: []map[string]string{{"title": subject, "description": body}}, Key: projectPRTaskKey(ev.Key, channel, b.PR)}
		saved, err := r.Store.ProjectPRTask(ctx, ev.Key, next, false)
		if err != nil {
			return err
		}
		if saved.Task == nil {
			if err = r.finishProjectPRTask(ctx, saved, identity); err != nil {
				return err
			}
		}
	}
	return nil
}
