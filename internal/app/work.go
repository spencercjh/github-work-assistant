package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
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
	Discussion     *TaskDispatch     `json:"discussion,omitempty"`
	TaskAuthority  string            `json:"task_authority"`
	History        []json.RawMessage `json:"processed_history"`
}
type WorkInput struct {
	Executor        string `json:"executor,omitempty"`
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

// RecordTriage is shared by local execution clients. It applies the same
// destination and watch boundary as the authenticated App action.
func (s *Service) RecordTriage(ctx context.Context, in Triage) error {
	if err := s.VerifyClaim(ctx, in.EventID, in.Executor); err != nil {
		return err
	}
	if in.Outcome == "notified" && (s.Config.NotifyTarget == "" || in.Target != s.Config.NotifyTarget) {
		return errors.New("notification destination does not match configuration")
	}
	if _, err := s.WorkContext(ctx, in.EventID); err != nil {
		return err
	}
	return s.Store.RecordTriage(ctx, in)
}

func (s *Service) WorkContext(ctx context.Context, id string) (WorkContext, error) {
	var w WorkContext
	if !idRE.MatchString(id) {
		return w, errors.New("invalid event id")
	}
	w, err := s.Store.ReadWork(ctx, id)
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
	w.Stale = w.CurrentVersion != w.Event.Version
	if d, e := s.Store.Discussion(ctx, w.Event.Key); e == nil {
		w.Discussion = d.Task
	} else if !errors.Is(e, ErrNotFound) {
		return WorkContext{}, e
	}
	w.TaskAuthority = "Raft; task link and reports are agent-provided references. Read live Raft task state before creating, claiming or resuming work."

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
	if err := s.VerifyClaim(ctx, in.EventID, in.Executor); err != nil {
		return w, err
	}
	if w.Stale || in.EvidenceVersion != w.CurrentVersion || in.GuideVersion != w.Guide.Version || w.ObservedAt < time.Now().Add(-10*time.Minute).Unix() {
		return w, ErrStaleWork
	}
	if !uuidRE.MatchString(in.TaskID) {
		return w, errors.New("a real Raft task UUID is required")
	}
	if s.Config.SharedTasks() {
		if w.Guide.TaskTarget == "" {
			return w, errors.New("repository task destination is not configured")
		}
		if err := s.Raft.verifyTaskOwner(ctx, w.Guide.TaskTarget, 0, in.TaskID, in.Executor); err != nil {
			return w, err
		}
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
	return s.Store.BindTask(ctx, w.Event.Key, in, w.Guide.TaskTarget)
}

func (s *Service) RecordWorkResult(ctx context.Context, in WorkResult) error {
	if !uuidRE.MatchString(in.ReportID) || len(in.Summary) == 0 || len(in.Summary) > 8000 || !slices.Contains([]string{"investigated", "changes-ready", "needs-decision", "waiting", "merged"}, in.Outcome) {
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
	if in.Outcome == "merged" {
		var snapshot Snapshot
		if !slices.Contains(w.Guide.GitHubActions, "merge") || json.Unmarshal(w.Snapshot, &snapshot) != nil || snapshot.Objects["pull"]["merged"] != true {
			return errors.New("merged result requires delegated merge authority and refreshed merged PR evidence")
		}
	}
	return s.Store.SaveWorkResult(ctx, w.Event.Key, in)
}

func (s *Service) registerWorkActions(m *http.ServeMux) {
	m.HandleFunc("POST /api/actions/check-github-action", s.protected(func(w http.ResponseWriter, r *http.Request) {
		var in GitHubActionInput
		if decode(r, &in) != nil {
			fail(w, 400, "INVALID_INPUT")
			return
		}
		v, err := s.CheckGitHubAction(r.Context(), in)
		if err != nil {
			workError(w, err)
			return
		}
		ok(w, v)
	}))
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
		ok(w, map[string]any{"recorded": true, "raft_task_status_changed": false, "verification": "executor-reported result; merged outcomes also require captured merged PR state"})
	}))
}
func workError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrWorkDenied):
		fail(w, 403, "WORK_NOT_ALLOWED")
	case errors.Is(err, ErrStaleWork):
		fail(w, 409, "STALE_WORK")
	case errors.Is(err, ErrNotFound):
		fail(w, 404, "EVENT_NOT_FOUND")
	default:
		fail(w, 409, "WORK_CONFLICT")
	}
}
