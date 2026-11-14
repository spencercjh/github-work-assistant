package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func collaborationKey(resource, id, channel string) string {
	return "collaboration:" + hash([]byte(resource+":"+id+":"+channel))
}

// Stored references deliberately contain no discussion or report bodies.
// Full reports use the ordinary immutable report store and retention policy.
func (s *Store) ForkCollaboration(ctx context.Context, key, id, channel string) (ForkCollaboration, error) {
	var value string
	// The channel is part of the stored scope, supplied by the service through
	// the ID namespace. This keeps a route change from inheriting old authority.
	name := collaborationKey(key, id, channel)
	err := s.db.QueryRowContext(ctx, "SELECT value FROM ghraft_meta WHERE name=?", name).Scan(&value)
	c := ForkCollaboration{ID: id, Items: []ForkWorkItem{}}
	if errors.Is(err, ErrNotFound) {
		return c, nil
	}
	if err == nil && json.Unmarshal([]byte(value), &c) != nil {
		err = errors.New("invalid stored collaboration")
	}
	return c, err
}

func (s *Store) SaveForkCollaboration(ctx context.Context, key, round, version string, revision int64, next ForkCollaboration, report *ContributionInput) error {
	tx, err := s.db.BeginWrite(ctx, "resource:"+key)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := readForkProgress(ctx, tx, key)
	if err != nil {
		return err
	}
	var current string
	if err = tx.QueryRowContext(ctx, "SELECT version_hash FROM ghraft_snapshots WHERE resource_key=?", key).Scan(&current); err != nil {
		return err
	}
	storedRevision := p.Revision
	if i := queueIndex(p, next.ID); i >= 0 {
		p = p.Queue.Batches[i].Progress
	}
	if current != version || storedRevision != revision || p.Round != round || collaborationID(&p) != next.ID {
		return ErrStaleWork
	}
	name := collaborationKey(key, next.ID, next.Channel)
	var value string
	err = tx.QueryRowContext(ctx, "SELECT value FROM ghraft_meta WHERE name=?", name).Scan(&value)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	var previous ForkCollaboration
	if err == nil && json.Unmarshal([]byte(value), &previous) != nil {
		return errors.New("invalid stored collaboration")
	}
	if previous.Revision != next.Revision || previous.Channel != "" && previous.Channel != next.Channel {
		return ErrStaleWork
	}
	if report != nil {
		if old := previous.item(report.TaskID); old != nil && old.Result != nil && old.Result.ReportID == report.ReportID {
			if err = saveReport(ctx, tx, report.EventID, report.ReportID, canonical(report)); err != nil {
				return err
			}
			return tx.Commit()
		}
		if err = saveReport(ctx, tx, report.EventID, report.ReportID, canonical(report)); err != nil {
			return err
		}
	}
	next.Revision++
	for i := range next.Items {
		if next.Items[i].Result != nil {
			next.Items[i].Result.Current = false
		}
	}
	changedAssignment := len(previous.Items) != len(next.Items)
	for _, item := range next.Items {
		if prior := previous.item(item.Task.MessageID); prior != nil && prior.Revoked != item.Revoked {
			changedAssignment = true
		}
		binding := "collaboration-task:" + hash([]byte(item.Task.Channel+":"+item.Task.MessageID))
		if item.Kind != "coordinate" {
			if err = tx.Upsert(ctx, &metaRecord{binding, key + ":" + next.ID}, []string{"name"}, nil); err != nil {
				return err
			}
			var scope string
			if err = tx.QueryRowContext(ctx, "SELECT value FROM ghraft_meta WHERE name=?", binding).Scan(&scope); err != nil {
				return err
			}
			if scope != key+":"+next.ID {
				return errors.New("participant task already belongs to another PR; register a separate task")
			}
		}
	}
	if changedAssignment || report != nil {
		request := fmt.Sprintf("%s:%s:%d", key, next.ID, next.Revision)
		origin := ""
		if report != nil {
			request = key + ":" + report.ReportID
			origin = report.TaskID
		}
		eventID := hash([]byte("collaboration-update:" + request))
		var body string
		if err = tx.QueryRowContext(ctx, "SELECT body FROM ghraft_snapshots WHERE resource_key=?", key).Scan(&body); err != nil {
			return err
		}
		var snapshot Snapshot
		if json.Unmarshal([]byte(body), &snapshot) != nil {
			return errors.New("invalid project snapshot")
		}
		payload := canonical(map[string]any{"event_id": eventID, "resource_key": key, "version": version, "data_classification": snapshot.Classification, "subject": subjectOf(snapshot), "kind": "collaboration.updated", "collaboration_id": next.ID, "origin_task": origin, "fork_batch": next.ID, "fork_pr": p.PR, "changes": []Change{}})
		if err = tx.Upsert(ctx, &eventRecord{EventID: eventID, ResourceKey: key, VersionHash: version, Payload: string(payload), State: "pending", CreatedAt: time.Now().Unix()}, []string{"event_id"}, nil); err != nil {
			return err
		}
		if err = tx.Upsert(ctx, &metaRecord{"current-event:" + key, eventID}, []string{"name"}, []string{"value"}); err != nil {
			return err
		}
	}
	if err = tx.Upsert(ctx, &metaRecord{name, string(canonical(next))}, []string{"name"}, []string{"value"}); err != nil {
		return err
	}
	return tx.Commit()
}
