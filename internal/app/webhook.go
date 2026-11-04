package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

const maxWebhookBody = 25 << 20

var webhookIDRE = regexp.MustCompile(`(?i)^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

type webhookRepo struct {
	Name    string `json:"full_name"`
	Private *bool  `json:"private"`
}

type webhookPull struct {
	Number int `json:"number"`
	Head   struct {
		Ref  string      `json:"ref"`
		SHA  string      `json:"sha"`
		Repo webhookRepo `json:"repo"`
	} `json:"head"`
}

type webhookCheck struct {
	HeadSHA      string        `json:"head_sha"`
	PullRequests []webhookPull `json:"pull_requests"`
}

type webhookPayload struct {
	Repository  webhookRepo  `json:"repository"`
	Number      int          `json:"number"`
	Ref         string       `json:"ref"`
	RefType     string       `json:"ref_type"`
	SHA         string       `json:"sha"`
	PullRequest webhookPull  `json:"pull_request"`
	CheckRun    webhookCheck `json:"check_run"`
	CheckSuite  webhookCheck `json:"check_suite"`
	WorkflowRun webhookCheck `json:"workflow_run"`
	Issue       struct {
		Number int `json:"number"`
	} `json:"issue"`
}

func (s *Service) webhookSignal() chan struct{} {
	s.webhookOnce.Do(func() { s.webhookWake = make(chan struct{}, 1) })
	return s.webhookWake
}

// Authenticate the original bytes before decoding. No GitHub calls, model
// execution, body logging or user-content persistence occur on this path.
func (s *Service) receiveWebhook(w http.ResponseWriter, r *http.Request) {
	if s.Config.GitHubSource != "webhook" {
		http.NotFound(w, r)
		return
	}
	if len(s.WebhookSecret) < 32 {
		fail(w, 503, "WEBHOOK_NOT_CONFIGURED")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		fail(w, 415, "JSON_REQUIRED")
		return
	}
	if r.Header.Get("Content-Encoding") != "" && r.Header.Get("Content-Encoding") != "identity" {
		fail(w, 415, "CONTENT_ENCODING_NOT_SUPPORTED")
		return
	}
	id := strings.ToLower(r.Header.Get("X-GitHub-Delivery"))
	if !webhookIDRE.MatchString(id) {
		fail(w, 400, "INVALID_DELIVERY_ID")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		fail(w, 413, "INVALID_WEBHOOK_BODY")
		return
	}
	signature, err := hex.DecodeString(strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256="))
	mac := hmac.New(sha256.New, s.WebhookSecret)
	mac.Write(body)
	if err != nil || !strings.HasPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=") || !hmac.Equal(signature, mac.Sum(nil)) {
		fail(w, 401, "INVALID_WEBHOOK_SIGNATURE")
		return
	}
	var payload *webhookPayload
	if err = json.Unmarshal(body, &payload); err != nil || payload == nil {
		fail(w, 400, "INVALID_WEBHOOK_JSON")
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	if event == "ping" {
		ok(w, map[string]bool{"received": true})
		return
	}
	if len(event) == 0 || len(event) > 64 {
		fail(w, 400, "INVALID_WEBHOOK_EVENT")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	keys, err := s.webhookTargets(ctx, event, *payload)
	if err != nil {
		fail(w, 503, "WEBHOOK_STORAGE_UNAVAILABLE")
		return
	}
	accepted, err := s.Store.AcceptWebhook(ctx, WebhookDelivery{ID: id, Hash: hash(body), Event: event, Keys: keys})
	if errors.Is(err, ErrWebhookConflict) {
		fail(w, 409, "DELIVERY_CONTENT_CHANGED")
		return
	}
	if err != nil {
		fail(w, 503, "WEBHOOK_STORAGE_UNAVAILABLE")
		return
	}
	if accepted && len(keys) > 0 {
		select {
		case s.webhookSignal() <- struct{}{}:
		default:
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "duplicate": !accepted})
}

// The payload supplies routing hints only. Fresh API reads still determine
// state and authorization; an event never discovers or authorizes new work.
func (s *Service) webhookTargets(ctx context.Context, event string, p webhookPayload) ([]string, error) {
	repo := p.Repository.Name
	if !repoRE.MatchString(repo) || strings.Contains(repo, "..") || p.Repository.Private == nil || (*p.Repository.Private && !privateRepositoryAllowed(s.Config.PrivateRepositories, repo)) {
		return nil, nil
	}
	numbers := []int{p.Number, p.Issue.Number, p.PullRequest.Number}
	var sha, branch string
	kind := ""
	switch event {
	case "issues", "issue_comment", "pull_request", "pull_request_review", "pull_request_review_comment", "pull_request_review_thread":
		kind = "number"
	case "check_run", "check_suite", "workflow_run":
		kind = "check"
		check := p.CheckRun
		if event == "check_suite" {
			check = p.CheckSuite
		} else if event == "workflow_run" {
			check = p.WorkflowRun
		}
		sha = check.HeadSHA
		for _, pull := range check.PullRequests {
			numbers = append(numbers, pull.Number)
		}
	case "status":
		kind, sha = "check", p.SHA
	case "push":
		if !strings.HasPrefix(p.Ref, "refs/heads/") {
			return nil, nil
		}
		kind, branch = "branch", strings.TrimPrefix(p.Ref, "refs/heads/")
	case "create", "delete":
		if p.RefType != "branch" {
			return nil, nil
		}
		kind, branch = "branch", p.Ref
	case "repository":
		kind = "repository"
	default:
		return nil, nil
	}
	// CI notifications can omit PR numbers and race the snapshot update for
	// a new head. In that case, refresh the repository's configured resources
	// instead of discarding a notification because its SHA is not cached yet.
	unassociatedCheck := kind == "check" && shaRE.MatchString(sha) && !slices.ContainsFunc(numbers, func(n int) bool { return n > 0 })
	keys := []string{}
	for _, watch := range s.Config.Watches {
		if !strings.EqualFold(watch.Repo, repo) {
			continue
		}
		match := kind == "repository" || unassociatedCheck || (kind != "branch" && slices.Contains(numbers, watch.Number))
		if !match && kind == "branch" {
			snap, err := s.webhookSnapshot(ctx, watch.Key())
			if err != nil {
				return nil, err
			}
			pull := snap.Objects["pull"]
			match = pull["base"] == branch || (pull["head_ref"] == branch && strings.EqualFold(str(pull["head_repo"]), repo))
		}
		if match {
			keys = append(keys, watch.Key())
		}
	}
	for _, fork := range s.Config.Forks {
		upstream, target := strings.EqualFold(repo, fork.Upstream.Repo), strings.EqualFold(repo, fork.Target.Repo)
		if !upstream && !target {
			continue
		}
		progress, err := s.Store.ForkProgress(ctx, fork.Key())
		if err != nil {
			return nil, err
		}
		match := kind == "repository" || (kind == "branch" && ((upstream && branch == fork.Upstream.Branch) || (target && (branch == fork.Target.Branch || branch == progress.Branch))))
		if target && kind != "branch" {
			match = match || unassociatedCheck || (progress.PR > 0 && slices.Contains(numbers, progress.PR))
			match = match || (kind == "number" && progress.Branch != "" && p.PullRequest.Head.Ref == progress.Branch && strings.EqualFold(p.PullRequest.Head.Repo.Name, fork.Target.Repo))
		}
		if match {
			keys = append(keys, fork.Key())
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys), nil
}

func (s *Service) webhookSnapshot(ctx context.Context, key string) (Snapshot, error) {
	body, _, err := s.Store.Snapshot(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return Snapshot{}, nil
	}
	if err != nil {
		return Snapshot{}, err
	}
	var snap Snapshot
	err = json.Unmarshal(body, &snap)
	return snap, err
}
