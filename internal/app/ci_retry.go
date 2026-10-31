package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ForkCIRetry authorizes one failed job at one run attempt, independently of the
// source validation contract. It never grants general workflow or merge writes.
type ForkCIRetry struct {
	ForkID     string `json:"fork_id"`
	PR         int    `json:"pr"`
	Head       string `json:"head"`
	Base       string `json:"base"`
	RunID      int64  `json:"run_id"`
	RunAttempt int    `json:"run_attempt"`
	JobID      int64  `json:"job_id"`
}

func (c Config) validateForkCIRetries() error {
	seen := map[string]bool{}
	for _, grant := range c.ForkCIRetries {
		_, ok := c.Fork("fork:" + grant.ForkID)
		key := fmt.Sprintf("%s:%d", grant.ForkID, grant.RunID)
		if !ok || grant.PR < 1 || !shaRE.MatchString(grant.Head) || !shaRE.MatchString(grant.Base) || grant.RunID < 1 || grant.RunAttempt < 1 || grant.JobID < 1 || seen[key] {
			return errors.New("fork_ci_retries require a configured fork, exact PR/head/base/run/attempt/job and one grant per run")
		}
		seen[key] = true
	}
	return nil
}

type ciRepository struct {
	FullName string `json:"full_name"`
}

type ciPRRef struct {
	SHA string `json:"sha"`
	Ref string `json:"ref"`
}

type ciPullRequest struct {
	Number int     `json:"number"`
	Head   ciPRRef `json:"head"`
	Base   ciPRRef `json:"base"`
}

type ciWorkflowRun struct {
	ID             int64           `json:"id"`
	Attempt        int             `json:"run_attempt"`
	Head           string          `json:"head_sha"`
	Branch         string          `json:"head_branch"`
	Event          string          `json:"event"`
	Status         string          `json:"status"`
	Conclusion     string          `json:"conclusion"`
	Repository     ciRepository    `json:"repository"`
	HeadRepository ciRepository    `json:"head_repository"`
	PullRequests   []ciPullRequest `json:"pull_requests"`
}

type ciWorkflowJob struct {
	ID         int64  `json:"id"`
	RunID      int64  `json:"run_id"`
	Attempt    int    `json:"run_attempt"`
	Head       string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type ciRetrySource interface {
	CIRetryState(context.Context, string, int64, int64) (ciWorkflowRun, ciWorkflowJob, error)
}

var ciReadRE = regexp.MustCompile(`^repos/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/actions/(runs|jobs)/[1-9][0-9]*$`)

func validCIReadPath(path string) bool {
	return !strings.Contains(path, "..") && ciReadRE.MatchString(path)
}

func (g GH) CIRetryState(ctx context.Context, repo string, runID, jobID int64) (ciWorkflowRun, ciWorkflowJob, error) {
	var run ciWorkflowRun
	var job ciWorkflowJob
	if !repoRE.MatchString(repo) || strings.Contains(repo, "..") || runID < 1 || jobID < 1 {
		return run, job, ErrWorkDenied
	}
	if _, err := g.RepositoryClassification(ctx, repo); err != nil {
		return run, job, err
	}
	// Read the job first so the run's attempt/status is the most recent query.
	b, err := g.api(ctx, fmt.Sprintf("repos/%s/actions/jobs/%d", repo, jobID), false)
	if err != nil {
		return run, job, err
	}
	if json.Unmarshal(b, &job) != nil {
		return run, job, errors.New("invalid workflow job response")
	}
	b, err = g.api(ctx, fmt.Sprintf("repos/%s/actions/runs/%d", repo, runID), false)
	if err != nil {
		return run, job, err
	}
	if json.Unmarshal(b, &run) != nil {
		return run, job, errors.New("invalid workflow run response")
	}
	return run, job, nil
}

func (s *Service) checkForkCIRetry(ctx context.Context, w WorkContext, in GitHubActionInput) (GitHubActionCheck, error) {
	var result GitHubActionCheck
	if err := requireForkWork(w, in.WorkInput); err != nil {
		return result, err
	}
	f, _ := s.Config.Fork(w.Event.Key)
	p := w.Fork
	grant := ForkCIRetry{ForkID: f.ID, PR: p.PR, Head: in.Head, Base: in.Base, RunID: in.RunID, RunAttempt: in.RunAttempt, JobID: in.JobID}
	if err := s.Config.validateForkCIRetries(); err != nil {
		return result, err
	}
	if !slices.Contains(s.Config.ForkCIRetries, grant) {
		return result, ErrWorkDenied
	}
	facts := forkFacts(w)
	var snapshot Snapshot
	if json.Unmarshal(w.Snapshot, &snapshot) != nil {
		return result, errors.New("invalid snapshot")
	}
	pull := snapshot.Objects["pull"]
	if in.Ref != p.Branch || facts["target_sha"] != in.Base || facts["remote_head"] != in.Head || pull["head"] != in.Head || pull["base_sha"] != in.Base || pull["head_ref"] != p.Branch || pull["base"] != f.Target.Branch || !strings.EqualFold(str(pull["head_repo"]), f.Target.Repo) || pull["state"] != "open" || pull["merged"] != false {
		return result, ErrStaleWork
	}
	reader, ok := s.GH.(ciRetrySource)
	if !ok {
		return result, errors.New("GitHub source cannot verify CI run and job")
	}
	run, job, err := reader.CIRetryState(ctx, f.Target.Repo, in.RunID, in.JobID)
	if err != nil {
		return result, err
	}
	if run.ID != in.RunID || run.Attempt != in.RunAttempt || run.Head != in.Head || run.Branch != p.Branch || run.Event != "pull_request" || run.Status != "completed" || run.Conclusion != "failure" || !strings.EqualFold(run.Repository.FullName, f.Target.Repo) || !strings.EqualFold(run.HeadRepository.FullName, f.Target.Repo) || job.ID != in.JobID || job.RunID != in.RunID || job.Attempt != in.RunAttempt || job.Head != in.Head || job.Status != "completed" || job.Conclusion != "failure" {
		return result, errors.New("CI retry requires the exact failed job and completed failed run at the authorized attempt")
	}
	if !slices.ContainsFunc(run.PullRequests, func(pr ciPullRequest) bool {
		return pr.Number == p.PR && pr.Head.SHA == in.Head && pr.Base.SHA == in.Base && pr.Head.Ref == p.Branch && pr.Base.Ref == f.Target.Branch
	}) {
		return result, errors.New("CI run does not reference the current PR and authorized revisions")
	}
	// Remote reads may take time; an ownership transfer or evidence update during
	// those reads must invalidate the action just like other work operations.
	if _, err := s.CheckWork(ctx, in.WorkInput); err != nil {
		return result, err
	}
	return GitHubActionCheck{GitHubActionInput: in, Repository: f.Target.Repo, Number: p.PR, Branch: p.Branch, TargetBranch: f.Target.Branch, Permitted: true, Verification: "Only retry this exact failed job once, using run rerun --job with the returned repository/run_id/job_id. GitHub also reruns dependent jobs. Recheck the run attempt immediately before writing; do not use --failed, retry the whole workflow, or reuse this check after any state change. Verify the next attempt remotely. This read-only check is not an atomic reservation, does not execute the retry, and grants no code, PR or merge writes."}, nil
}
