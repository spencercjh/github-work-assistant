package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

var reviewThreadIDRE = regexp.MustCompile(`^[A-Za-z0-9_+/=-]{1,256}$`)

type reviewRepository struct {
	Name string `json:"nameWithOwner"`
}

type reviewPR struct {
	Number         int              `json:"number"`
	State          string           `json:"state"`
	Head           string           `json:"headRefOid"`
	Base           string           `json:"baseRefOid"`
	HeadRef        string           `json:"headRefName"`
	BaseRef        string           `json:"baseRefName"`
	Repository     reviewRepository `json:"repository"`
	HeadRepository reviewRepository `json:"headRepository"`
}

type reviewThread struct {
	ID         string `json:"id"`
	IsResolved *bool  `json:"isResolved"`
}

type reviewThreadSource interface {
	ReviewThread(context.Context, string, int, string) (reviewPR, reviewThread, error)
}

// Query through the authorized repository/PR, never a caller-supplied global
// node or query. This avoids reading threads from unrelated private repositories.
const forkReviewThreadQuery = `query ForkReviewThread($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    nameWithOwner
    pullRequest(number: $number) {
      number state headRefOid baseRefOid headRefName baseRefName
      repository { nameWithOwner }
      headRepository { nameWithOwner }
      reviewThreads(first: 100, after: $after) {
        nodes { id isResolved }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

func (g GH) ReviewThread(ctx context.Context, repo string, number int, threadID string) (reviewPR, reviewThread, error) {
	var zeroPR reviewPR
	var zeroThread reviewThread
	if !repoRE.MatchString(repo) || strings.Contains(repo, "..") || number < 1 || !reviewThreadIDRE.MatchString(threadID) {
		return zeroPR, zeroThread, ErrWorkDenied
	}
	if _, err := g.RepositoryClassification(ctx, repo); err != nil {
		return zeroPR, zeroThread, err
	}
	parts := strings.SplitN(repo, "/", 2)
	variables := map[string]any{"owner": parts[0], "name": parts[1], "number": number, "after": nil}
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		body := canonical(map[string]any{"query": forkReviewThreadQuery, "variables": variables})
		b, err := g.readCommand(ctx, []string{"api", "--hostname", "github.com", "--method", "POST", "graphql", "--input", "-"}, bytes.NewReader(body))
		if err != nil {
			return zeroPR, zeroThread, err
		}
		var response struct {
			Errors []json.RawMessage `json:"errors"`
			Data   struct {
				Repository *struct {
					Name string `json:"nameWithOwner"`
					Pull *struct {
						reviewPR
						Threads *struct {
							Nodes []reviewThread `json:"nodes"`
							Page  struct {
								More   *bool  `json:"hasNextPage"`
								Cursor string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		if json.Unmarshal(b, &response) != nil || len(response.Errors) != 0 || response.Data.Repository == nil {
			return zeroPR, zeroThread, errors.New("review thread query failed or returned incomplete data")
		}
		r := response.Data.Repository
		p := r.Pull
		if !strings.EqualFold(r.Name, repo) || p == nil || p.Number != number || !strings.EqualFold(p.Repository.Name, repo) || p.Threads == nil || p.Threads.Page.More == nil {
			return zeroPR, zeroThread, errors.New("review thread response does not identify the requested repository and PR")
		}
		for _, thread := range p.Threads.Nodes {
			if thread.ID == threadID {
				if thread.IsResolved == nil {
					return zeroPR, zeroThread, errors.New("review thread resolution state is unknown")
				}
				return p.reviewPR, thread, nil
			}
		}
		if !*p.Threads.Page.More {
			return zeroPR, zeroThread, errors.New("review thread does not belong to this sync PR")
		}
		cursor := p.Threads.Page.Cursor
		if cursor == "" || seen[cursor] {
			return zeroPR, zeroThread, errors.New("review thread pagination is incomplete")
		}
		seen[cursor], variables["after"] = true, cursor
	}
	return zeroPR, zeroThread, errors.New("review thread pagination exceeds the read limit")
}

// Called after the same current-workspace, test-proof and provenance checks as
// a code write, and after confirming that exact head was pushed to the open PR.
func (s *Service) checkForkReviewThread(ctx context.Context, w WorkContext, in GitHubActionInput) (GitHubActionCheck, error) {
	var result GitHubActionCheck
	if !w.Guide.ResolveReviewThreads || !reviewThreadIDRE.MatchString(in.ReviewThreadID) || strings.TrimSpace(in.Body) == "" || len(in.Body) > 8000 {
		return result, errors.New("review resolution requires resolve_review_threads, one thread ID and a 1..8000 byte fix explanation")
	}
	f, _ := s.Config.Fork(w.Event.Key)
	reader, ok := s.GH.(reviewThreadSource)
	if !ok {
		return result, errors.New("GitHub source cannot verify review threads")
	}
	pr, thread, err := reader.ReviewThread(ctx, f.Target.Repo, w.Fork.PR, in.ReviewThreadID)
	if err != nil {
		return result, err
	}
	if thread.ID != in.ReviewThreadID || thread.IsResolved == nil || *thread.IsResolved || pr.Number != w.Fork.PR || pr.State != "OPEN" || pr.Head != in.Head || pr.Base != in.Base || pr.HeadRef != w.Fork.Branch || pr.BaseRef != f.Target.Branch || !strings.EqualFold(pr.Repository.Name, f.Target.Repo) || !strings.EqualFold(pr.HeadRepository.Name, f.Target.Repo) {
		return result, errors.New("resolve requires an unresolved thread on the current tested open sync PR")
	}
	// Slow remote reads cannot outlive an ownership, guidance, evidence or proof
	// change. This is still a preflight check, not a GitHub-side atomic write.
	current, err := s.CheckWork(ctx, in.WorkInput)
	if err != nil {
		return result, err
	}
	if current.Fork == nil || current.Fork.Revision != w.Fork.Revision {
		return result, ErrStaleWork
	}
	return GitHubActionCheck{GitHubActionInput: in, Repository: f.Target.Repo, Number: w.Fork.PR, Branch: w.Fork.Branch, TargetBranch: f.Target.Branch, Permitted: true, Verification: "Resolve only review_thread_id on this tested, published PR head after reading the complete latest discussion and confirming the stated fix addresses it. Outdated status alone is not a fix. Use the authorized GitHub/Stamp identity and resolveReviewThread; no comment, review, PR-state or merge permission is implied. Immediately recheck the PR head/base and discussion before writing, then verify isResolved=true. This check performs no write, cannot judge the semantic fix, and is not an atomic reservation; repeat it after any change. Include the fix explanation and confirmed resolution in the work report."}, nil
}
