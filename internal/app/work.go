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
	Collaboration  *ForkCollaboration `json:"collaboration,omitempty"`
	CurrentEventID string             `json:"current_event_id"`
	Fork           *ForkProgress      `json:"fork,omitempty"`
	Event          Event              `json:"event"`
	Snapshot       json.RawMessage    `json:"snapshot"`
	CurrentVersion string             `json:"current_version"`
	ObservedAt     int64              `json:"observed_at"`
	Stale          bool               `json:"stale"`
	Guide          RepositoryGuide    `json:"guide"`
	Task           *TaskLink          `json:"task,omitempty"`
	Discussion     *TaskDispatch      `json:"discussion,omitempty"`
	TaskAuthority  string             `json:"task_authority"`
	History        []json.RawMessage  `json:"processed_history"`
}
type WorkInput struct {
	BatchID         string `json:"batch_id,omitempty"`
	Executor        string `json:"executor,omitempty"`
	EventID         string `json:"event_id"`
	EvidenceVersion string `json:"evidence_version"`
	GuideVersion    string `json:"guide_version"`
	TaskID          string `json:"raft_task_message_id"`
}
type WorkResult struct {
	ReviewOnly bool `json:"review_only,omitempty"`
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
		if !errors.Is(err, ErrClaimUnavailable) || in.Outcome != "quiet" {
			return err
		}
		w, readErr := s.WorkContext(ctx, in.EventID)
		if readErr != nil {
			return readErr
		}
		if err := s.verifyDoneTriage(ctx, w, in.Executor); err != nil {
			return err
		}
	}
	if in.Outcome == "notified" && (s.Config.NotifyTarget == "" || in.Target != s.Config.NotifyTarget) {
		return errors.New("notification destination does not match configuration")
	}
	w, err := s.WorkContext(ctx, in.EventID)
	if err != nil {
		return err
	}
	if in.Outcome == "superseded" {
		if err := s.checkTriageCoverage(ctx, w, in); err != nil {
			return err
		}
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
	if err := s.checkAccess(ctx, w.Event.Key); err != nil {
		return WorkContext{}, err
	}
	definition, err := s.Config.WorkDefinition(w.Event.Key)
	if err != nil {
		return w, err
	}
	w.Guide, err = definition.Guide(s.Config)
	if definition.Fork != nil && err == nil {
		progress, e := s.Store.ForkProgress(ctx, w.Event.Key)
		if e != nil {
			return w, e
		}
		w.Fork = &progress
	}
	if err != nil {
		return w, err
	}
	w.Stale = w.CurrentVersion != w.Event.Version
	if d, e := s.Store.Discussion(ctx, s.Config.discussionResource(w.Event.Key)); e == nil {
		w.Discussion = d.Task
	} else if !errors.Is(e, ErrNotFound) {
		return WorkContext{}, e
	}
	w.Collaboration, err = s.readCollaboration(ctx, w)
	if err != nil {
		return WorkContext{}, err
	}
	if s.Config.Project(w.Event.Key) {
		w.Task = nil
		if w.Discussion != nil {
			if item := w.Collaboration.item(w.Discussion.MessageID); item != nil {
				w.Task = item.Link
			}
		}
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
	definition, err := s.Config.WorkDefinition(w.Event.Key)
	if err != nil {
		return w, err
	}
	// Fork refreshes coordinate across both HTTP callers and independent CLI
	// processes. Do not serialize them here before they can join a live refresh.
	if definition.Fork == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	if err = s.GH.CheckIdentity(ctx); err != nil {
		return w, err
	}
	err = definition.Collect(ctx, s)
	if definition.Fork == nil {
		if statusErr := s.Store.CollectionStatus(ctx, definition.Key, err); statusErr != nil {
			return w, statusErr
		}
	}
	if err != nil {
		return w, err
	}
	return s.WorkContext(ctx, id)
}

func (s *Service) CheckWork(ctx context.Context, in WorkInput) (WorkContext, error) {
	return s.checkWork(ctx, in, "write")
}

func (s *Service) checkWork(ctx context.Context, in WorkInput, operation string) (WorkContext, error) {
	w, err := s.WorkContextFor(ctx, in.EventID, in.BatchID, in.TaskID)
	if err != nil {
		return w, err
	}
	if w.Fork != nil && w.Fork.Queue != nil && !slices.Contains([]string{"inspect", "report", "register", "review"}, operation) {
		selected, e := s.Store.ForkProgress(ctx, w.Event.Key)
		if e != nil {
			return w, e
		}
		if w.Fork.BatchID != selected.BatchID {
			return w, errors.New("the coordinator must select this batch before workspace or GitHub operations")
		}
	}
	if err := s.checkWorkAuthority(ctx, &w, in, operation); err != nil {
		return w, err
	}
	// Project registration/revocation changes only collaboration scope. It can
	// use an older accepted coordination receipt with current evidence, allowing
	// the coordinator to revoke a deleted recipient that blocks pending delivery.
	if (w.Stale && operation != "register") || in.EvidenceVersion != w.CurrentVersion {
		return w, ErrStaleWork
	}
	if err := s.recordProjectActor(ctx, &w, in, operation); err != nil {
		return w, err
	}
	return w, nil
}

// Evidence is checked separately: completed fork tests may tolerate only a
// retry wake, but must still satisfy the same live authority and freshness.
func (s *Service) checkWorkAuthority(ctx context.Context, w *WorkContext, in WorkInput, operation string) error {
	if s.Config.Project(w.Event.Key) {
		if err := s.authorizeProjectWork(ctx, w, in, operation); err != nil {
			return err
		}
	} else if err := s.VerifyClaim(ctx, in.EventID, in.Executor); err != nil {
		return err
	}
	if in.GuideVersion != w.Guide.Version || w.ObservedAt < time.Now().Add(-10*time.Minute).Unix() {
		return ErrStaleWork
	}
	if !uuidRE.MatchString(in.TaskID) {
		return errors.New("a real Raft task UUID is required")
	}
	if s.Config.SharedTasksFor(w.Event.Key) {
		if w.Guide.TaskTarget == "" {
			return errors.New("repository task destination is not configured")
		}
		if err := s.Raft.verifyTaskOwner(ctx, w.Guide.TaskTarget, 0, in.TaskID, in.Executor); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) LinkTask(ctx context.Context, in WorkInput) (TaskLink, error) {
	w, err := s.checkWork(ctx, in, "report")
	if err != nil {
		return TaskLink{}, err
	}
	if s.Config.Project(w.Event.Key) {
		return s.linkProjectTask(ctx, w, in)
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
	if in.ReviewOnly && (w.Fork == nil || w.Fork.Queue == nil || in.Outcome != "needs-decision") {
		return errors.New("review_only is limited to a queued fork waiting solely for human review")
	}
	if in.Outcome == "changes-ready" && w.Guide.Mode != "local-edit" {
		return ErrWorkDenied
	}
	if in.Outcome == "needs-decision" && s.Config.SharedTasksFor(w.Event.Key) {
		if err := s.verifyDecisionReport(ctx, w, in); err != nil {
			return err
		}
	}
	if w.Fork != nil {
		if in.Outcome == "merged" {
			if _, err := s.RefreshWorkContext(ctx, in.EventID); err != nil {
				return err
			}
			w, err = s.CheckWork(ctx, in.WorkInput)
			if err != nil {
				return err
			}
		}
		return s.recordForkResult(ctx, w, in)
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
		if decodeLimit(r, &in, maxGitHubActionRequestBytes) != nil {
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
		response := map[string]any{"recorded": true, "raft_task_status_changed": false, "verification": "executor-reported result; merged outcomes also require captured merged PR state"}
		if in.Outcome == "merged" && !in.ReviewOnly {
			response["next_action"] = "For project PR tasks, run finish-work from the current coordinator's CLI; this HTTP request does not change task status."
		}
		ok(w, response)
	}))
}
func workError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrGitHubRateLimited):
		fail(w, 503, "GITHUB_RATE_LIMITED")
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
