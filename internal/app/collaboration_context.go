package app

import (
	"context"
	"errors"
	"strings"
)

// WorkContextFor reads a participant's PR without selecting it for the source
// writer. Queue selection remains an explicit coordinator operation. Event and
// guide versions continue to cover the whole resource's latest observation.
func (s *Service) WorkContextFor(ctx context.Context, event, batch, task string) (w WorkContext, err error) {
	defer func() {
		if err == nil && s.Config.Project(w.Event.Key) && task != "" {
			w.Task = nil
			if item := w.Collaboration.item(task); item != nil {
				if item.PRDiscussion && (w.Fork == nil || w.Fork.PR != item.PR) {
					w = WorkContext{}
					err = errors.New("this task belongs to a previous PR; read its original discussion and use the replacement PR's own task")
					return
				}
				w.Task = item.Link
			}
		}
	}()
	w, err = s.WorkContext(ctx, event)
	if err != nil {
		return w, err
	}
	if !s.Config.Project(w.Event.Key) {
		if batch != "" {
			return w, errors.New("batch context requires a project channel")
		}
		return w, nil
	}
	if task != "" {
		scope, e := s.Store.CollaborationScope(ctx, s.Config.DeliveryTargetFor(w.Event.Key), task)
		if e != nil {
			return w, e
		}
		if scope != "" {
			prefix := w.Event.Key + ":"
			if !strings.HasPrefix(scope, prefix) {
				return w, ErrWorkDenied
			}
			bound := strings.TrimPrefix(scope, prefix)
			if batch != "" && batch != bound {
				return w, ErrWorkDenied
			}
			batch = bound
		}
	}
	if batch == "" || w.Fork != nil && collaborationID(w.Fork) == batch {
		return w, nil
	}
	if w.Fork == nil || !idRE.MatchString(batch) {
		return w, ErrWorkDenied
	}
	i := queueIndex(*w.Fork, batch)
	if i < 0 {
		return w, errors.New("batch is no longer active; its stored reports remain in history")
	}
	selected := *w.Fork
	b := selected.Queue.Batches[i]
	p := cloneForkProgress(b.Progress)
	p.Revision, p.Queue = selected.Revision, selected.Queue
	w.Fork = &p
	spec, _ := s.Config.Fork(w.Event.Key)
	w.Snapshot = forkSnapshot(spec, p, b.Observation)
	w.Collaboration, err = s.readCollaboration(ctx, w)
	return w, err
}

func (s *Store) CollaborationScope(ctx context.Context, channel, task string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM ghraft_meta WHERE name=?", "collaboration-task:"+hash([]byte(channel+":"+task))).Scan(&value)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	return value, err
}
