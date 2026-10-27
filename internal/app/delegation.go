package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

type GitHubActionInput struct {
	WorkInput
	Action string `json:"action"`
	Ref    string `json:"ref,omitempty"`
	Head   string `json:"head"`
	Base   string `json:"base"`
}

// RequestWork is an operator command: enqueue a new consideration of an already
// watched item after a new delegation, without inventing a GitHub change or
// replaying archived events. It never reopens a closed discussion.
func (s *Service) RequestWork(ctx context.Context, eventID, requestID, reason string) (Event, error) {
	if !uuidRE.MatchString(requestID) || strings.TrimSpace(reason) == "" || len(reason) > 2000 {
		return Event{}, errors.New("request-work requires a request UUID and a 1..2000 byte reason")
	}
	w, err := s.RefreshWorkContext(ctx, eventID)
	if err != nil {
		return Event{}, err
	}
	return s.Store.RequestWork(ctx, w.Event.Key, w.CurrentVersion, requestID, reason)
}

type GitHubActionCheck struct {
	ExpectedRemoteHead string `json:"expected_remote_head,omitempty"`
	GitHubActionInput
	Repository   string `json:"repository"`
	Number       int    `json:"number"`
	Branch       string `json:"branch,omitempty"`
	TargetBranch string `json:"target_branch,omitempty"`
	Permitted    bool   `json:"permitted"`
	Verification string `json:"verification"`
}

// This is a fresh authorization/revision check, not a merge decision or a write
// proxy. The executor evaluates the trusted guide and uses its own gh session.
func (s *Service) CheckGitHubAction(ctx context.Context, in GitHubActionInput) (GitHubActionCheck, error) {
	var result GitHubActionCheck
	if _, err := s.RefreshWorkContext(ctx, in.EventID); err != nil {
		return result, err
	}
	w, err := s.CheckWork(ctx, in.WorkInput)
	if err != nil {
		return result, err
	}
	if !slices.Contains(w.Guide.GitHubActions, in.Action) || w.Task == nil || w.Task.TaskID != in.TaskID || w.Task.GuideVersion != in.GuideVersion || w.Task.EvidenceVersion != in.EvidenceVersion {
		return result, ErrWorkDenied
	}
	if w.Fork != nil {
		return s.checkForkAction(ctx, w, in)
	}
	var snapshot Snapshot
	if json.Unmarshal(w.Snapshot, &snapshot) != nil {
		return result, errors.New("invalid snapshot")
	}
	if pull, ok := snapshot.Objects["pull"]; ok {
		if !shaRE.MatchString(in.Head) || !shaRE.MatchString(in.Base) || str(pull["head"]) != in.Head || str(pull["base_sha"]) != in.Base {
			return result, ErrStaleWork
		}
		if in.Action == "merge" && (pull["state"] != "open" || pull["merged"] == true || pull["draft"] != false) {
			return result, errors.New("merge requires an open non-draft PR")
		}
	} else if in.Action == "merge" || in.Action == "review" || in.Head != "" || in.Base != "" {
		return result, errors.New("action requires a PR with captured revisions")
	}
	watch, _ := resourceWatch(w.Event.Key)
	return GitHubActionCheck{GitHubActionInput: in, Repository: watch.Repo, Number: watch.Number, Permitted: true, Verification: "Current delegation, task ownership and revisions checked. The executor must assess the guide's conditions, diff, latest checks and review discussions. Use gh with expected head for merging, never bypass branch rules, then verify the remote outcome. This check performs no GitHub write."}, nil
}
