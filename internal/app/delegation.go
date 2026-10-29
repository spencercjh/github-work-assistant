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
	Body   string `json:"body,omitempty"`
}

const maxPRBodyBytes = 128 << 10

// JSON can expand one body byte into six (for example '<' becomes '\u003c').
// Leave room for that expansion and the action's other fields; other endpoints
// retain the smaller default request limit.
const maxGitHubActionRequestBytes = 1 << 20

func checkPRBodySize(body string) error {
	if len(body) > maxPRBodyBytes {
		return errors.New("PR body exceeds 128 KiB; shorten the descriptions or select a smaller batch")
	}
	return nil
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
	Repository     string   `json:"repository"`
	Number         int      `json:"number"`
	Branch         string   `json:"branch,omitempty"`
	TargetBranch   string   `json:"target_branch,omitempty"`
	EditableFields []string `json:"editable_fields,omitempty"`
	Permitted      bool     `json:"permitted"`
	Verification   string   `json:"verification"`
}

// This is a fresh authorization/revision check, not a merge decision or a write
// proxy. The executor evaluates the trusted guide and uses its own gh session.
func (s *Service) CheckGitHubAction(ctx context.Context, in GitHubActionInput) (GitHubActionCheck, error) {
	var result GitHubActionCheck
	if err := checkPRBodySize(in.Body); err != nil {
		return result, err
	}
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
		if in.Action == "edit-pr" && (pull["state"] != "open" || pull["merged"] != false) {
			return result, errors.New("metadata updates require an open PR")
		}
	} else if in.Action == "merge" || in.Action == "review" || in.Action == "edit-pr" || in.Head != "" || in.Base != "" {
		return result, errors.New("action requires a PR with captured revisions")
	}
	watch, _ := resourceWatch(w.Event.Key)
	if in.Action == "edit-pr" {
		return prMetadataCheck(in, watch.Repo, watch.Number), nil
	}
	return GitHubActionCheck{GitHubActionInput: in, Repository: watch.Repo, Number: watch.Number, Permitted: true, Verification: "Current delegation, task ownership and revisions checked. The executor must assess the guide's conditions, diff, latest checks and review discussions. Use gh with expected head for merging, never bypass branch rules, then verify the remote outcome. This check performs no GitHub write."}, nil
}

func prMetadataCheck(in GitHubActionInput, repo string, number int) GitHubActionCheck {
	return GitHubActionCheck{GitHubActionInput: in, Repository: repo, Number: number, Permitted: true, EditableFields: []string{"title", "body"}, Verification: "Current delegation, owner and open PR revisions checked. Only update this PR's title and body with gh pr edit --title/--body-file. No branch, label, reviewer, state or merge changes are authorized. Re-read the PR before writing to avoid overwriting another editor, then verify the remote result. This check performs no GitHub write and is not test or merge approval."}
}
