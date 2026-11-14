package app

import (
	"context"
	"errors"
	"regexp"
	"slices"
)

// Work items belong to a PR/batch, not to a poll or to the selected queue slot.
// Raft owns their assignment and status; this registry binds their scope and
// permits only the operations needed for their kind of work.
type ForkCollaboration struct {
	ID       string         `json:"id"`
	Channel  string         `json:"channel"`
	Revision int64          `json:"revision"`
	Items    []ForkWorkItem `json:"items"`
}

type ForkWorkItem struct {
	Task         TaskDispatch           `json:"task"`
	Kind         string                 `json:"kind"`
	Registered   string                 `json:"registered_by"`
	Actors       []string               `json:"executors,omitempty"`
	Revoked      bool                   `json:"revoked,omitempty"`
	PRDiscussion bool                   `json:"pr_discussion,omitempty"`
	PR           int                    `json:"pr,omitempty"`
	Link         *TaskLink              `json:"link,omitempty"`
	Result       *ContributionReference `json:"result,omitempty"`
}

type ContributionReference struct {
	ReportID    string `json:"report_id"`
	EventID     string `json:"event_id"`
	Executor    string `json:"executor"`
	Outcome     string `json:"outcome"`
	Round       string `json:"round"`
	Head        string `json:"head"`
	Base        string `json:"base"`
	Tree        string `json:"tree"`
	Mapping     string `json:"mapping"`
	Guide       string `json:"guide_version"`
	TestAttempt string `json:"test_attempt,omitempty"`
	Current     bool   `json:"current"` // Derived on read, never an authority bit.
}

type ForkWorkItemInput struct {
	WorkInput
	ParticipantTask string `json:"participant_task"`
	Kind            string `json:"kind"`
	Revoke          bool   `json:"revoke,omitempty"`
}

type ContributionInput struct {
	WorkInput
	ReportID string `json:"report_id"`
	Outcome  string `json:"outcome"`
	Summary  string `json:"summary"`
	Head     string `json:"head"`
	Base     string `json:"base"`
}

var workKindRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

func collaborationID(p *ForkProgress) string {
	if p.BatchID != "" {
		return p.BatchID
	}
	return p.Round
}

func (c *ForkCollaboration) item(task string) *ForkWorkItem {
	if c != nil {
		for i := range c.Items {
			if c.Items[i].Task.MessageID == task {
				return &c.Items[i]
			}
		}
	}
	return nil
}

func (s *Service) readCollaboration(ctx context.Context, w WorkContext) (*ForkCollaboration, error) {
	if !s.Config.Project(w.Event.Key) || w.Fork == nil || !idRE.MatchString(collaborationID(w.Fork)) {
		return nil, nil
	}
	c, err := s.Store.ForkCollaboration(ctx, w.Event.Key, collaborationID(w.Fork), s.Config.DeliveryTargetFor(w.Event.Key))
	if err != nil {
		return nil, err
	}
	if c.Channel == "" {
		c.Channel = s.Config.DeliveryTargetFor(w.Event.Key)
	}
	if c.Channel != s.Config.DeliveryTargetFor(w.Event.Key) {
		return nil, errors.New("work items belong to the previous project channel; explicitly register tasks in the new channel")
	}
	for i := range c.Items {
		if result := c.Items[i].Result; result != nil {
			result.Current = !c.Items[i].Revoked && (!c.Items[i].PRDiscussion || c.Items[i].PR == w.Fork.PR) && contributionCurrent(w, *result)
		}
	}
	return &c, nil
}

func contributionCurrent(w WorkContext, r ContributionReference) bool {
	p := w.Fork
	return p != nil && r.Round == p.Round && r.Guide == w.Guide.Version &&
		(str(forkFacts(w)["remote_head"]) == "" || str(forkFacts(w)["remote_head"]) == r.Head) &&
		r.Head != "" && r.Head == p.ValidatedHead && r.Base == p.ValidatedBase &&
		r.Base == str(forkFacts(w)["target_sha"]) && r.Tree == p.ValidatedTree &&
		r.Mapping == p.ValidatedMapping && p.ValidatedGuide == r.Guide &&
		p.QueueStale == "" && p.Validation != nil && p.Validation.State == "passed" && (r.TestAttempt == "" || (p.Validation != nil && p.Validation.State == "passed" && p.Validation.ID == r.TestAttempt))
}

// RegisterForkWorkItem can only be called by the current project coordinator.
// It associates an already-existing top-level Raft task; it never assigns an
// agent, impersonates a contributor, or creates tasks in someone else's name.
func (s *Service) RegisterForkWorkItem(ctx context.Context, in ForkWorkItemInput) (ForkCollaboration, error) {
	w, err := s.checkWork(ctx, in.WorkInput, "register")
	if err != nil {
		return ForkCollaboration{}, err
	}
	if !s.Config.Project(w.Event.Key) || w.Collaboration == nil || !uuidRE.MatchString(in.ParticipantTask) || !workKindRE.MatchString(in.Kind) || in.Kind == "coordinate" {
		return ForkCollaboration{}, errors.New("work items require a configured project channel, a current batch, a task UUID and a kind")
	}
	root, err := s.fixedTask(ctx, w)
	if err != nil {
		return ForkCollaboration{}, err
	}
	if err = s.Raft.verifyTaskOwner(ctx, root.Channel, root.Number, root.MessageID, in.Executor); err != nil {
		return ForkCollaboration{}, err
	}
	if in.ParticipantTask == root.MessageID {
		return ForkCollaboration{}, errors.New("the project coordination task cannot also be a participant work item")
	}
	c := *w.Collaboration
	item := c.item(in.ParticipantTask)
	var task taskState
	if item == nil {
		task, err = s.Raft.readTask(ctx, root.Channel, 0, in.ParticipantTask)
		if err != nil {
			return ForkCollaboration{}, err
		}
	}
	if item != nil {
		if item.Kind != in.Kind {
			return c, errors.New("a work item's kind is immutable; register a separate task")
		}
		if in.Revoke {
			item.Revoked = true
		} else if item.Revoked {
			return c, errors.New("work item was revoked; register a new task")
		} else {
			return c, nil
		}
	} else {
		if in.Revoke || len(c.Items) >= 32 || task.Status == "closed" || task.Status == "done" {
			return c, errors.New("cannot register this work item: absent revocation, finished task or 32-item limit")
		}
		for _, existing := range c.Items {
			if in.Kind == "sync" && existing.Kind == "sync" && !existing.Revoked {
				return c, errors.New("a PR has one synchronization work item; explicitly revoke it before handing off")
			}
		}
		c.Items = append(c.Items, ForkWorkItem{Task: TaskDispatch{Channel: root.Channel, Number: task.Number, MessageID: task.MessageID, ConversationAnchored: task.ConversationAnchored}, Kind: in.Kind, Registered: in.Executor})
	}
	err = s.Store.SaveForkCollaboration(ctx, w.Event.Key, w.Fork.Round, w.CurrentVersion, w.Fork.Revision, c, nil)
	if err == nil {
		c.Revision++
	}
	return c, err
}

// A coordinator controls the queue. A registered sync task controls one batch;
// a tester may run the trusted validator; reviewers and custom work kinds may
// inspect and report. None of these grants adds a GitHub action to the guide.
func (s *Service) authorizeProjectWork(ctx context.Context, w *WorkContext, in WorkInput, operation string) error {
	if !agentHandleRE.MatchString(in.Executor) {
		return errors.New("--executor must name your own Raft agent identity")
	}
	root, err := s.fixedTask(ctx, *w)
	if err != nil {
		return err
	}
	rootState, err := s.Raft.readTask(ctx, root.Channel, root.Number, root.MessageID)
	if err != nil {
		return err
	}
	if rootState.Status != "in_progress" || rootState.Owner == "" || rootState.OwnerType == "user" {
		return ErrClaimUnavailable
	}
	if in.TaskID == root.MessageID {
		if err = s.Raft.verifyTaskOwner(ctx, root.Channel, root.Number, root.MessageID, in.Executor); err != nil {
			return err
		}
		if operation == "review" {
			return ErrWorkDenied
		}
		if operation == "write" {
			for _, item := range collaborationItems(w.Collaboration) {
				if item.Kind == "sync" && !item.Revoked {
					return ErrWorkDenied
				}
			}
		}
		// The coordination task's link is scoped to this batch, not a different
		// PR's last work-link row. Participants never replace that row.
		if item := w.Collaboration.item(in.TaskID); item != nil {
			w.Task = item.Link
		} else {
			w.Task = nil
		}
		return nil
	}
	item := w.Collaboration.item(in.TaskID)
	if item == nil || item.Revoked || operation == "coordinate" || operation == "register" {
		return ErrWorkDenied
	}
	if item.PRDiscussion && (w.Fork == nil || item.PR != w.Fork.PR) {
		return ErrWorkDenied
	}
	if operation == "write" && item.Kind != "sync" || operation == "test" && item.Kind != "test" && item.Kind != "sync" || operation == "review" && item.Kind != "review" {
		return ErrWorkDenied
	}
	if err = s.Raft.verifyTaskOwner(ctx, item.Task.Channel, item.Task.Number, item.Task.MessageID, in.Executor); err != nil {
		return err
	}
	w.Task = item.Link
	return nil
}

func (s *Service) linkProjectTask(ctx context.Context, w WorkContext, in WorkInput) (TaskLink, error) {
	if w.Collaboration == nil {
		return TaskLink{}, errors.New("select a synchronization batch before linking project work")
	}
	c := *w.Collaboration
	item := c.item(in.TaskID)
	if item == nil {
		root, err := s.fixedTask(ctx, w)
		if err != nil || in.TaskID != root.MessageID {
			return TaskLink{}, ErrWorkDenied
		}
		c.Items = append(c.Items, ForkWorkItem{Task: root, Kind: "coordinate", Registered: in.Executor})
		item = &c.Items[len(c.Items)-1]
	}
	link := TaskLink{in.TaskID, c.Channel, in.EventID, in.EvidenceVersion, in.GuideVersion}
	if !slices.Contains(item.Actors, in.Executor) {
		item.Actors = append(item.Actors, in.Executor)
	}
	item.Link = &link
	return link, s.Store.SaveForkCollaboration(ctx, w.Event.Key, w.Fork.Round, w.CurrentVersion, w.Fork.Revision, c, nil)
}

// A task's link can survive an ownership handoff. Persist the live executor
// before granting a source operation, even when the successor reuses that link.
// Failure to save this audit record must prevent the operation from starting.
func (s *Service) recordProjectActor(ctx context.Context, w *WorkContext, in WorkInput, operation string) error {
	if w.Collaboration == nil || !slices.Contains([]string{"write", "test", "coordinate"}, operation) {
		return nil
	}
	c := *w.Collaboration
	item := c.item(in.TaskID)
	if item == nil {
		root, err := s.fixedTask(ctx, *w)
		if err != nil || in.TaskID != root.MessageID {
			return ErrWorkDenied
		}
		c.Items = append(c.Items, ForkWorkItem{Task: root, Kind: "coordinate", Registered: in.Executor})
		item = &c.Items[len(c.Items)-1]
	}
	if item.Kind != "sync" && item.Kind != "coordinate" || slices.Contains(item.Actors, in.Executor) {
		return nil
	}
	item.Actors = append(item.Actors, in.Executor)
	if err := s.Store.SaveForkCollaboration(ctx, w.Event.Key, w.Fork.Round, w.CurrentVersion, w.Fork.Revision, c, nil); err != nil {
		return err
	}
	c.Revision++
	w.Collaboration = &c
	return nil
}

func (s *Service) RecordContribution(ctx context.Context, in ContributionInput) error {
	w, err := s.checkWork(ctx, in.WorkInput, "report")
	if err != nil {
		return err
	}
	item := w.Collaboration.item(in.TaskID)
	if item == nil || item.Revoked || item.Link == nil || item.Link.EvidenceVersion != in.EvidenceVersion || item.Link.GuideVersion != in.GuideVersion {
		return errors.New("link the participant task to current evidence before reporting")
	}
	if !uuidRE.MatchString(in.ReportID) || len(in.Summary) == 0 || len(in.Summary) > 8000 || !slices.Contains([]string{"investigated", "waiting", "passed", "failed", "approved", "changes-requested"}, in.Outcome) {
		return errors.New("invalid contribution")
	}
	if (in.Outcome == "approved" || in.Outcome == "changes-requested") && item.Kind != "review" || (in.Outcome == "passed" || in.Outcome == "failed") && item.Kind != "test" {
		return ErrWorkDenied
	}
	p := w.Fork
	if p == nil || !shaRE.MatchString(in.Head) || !shaRE.MatchString(in.Base) || in.Base != str(forkFacts(w)["target_sha"]) {
		return ErrStaleWork
	}
	if in.Outcome == "approved" || in.Outcome == "passed" {
		if p.ValidatedHead != in.Head || p.ValidatedBase != in.Base || p.ValidatedGuide != in.GuideVersion || !validForkProofHead(*p) || p.QueueStale != "" {
			return errors.New("a positive test or review result requires the current complete validation proof")
		}
	}
	if in.Outcome == "passed" && (p.Validation == nil || p.Validation.Executor != in.Executor || p.Validation.TaskID != in.TaskID || p.Validation.State != "passed") {
		return errors.New("passed contribution must reference this executor's own completed validation attempt")
	}
	if in.Outcome == "approved" {
		if err = s.requireIndependentReviewer(ctx, w, in.WorkInput); err != nil {
			return err
		}
	}

	if err = s.verifyContributionReport(ctx, item.Task, in.Executor, in.ReportID); err != nil {
		return err
	}
	ref := ContributionReference{ReportID: in.ReportID, EventID: in.EventID, Executor: in.Executor, Outcome: in.Outcome, Round: p.Round, Head: in.Head, Base: in.Base, Tree: p.ValidatedTree, Mapping: p.ValidatedMapping, Guide: in.GuideVersion}
	if in.Outcome == "passed" && p.Validation != nil {
		ref.TestAttempt = p.Validation.ID
	}
	item.Result = &ref
	return s.Store.SaveForkCollaboration(ctx, w.Event.Key, p.Round, w.CurrentVersion, p.Revision, *w.Collaboration, &in)
}

func collaborationItems(c *ForkCollaboration) []ForkWorkItem {
	if c == nil {
		return nil
	}
	return c.Items
}

func (s *Service) VerifyWorkItemClaim(ctx context.Context, in WorkInput) error {
	if in.TaskID == "" {
		return s.VerifyClaim(ctx, in.EventID, in.Executor)
	}
	w, err := s.WorkContextFor(ctx, in.EventID, in.BatchID, in.TaskID)
	if err != nil {
		return err
	}
	if !s.Config.Project(w.Event.Key) {
		return s.VerifyClaim(ctx, in.EventID, in.Executor)
	}
	return s.authorizeProjectWork(ctx, &w, in, "inspect")
}

func (s *Service) requireIndependentReviewer(ctx context.Context, w WorkContext, in WorkInput) error {
	root, err := s.fixedTask(ctx, w)
	if err != nil {
		return err
	}
	state, err := s.Raft.readTask(ctx, root.Channel, root.Number, root.MessageID)
	if err != nil {
		return err
	}
	if sameExecutor(state, in.Executor) {
		return errors.New("review approval requires an independent executor")
	}
	for _, other := range w.Collaboration.Items {
		if other.Kind != "sync" && other.Kind != "coordinate" {
			continue
		}
		if slices.Contains(other.Actors, in.Executor) {
			return errors.New("a synchronization contributor cannot approve its own batch after handoff")
		}
		if other.Revoked {
			continue // Historical actors remain authoritative after task deletion.
		}
		state, err := s.Raft.readTask(ctx, other.Task.Channel, other.Task.Number, other.Task.MessageID)
		if err != nil {
			return err
		}
		if sameExecutor(state, in.Executor) {
			return errors.New("review approval requires an executor independent of synchronization and coordination")
		}
	}
	return nil
}
