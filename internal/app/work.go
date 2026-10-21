package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

type TaskLink struct {
	TaskID          string `json:"raft_task_message_id"`
	Target          string `json:"target"`
	EventID         string `json:"event_id"`
	EvidenceVersion string `json:"evidence_version"`
	GuideVersion    string `json:"guide_version"`
}
type WorkContext struct {
	Event          Event             `json:"event"`
	Snapshot       json.RawMessage   `json:"snapshot"`
	CurrentVersion string            `json:"current_version"`
	ObservedAt     int64             `json:"observed_at"`
	Stale          bool              `json:"stale"`
	Guide          RepositoryGuide   `json:"guide"`
	Task           *TaskLink         `json:"task,omitempty"`
	TaskAuthority  string            `json:"task_authority"`
	History        []json.RawMessage `json:"processed_history"`
}
type WorkInput struct {
	EventID         string `json:"event_id"`
	EvidenceVersion string `json:"evidence_version"`
	GuideVersion    string `json:"guide_version"`
	TaskID          string `json:"raft_task_message_id"`
}
type WorkResult struct {
	WorkInput
	ReportID string `json:"raft_report_message_id"`
	Outcome  string `json:"outcome"`
	Summary  string `json:"summary"`
}

var ErrStaleWork = errors.New("work evidence or repository guidance changed; refresh context")
var ErrWorkDenied = errors.New("resource is outside current authorization")

func (s *Service) WorkContext(ctx context.Context, id string) (WorkContext, error) {
	var w WorkContext
	if !idRE.MatchString(id) {
		return w, errors.New("invalid event id")
	}
	var payload, body string
	err := s.Store.DB.QueryRowContext(ctx, `SELECT e.event_id,e.resource_key,e.version_hash,e.payload,e.state,s.body,s.version_hash,s.observed_at FROM ghraft_events e JOIN ghraft_snapshots s ON s.resource_key=e.resource_key WHERE e.event_id=?`, id).Scan(&w.Event.ID, &w.Event.Key, &w.Event.Version, &payload, &w.Event.State, &body, &w.CurrentVersion, &w.ObservedAt)
	if err != nil {
		return w, err
	}
	if !s.allowed(ctx, w.Event.Key) {
		return WorkContext{}, ErrWorkDenied
	}
	watch, err := resourceWatch(w.Event.Key)
	if err != nil {
		return w, err
	}
	w.Guide, err = s.Config.RepositoryGuide(watch.Repo)
	if err != nil {
		return w, err
	}
	w.Event.Payload = json.RawMessage(payload)
	w.Snapshot = json.RawMessage(body)
	w.Stale = w.CurrentVersion != w.Event.Version
	w.TaskAuthority = "Raft; task link and reports are agent-provided references. Read live Raft task state before creating, claiming or resuming work."
	link := &TaskLink{}
	err = s.Store.DB.QueryRowContext(ctx, `SELECT task_id,target,event_id,evidence_version,guide_version FROM ghraft_work_links WHERE resource_key=?`, w.Event.Key).Scan(&link.TaskID, &link.Target, &link.EventID, &link.EvidenceVersion, &link.GuideVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return w, err
	}
	if err == nil {
		w.Task = link
	}
	w.History, err = s.Store.History(ctx, w.Event.Key)
	if err != nil {
		return w, err
	}
	return w, nil
}

func (s *Service) RefreshWorkContext(ctx context.Context, id string) (WorkContext, error) {
	w, err := s.WorkContext(ctx, id)
	if err != nil {
		return w, err
	}
	watch, err := resourceWatch(w.Event.Key)
	if err != nil {
		return w, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.GH.CheckIdentity(ctx); err != nil {
		return w, err
	}
	err = s.collectWatch(ctx, watch)
	if statusErr := s.Store.CollectionStatus(ctx, watch.Key(), err); statusErr != nil {
		return w, statusErr
	}
	if err != nil {
		return w, err
	}
	return s.WorkContext(ctx, id)
}

func (s *Service) CheckWork(ctx context.Context, in WorkInput) (WorkContext, error) {
	w, err := s.WorkContext(ctx, in.EventID)
	if err != nil {
		return w, err
	}
	if w.Stale || in.EvidenceVersion != w.CurrentVersion || in.GuideVersion != w.Guide.Version || w.ObservedAt < time.Now().Add(-10*time.Minute).Unix() {
		return w, ErrStaleWork
	}
	if !uuidRE.MatchString(in.TaskID) {
		return w, errors.New("a real Raft task UUID is required")
	}
	return w, nil
}

func (s *Service) LinkTask(ctx context.Context, in WorkInput) (TaskLink, error) {
	w, err := s.CheckWork(ctx, in)
	if err != nil {
		return TaskLink{}, err
	}
	if w.Guide.TaskTarget == "" {
		return TaskLink{}, errors.New("repository task destination is not configured")
	}
	tx, err := s.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return TaskLink{}, err
	}
	defer tx.Rollback()
	var version string
	if err = tx.QueryRowContext(ctx, `SELECT version_hash FROM ghraft_snapshots WHERE resource_key=? FOR UPDATE`, w.Event.Key).Scan(&version); err != nil {
		return TaskLink{}, err
	}
	if version != in.EvidenceVersion {
		return TaskLink{}, ErrStaleWork
	}
	var task, target string
	err = tx.QueryRowContext(ctx, `SELECT task_id,target FROM ghraft_work_links WHERE resource_key=? FOR UPDATE`, w.Event.Key).Scan(&task, &target)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return TaskLink{}, err
	}
	if err == nil && (task != in.TaskID || target != w.Guide.TaskTarget) {
		return TaskLink{}, errors.New("resource already belongs to a different Raft task or destination")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ghraft_work_links(resource_key,task_id,target,event_id,evidence_version,guide_version,updated_at) VALUES(?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE event_id=VALUES(event_id),evidence_version=VALUES(evidence_version),guide_version=VALUES(guide_version),updated_at=VALUES(updated_at)`, w.Event.Key, in.TaskID, w.Guide.TaskTarget, in.EventID, in.EvidenceVersion, in.GuideVersion, time.Now().Unix())
	if err != nil {
		return TaskLink{}, err
	}
	if err = tx.Commit(); err != nil {
		return TaskLink{}, err
	}
	return TaskLink{in.TaskID, w.Guide.TaskTarget, in.EventID, in.EvidenceVersion, in.GuideVersion}, nil
}

func (s *Service) RecordWorkResult(ctx context.Context, in WorkResult) error {
	if !uuidRE.MatchString(in.ReportID) || len(in.Summary) == 0 || len(in.Summary) > 8000 || (in.Outcome != "investigated" && in.Outcome != "changes-ready" && in.Outcome != "needs-decision") {
		return errors.New("invalid work result")
	}
	w, err := s.CheckWork(ctx, in.WorkInput)
	if err != nil {
		return err
	}
	if w.Task == nil || w.Task.TaskID != in.TaskID || w.Task.Target != w.Guide.TaskTarget || w.Task.GuideVersion != in.GuideVersion || w.Task.EvidenceVersion != in.EvidenceVersion {
		return errors.New("bind current evidence to the existing Raft task before reporting")
	}
	if in.Outcome == "changes-ready" && w.Guide.Mode != "local-edit" {
		return ErrWorkDenied
	}
	tx, err := s.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current string
	if err = tx.QueryRowContext(ctx, `SELECT version_hash FROM ghraft_snapshots WHERE resource_key=? FOR UPDATE`, w.Event.Key).Scan(&current); err != nil {
		return err
	}
	if current != in.EvidenceVersion {
		return ErrStaleWork
	}
	body := string(canonical(struct {
		WorkResult
		Guide RepositoryGuide `json:"guide"`
	}{in, w.Guide}))
	_, err = tx.ExecContext(ctx, `INSERT IGNORE INTO ghraft_results(event_id,report_id,body,created_at) VALUES(?,?,?,?)`, in.EventID, in.ReportID, body, time.Now().Unix())
	if err != nil {
		return err
	}
	var saved string
	if err = tx.QueryRowContext(ctx, `SELECT body FROM ghraft_results WHERE event_id=? AND report_id=?`, in.EventID, in.ReportID).Scan(&saved); err != nil {
		return err
	}
	if saved != body {
		return errors.New("report already recorded with different content")
	}
	return tx.Commit()
}

func (s *Service) registerWorkActions(m *http.ServeMux) {
	for _, refresh := range []bool{false, true} {
		name := "get-work-context"
		if refresh {
			name = "refresh-work-context"
		}
		m.HandleFunc("POST /api/actions/"+name, s.protected(func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				EventID string `json:"event_id"`
			}
			if decode(r, &in) != nil {
				fail(w, 400, "INVALID_INPUT")
				return
			}
			var v WorkContext
			var err error
			if refresh {
				v, err = s.RefreshWorkContext(r.Context(), in.EventID)
			} else {
				v, err = s.WorkContext(r.Context(), in.EventID)
			}
			if err != nil {
				workError(w, err)
				return
			}
			ok(w, v)
		}))
	}
	m.HandleFunc("POST /api/actions/link-task", s.protected(func(w http.ResponseWriter, r *http.Request) {
		var in WorkInput
		if decode(r, &in) != nil {
			fail(w, 400, "INVALID_INPUT")
			return
		}
		v, err := s.LinkTask(r.Context(), in)
		if err != nil {
			workError(w, err)
			return
		}
		ok(w, v)
	}))
	m.HandleFunc("POST /api/actions/record-work-result", s.protected(func(w http.ResponseWriter, r *http.Request) {
		var in WorkResult
		if decode(r, &in) != nil {
			fail(w, 400, "INVALID_INPUT")
			return
		}
		if err := s.RecordWorkResult(r.Context(), in); err != nil {
			workError(w, err)
			return
		}
		ok(w, map[string]any{"recorded": true, "raft_task_status_changed": false, "verification": "agent-reported evidence; remote writes remain unapproved"})
	}))
}
func workError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrWorkDenied):
		fail(w, 403, "WORK_NOT_ALLOWED")
	case errors.Is(err, ErrStaleWork):
		fail(w, 409, "STALE_WORK")
	case errors.Is(err, sql.ErrNoRows):
		fail(w, 404, "EVENT_NOT_FOUND")
	default:
		fail(w, 409, "WORK_CONFLICT")
	}
}
