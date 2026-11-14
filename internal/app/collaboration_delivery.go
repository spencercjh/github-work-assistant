package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type participantDelivery struct {
	Identity  string       `json:"identity"`
	CreatedAt int64        `json:"created_at"`
	Task      TaskDispatch `json:"task"`
	Request   struct {
		Target   string `json:"target"`
		Content  string `json:"content"`
		Key      string `json:"idempotencyKey"`
		Continue bool   `json:"continueAnyway"`
		Draft    bool   `json:"sendDraft"`
	} `json:"request"`
	Receipt json.RawMessage `json:"receipt,omitempty"`
}

type participantRecipient struct {
	Task         TaskDispatch `json:"task"`
	Kind         string       `json:"kind"`
	PR           int          `json:"pr,omitempty"`
	HideMetadata bool         `json:"hide_metadata,omitempty"`
	PRDiscussion bool         `json:"pr_discussion,omitempty"`
}

// Freeze the recipient set before any network write. A retry uses that plan
// even if the queue advances, another task is registered, or the process exits.
func (r *Raft) projectParticipantPlan(ctx context.Context, ev Event) ([]participantRecipient, error) {
	if !r.Config.Project(ev.Key) {
		return nil, nil
	}
	p, err := r.Store.ForkProgress(ctx, ev.Key)
	if err != nil {
		return nil, err
	}
	var payload struct {
		CollaborationID string   `json:"collaboration_id"`
		OriginTask      string   `json:"origin_task"`
		Classification  string   `json:"data_classification"`
		ForkBatch       string   `json:"fork_batch"`
		ForkPR          int      `json:"fork_pr"`
		Changes         []Change `json:"changes"`
	}
	if err = json.Unmarshal(ev.Payload, &payload); err != nil {
		return nil, err
	}
	batches := []ForkProgress{p}
	if p.Queue != nil {
		batches = nil
		for _, batch := range p.Queue.Batches {
			batches = append(batches, batch.Progress)
		}
		// A merged batch can leave the active queue in the same observation.
		// Its last update still belongs in that PR's existing discussion.
		for _, change := range payload.Changes {
			if id, ok := strings.CutPrefix(change.Object, "queue:"); ok && idRE.MatchString(id) && queueIndex(p, id) < 0 {
				before := obj(change.Before)
				number, _ := before["pr"].(float64)
				batches = append(batches, ForkProgress{BatchID: id, PR: int(number)})
			}
		}
	} else {
		for _, change := range payload.Changes {
			if change.Object == "fork" {
				before := obj(change.Before)
				if round := str(before["round"]); idRE.MatchString(round) && round != p.Round {
					number, _ := before["pr"].(float64)
					batches = append(batches, ForkProgress{Round: round, PR: int(number)})
				}
			}
		}
	}
	if idRE.MatchString(payload.ForkBatch) {
		found := false
		for _, batch := range batches {
			found = found || collaborationID(&batch) == payload.ForkBatch
		}
		if !found {
			batches = append(batches, ForkProgress{BatchID: payload.ForkBatch, PR: payload.ForkPR})
		}
	}
	recipients := []participantRecipient{}
	for _, batch := range batches {
		id := collaborationID(&batch)
		if !idRE.MatchString(id) {
			continue
		}
		affected := p.Queue == nil || len(payload.Changes) == 0 && id == payload.ForkBatch
		if payload.CollaborationID != "" {
			affected = payload.CollaborationID == id
		} else {
			for _, change := range payload.Changes {
				if change.Object == "queue:"+id {
					affected = true
				}
			}
		}
		if !affected {
			continue
		}
		c, err := r.Store.ForkCollaboration(ctx, ev.Key, id, r.Config.DeliveryTargetFor(ev.Key))
		if err != nil {
			return nil, err
		}
		for _, item := range c.Items {
			if item.Revoked || item.Kind == "coordinate" || item.Task.MessageID == payload.OriginTask {
				continue
			}
			pr := batch.PR
			if item.PRDiscussion {
				// Use the PR observed by this event, never the current PR occupying
				// the batch. A queued old event must not reach a replacement PR.
				matches, hasChange := false, false
				for _, change := range payload.Changes {
					if change.Object != "queue:"+id && change.Object != "fork" {
						continue
					}
					for _, value := range []any{change.Before, change.After} {
						facts := obj(value)
						if change.Object == "fork" && str(facts["round"]) != id && str(facts["batch_id"]) != id {
							continue
						}
						number, present := facts["pr"].(float64)
						hasChange = hasChange || present
						matches = matches || present && int(number) == item.PR
					}
				}
				if !hasChange {
					matches = payload.ForkBatch == id && payload.ForkPR == item.PR
				}
				if item.PR <= 0 || !matches {
					continue
				}
				pr = item.PR
			}
			recipients = append(recipients, participantRecipient{item.Task, item.Kind, pr, payload.Classification == "private" && !r.Config.PrivateNotificationDetails, item.PRDiscussion})
		}
	}
	return r.Store.PrepareParticipantPlan(ctx, ev.ID, recipients)
}

// Accepting the source event requires every planned recipient's durable
// receipt. Closed or reassigned tasks retain their history but gain no grant.
func (r *Raft) sendProjectParticipants(ctx context.Context, ev Event) error {
	recipients, err := r.projectParticipantPlan(ctx, ev)
	if err != nil {
		return err
	}
	for _, recipient := range recipients {
		if err := r.sendParticipantUpdate(ctx, ev, ForkProgress{PR: recipient.PR}, ForkWorkItem{Task: recipient.Task, Kind: recipient.Kind, PRDiscussion: recipient.PRDiscussion}, recipient.HideMetadata); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) PrepareParticipantPlan(ctx context.Context, eventID string, proposed []participantRecipient) ([]participantRecipient, error) {
	tx, _, err := s.eventWrite(ctx, eventID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var state string
	if err = tx.QueryRowContext(ctx, "SELECT state FROM ghraft_events WHERE event_id=?", eventID).Scan(&state); err != nil {
		return nil, err
	}
	if state == "accepted" {
		return nil, nil
	}
	name := "participant-plan:" + eventID
	var value string
	err = tx.QueryRowContext(ctx, "SELECT value FROM ghraft_meta WHERE name=?", name).Scan(&value)
	if errors.Is(err, ErrNotFound) {
		if err = tx.Upsert(ctx, &metaRecord{name, string(canonical(proposed))}, []string{"name"}, nil); err != nil {
			return nil, err
		}
		return proposed, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	var saved []participantRecipient
	if json.Unmarshal([]byte(value), &saved) != nil {
		return nil, errors.New("invalid stored participant plan")
	}
	return saved, tx.Commit()
}

func (r *Raft) sendParticipantUpdate(ctx context.Context, ev Event, p ForkProgress, item ForkWorkItem, hideMetadata bool) error {
	if !item.Task.valid() || item.Task.Channel != r.Config.DeliveryTargetFor(ev.Key) {
		return errors.New("participant destination differs from the project channel")
	}
	// Explicit revocation cancels a pending participant notification, including
	// an ambiguous old send. A revoked task cannot regain authority by retrying.
	scope, err := r.Store.CollaborationScope(ctx, item.Task.Channel, item.Task.MessageID)
	if err != nil {
		return err
	}
	prefix := ev.Key + ":"
	if !strings.HasPrefix(scope, prefix) {
		return errors.New("participant scope is missing or belongs to another project")
	}
	current, err := r.Store.ForkCollaboration(ctx, ev.Key, strings.TrimPrefix(scope, prefix), item.Task.Channel)
	if err != nil {
		return err
	}
	registered := current.item(item.Task.MessageID)
	if registered == nil {
		return errors.New("participant registration is missing")
	}
	if registered.Revoked {
		return nil
	}
	state, err := r.readTask(ctx, item.Task.Channel, item.Task.Number, item.Task.MessageID)
	if err != nil {
		return err
	}
	owner := ""
	if state.Owner != "" {
		owner = "@" + strings.TrimPrefix(state.Owner, "@")
		if !agentHandleRE.MatchString(owner) {
			return errors.New("invalid participant owner")
		}
	}
	subject := "同步批次"
	if p.PR > 0 && !hideMetadata {
		subject = fmt.Sprintf("同步 PR %d", p.PR)
	}
	d := participantDelivery{Identity: r.Config.ExternalAgent, CreatedAt: time.Now().Unix(), Task: item.Task}
	d.Request.Target, d.Request.Key = item.Task.target(), "github-participant:"+ev.ID+":"+item.Task.MessageID
	d.Request.Continue, d.Request.Draft = true, true
	d.Request.Content = fmt.Sprintf("%s · %s 工作项有更新。\n\n%s 请读取本 thread 和 work-context.collaboration，按本任务的实时归属处理；先运行 verify-claim --event %s --task %s --executor <自己的名称>。只做本工作项范围内的操作。已有结果与当前提交一致时不要重复执行；需要更新结果时，在本 thread 发布报告后使用 record-contribution。其他工作项及项目协调任务保持原归属。\n\n事件 ID：%s\n证据版本：%s", subject, item.Kind, owner, ev.ID, item.Task.MessageID, ev.ID, ev.Version)
	if item.PRDiscussion {
		d.Request.Content = fmt.Sprintf("%s 有更新。\n\n%s 请在本 PR 的固定讨论区核对提交、CI 及评审变化。用户邀请的其他 agent 可以直接在这里 review 和回复，不接管项目同步任务；正式评审结果仍须使用实际任务归属和当前提交证明。PR 已结束时保留原任务记录，不自动重开或另建任务。\n\n事件 ID：%s\n证据版本：%s\n任务：%s", subject, owner, ev.ID, ev.Version, item.Task.MessageID)
	}
	if state.Status == "closed" || state.Status == "done" || state.OwnerType == "user" {
		d.Request.Content += "\n本任务当前已结束或由用户负责，此通知不恢复任务状态或授予执行权限。"
	}
	saved, err := r.Store.ParticipantDelivery(ctx, ev.ID, item.Task.MessageID, d, false)
	if err != nil {
		return err
	}
	if len(saved.Receipt) > 0 {
		return nil
	}
	if saved.Identity != d.Identity || saved.Task != d.Task || saved.Request.Target != d.Request.Target || saved.Request.Key != d.Request.Key {
		return errors.New("pending participant notification belongs to a different identity or thread")
	}
	if saved.CreatedAt <= 0 || time.Since(time.Unix(saved.CreatedAt, 0)) >= 23*time.Hour {
		return errors.New("participant notification exceeded the safe retry window; reconcile the original request")
	}
	raw, _, err := r.request(ctx, "/internal/agent-api/send", saved.Request, r.Credential, false)
	if err != nil {
		return err
	}
	var receipt struct {
		OK        bool   `json:"ok"`
		State     string `json:"state"`
		MessageID string `json:"messageId"`
	}
	if json.Unmarshal(raw, &receipt) != nil || !receipt.OK || (receipt.State != "sent" && receipt.State != "committed") || !uuidRE.MatchString(receipt.MessageID) {
		return errors.New("participant notification was not confirmed")
	}
	saved.Receipt = canonical(receipt)
	_, err = r.Store.ParticipantDelivery(ctx, ev.ID, item.Task.MessageID, saved, true)
	return err
}

func (s *Store) ParticipantDelivery(ctx context.Context, eventID, taskID string, proposed participantDelivery, confirm bool) (participantDelivery, error) {
	tx, _, err := s.eventWrite(ctx, eventID)
	if err != nil {
		return participantDelivery{}, err
	}
	defer tx.Rollback()
	var state string
	if err = tx.QueryRowContext(ctx, "SELECT state FROM ghraft_events WHERE event_id=?", eventID).Scan(&state); err != nil {
		return participantDelivery{}, err
	}
	if state == "accepted" {
		return participantDelivery{Receipt: json.RawMessage(`{}`)}, nil
	}
	name := "participant:" + eventID + ":" + taskID
	if !idRE.MatchString(eventID) || !uuidRE.MatchString(taskID) {
		return participantDelivery{}, errors.New("invalid participant outbox identity")
	}
	var value string
	err = tx.QueryRowContext(ctx, "SELECT value FROM ghraft_meta WHERE name=?", name).Scan(&value)
	if errors.Is(err, ErrNotFound) {
		if confirm {
			return participantDelivery{}, errors.New("participant request was not prepared")
		}
		if err = tx.Upsert(ctx, &metaRecord{name, string(canonical(proposed))}, []string{"name"}, nil); err != nil {
			return participantDelivery{}, err
		}
		return proposed, tx.Commit()
	}
	if err != nil {
		return participantDelivery{}, err
	}
	var saved participantDelivery
	if json.Unmarshal([]byte(value), &saved) != nil {
		return saved, errors.New("invalid participant request")
	}
	if confirm {
		before, after := saved, proposed
		before.Receipt = nil
		after.Receipt = nil
		if string(canonical(before)) != string(canonical(after)) || !json.Valid(proposed.Receipt) {
			return saved, errors.New("participant request changed during delivery")
		}
		if len(saved.Receipt) > 0 && string(saved.Receipt) != string(proposed.Receipt) {
			return saved, errors.New("participant receipt changed")
		}
		saved = proposed
		if err = tx.Upsert(ctx, &metaRecord{name, string(canonical(saved))}, []string{"name"}, []string{"value"}); err != nil {
			return saved, err
		}
	}
	return saved, tx.Commit()
}
