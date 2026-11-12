package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ForkSpec is trusted operator configuration. Its ID identifies a continuing
// maintenance job, independently of any execution round or generated pull request.
type BranchRef struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
}
type ForkSpec struct {
	ID              string           `json:"id"`
	Upstream        BranchRef        `json:"upstream"`
	Target          BranchRef        `json:"target"`
	LocalRepository string           `json:"local_repository,omitempty"`
	Queue           *ForkQueueConfig `json:"queue,omitempty"`
}

// An operator may acknowledge one already published merge without authorizing
// future merges or changing the rule's identity. The collector still requires
// the exact tested parents and tree, the same PR head and target reachability.
type ForkMergeAcceptance struct {
	ForkID      string `json:"fork_id"`
	PR          int    `json:"pr"`
	MergeCommit string `json:"merge_commit"`
}

func acceptedForkMerge(acceptances []ForkMergeAcceptance, f ForkSpec, pr int, sha string) bool {
	if pr < 1 || !shaRE.MatchString(sha) {
		return false
	}
	return slices.ContainsFunc(acceptances, func(a ForkMergeAcceptance) bool {
		return a.ForkID == f.ID && a.PR == pr && a.MergeCommit == sha
	})
}

var forkIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
var branchRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,159}$`)

func validBranch(s string) bool {
	if !branchRE.MatchString(s) || strings.Contains(s, "..") || strings.Contains(s, "//") || strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
func (f ForkSpec) Key() string { return "fork:" + f.ID }
func (f ForkSpec) Version() string {
	// Queue policy may change without changing the maintained repository pair.
	// The complete policy is still included in the guide's proof fingerprint.
	f.Queue = nil
	return hash(canonical(f))
}
func (c Config) Fork(key string) (ForkSpec, bool) {
	for _, f := range c.Forks {
		if f.Key() == key {
			return f, true
		}
	}
	return ForkSpec{}, false
}
func (c Config) validateForks() error {
	seen, targets := map[string]bool{}, map[string]bool{}
	for _, f := range c.Forks {
		if !forkIDRE.MatchString(f.ID) || seen[f.ID] {
			return errors.New("forks require unique lowercase IDs")
		}
		seen[f.ID] = true
		if f.LocalRepository != "" && (!filepath.IsAbs(f.LocalRepository) || strings.ContainsAny(f.LocalRepository, "\x00\r\n")) {
			return errors.New("fork local_repository must be an absolute local clone path")
		}
		for _, ref := range []BranchRef{f.Upstream, f.Target} {
			if !repoRE.MatchString(ref.Repo) || strings.Contains(ref.Repo, "..") || !validBranch(ref.Branch) {
				return errors.New("invalid fork repository or branch")
			}
		}
		key := strings.ToLower(f.Target.Repo) + ":" + f.Target.Branch
		if targets[key] || (strings.EqualFold(f.Upstream.Repo, f.Target.Repo) && f.Upstream.Branch == f.Target.Branch) {
			return errors.New("fork target must be distinct and have one maintenance rule")
		}
		targets[key] = true
		g, err := c.RepositoryGuide(f.Target.Repo)
		if err != nil {
			return err
		}
		if g.ForkSyncMode == adaptedBatches && f.LocalRepository == "" {
			return errors.New("adapted batches require a complete local_repository")
		}
		if f.Queue != nil && (g.ForkSyncMode != adaptedBatches || f.Queue.MaxOpenPRs < 0 || f.Queue.MaxOpenPRs > 50) {
			return errors.New("fork queue requires adapted-batches and max_open_prs in 1..50 (0 defaults to 10)")
		}
		if g.Mode != "local-edit" || len(g.TestCommands) == 0 {
			return errors.New("fork maintenance requires an explicit local-edit target policy and regression commands")
		}
	}
	accepted := map[string]bool{}
	for _, a := range c.ForkMergeAcceptances {
		f, ok := c.Fork("fork:" + a.ForkID)
		key := fmt.Sprintf("%s:%d", a.ForkID, a.PR)
		if !ok || a.PR < 1 || !shaRE.MatchString(a.MergeCommit) || accepted[key] {
			return errors.New("fork_merge_acceptances require a configured fork, unique PR and exact merge commit SHA")
		}
		g, err := c.RepositoryGuide(f.Target.Repo)
		if err != nil {
			return err
		}
		if g.ForkSyncMode != adaptedBatches {
			return errors.New("fork_merge_acceptances apply only to already merged adapted batches")
		}
		accepted[key] = true
	}
	if err := c.validateForkProofRecoveries(); err != nil {
		return err
	}
	return c.validateForkCIRetries()
}

// Only progress needed to recover is persisted. Source and review bodies remain
// in the current snapshot, while old reports follow ordinary retention rules.
type ForkProgress struct {
	GenerationAt       int64           `json:"generation_at,omitempty"`
	IntegrationBase    string          `json:"integration_base,omitempty"`
	Queue              *ForkQueueState `json:"queue,omitempty"`
	BatchID            string          `json:"batch_id,omitempty"`
	ParentBatch        string          `json:"parent_batch,omitempty"`
	ParentHead         string          `json:"parent_head,omitempty"`
	TargetBranch       string          `json:"target_branch,omitempty"`
	QueueStale         string          `json:"queue_stale,omitempty"`
	RebuildSourceHead  string          `json:"rebuild_source_head,omitempty"`
	RetargetFrom       string          `json:"retarget_from,omitempty"`
	RebuildFromHead    string          `json:"rebuild_from_head,omitempty"`
	RebuildFromBase    string          `json:"rebuild_from_base,omitempty"`
	PendingPublishFrom string          `json:"pending_publish_from,omitempty"`
	CreatedAt          int64           `json:"created_at,omitempty"`
	ReadyAt            int64           `json:"ready_at,omitempty"`
	MergedAt           int64           `json:"merged_at,omitempty"`
	AdaptationBaseline string          `json:"adaptation_baseline,omitempty"`
	SyncMode           string          `json:"sync_mode,omitempty"`
	BatchAfter         string          `json:"batch_after,omitempty"`
	BatchCommits       []string        `json:"batch_commits,omitempty"`
	BatchReason        string          `json:"batch_reason,omitempty"`
	PrivateRepos       []string        `json:"private_repositories,omitempty"`
	Revision           int64           `json:"revision"`
	SpecVersion        string          `json:"spec_version"`
	Round              string          `json:"round"`
	Phase              string          `json:"phase"`
	Upstream           string          `json:"upstream_sha"`
	InitialTarget      string          `json:"initial_target_sha"`
	ObservedUpstream   string          `json:"observed_upstream_sha"`
	Branch             string          `json:"sync_branch"`
	PR                 int             `json:"pr"`
	LastSuccess        string          `json:"last_success_upstream_sha"`
	ValidatedHead      string          `json:"validated_head"`
	ValidatedBase      string          `json:"validated_base"`
	ValidatedTree      string          `json:"validated_tree"`
	ValidatedGuide     string          `json:"validated_guide"`
	ValidatedMapping   string          `json:"validated_mapping,omitempty"`
	ValidatedPRSection string          `json:"validated_pr_section,omitempty"`
	TestLog            string          `json:"test_log,omitempty"`
	TestLogHash        string          `json:"test_log_hash,omitempty"`
	Validation         *ForkValidation `json:"validation,omitempty"`
	NextAttempt        int64           `json:"next_attempt"`
	Retries            int             `json:"retries"`
	Wake               int             `json:"wake"`
}

type ForkObservation struct {
	IntegrationTarget string
	PrivateRepos      []string
	MergeVerified     bool
	MergeCommit       string
	Classification    string
	Upstream          string
	Target            string
	ContainsLatest    bool
	ContainsPinned    bool
	ContainsValidated bool
	PR                int
	SyncHead          string
	Objects           Objects
}

type ForkSource interface {
	CollectFork(context.Context, ForkSpec, ForkProgress) (ForkObservation, error)
}

func forkProgressName(key string) string { return "fork-progress:" + key }
func forkPRName(w Watch) string          { return "fork-pr:" + hash([]byte(w.Key())) }

// WorkDefinition resolves identity once. Generic delivery and reporting never
// need to manufacture a PR number for a maintenance job.
type WorkDefinition struct {
	Key   string
	Watch Watch
	Fork  *ForkSpec
}

func (c Config) WorkDefinition(key string) (WorkDefinition, error) {
	if f, ok := c.Fork(key); ok {
		return WorkDefinition{Key: key, Fork: &f}, nil
	}
	w, err := resourceWatch(key)
	return WorkDefinition{Key: key, Watch: w}, err
}
func (d WorkDefinition) Repository() string {
	if d.Fork != nil {
		return d.Fork.Target.Repo
	}
	return d.Watch.Repo
}

const adaptedMergeInstruction = "Merge requires separate delegation and squash; the resulting commit's sole parent must equal the tested target, and its tree must match the test proof."
const legacyAdaptedMergeInstruction = "Merge requires separate delegation and a merge commit whose parents and tree match the test proof."

func (d WorkDefinition) Guide(c Config) (RepositoryGuide, error) {
	g, err := d.guide(c, adaptedMergeInstruction)
	if err == nil && d.Fork != nil && g.ForkSyncMode == adaptedBatches {
		g.Text += "\n\n" + adaptedProvenanceInstruction
		if d.Fork.Queue != nil {
			g.ForkQueue = d.Fork.Queue
			g.Text += "\n\n" + forkQueueInstruction
		}
		g.Version = ""
		g.Version = hash(canonical(g))
	}
	return g, err
}

func (d WorkDefinition) guide(c Config, adaptedMerge string) (RepositoryGuide, error) {
	g, err := c.RepositoryGuide(d.Repository())
	if err == nil && d.Fork != nil {
		if g.ForkSyncMode == adaptedBatches {
			g.Text += "\n\nUse start-sync-batch to select the oldest next first-parent upstream commits after the documented baseline or verified completed batch. Prepare a separate branch from the target, port only the listed changes and preserve custom behavior. Record adaptations and already-present changes for human review; ancestry does not prove ported semantics. Test the actual adapted head merged with the current target. No upstream writes or direct target pushes; only the current sync branch and its PR. " + adaptedMerge
			g.Text += "\nTrusted maintenance rule: " + string(canonical(d.Fork))
		} else {
			g.Text += "\n\nKeep the sync PR head exactly equal to the pinned upstream commit; do not add integration commits or edits. Test its temporary merge with the current target to preserve custom behavior. Conflicts or failed regressions require a decision. Merge using a merge commit only when authorized, and verify the resulting tree against the tested merge tree. Upstream is read-only. Push only the current round's sync branch; merge only its PR into the configured target. No force pushes except an exact operator sync_branch_replacement grant checked against the expected remote head.\nTrusted maintenance rule: " + string(canonical(d.Fork))
		}
		g.Version = ""
		g.Version = hash(canonical(g))
	}
	return g, err
}
func (d WorkDefinition) Collect(ctx context.Context, s *Service) error {
	if d.Fork != nil {
		return s.collectFork(ctx, *d.Fork)
	}
	return s.collectWatch(ctx, d.Watch)
}

func (s *Service) collectFork(ctx context.Context, spec ForkSpec) error {
	return s.coordinateForkCollection(ctx, spec, func(ctx context.Context) error {
		return s.collectForkNow(ctx, spec)
	})
}

func (s *Service) collectForkNow(ctx context.Context, spec ForkSpec) error {
	g, ok := s.GH.(ForkSource)
	if !ok {
		return errors.New("GitHub source does not support branch maintenance")
	}
	progress, err := s.Store.ForkProgress(ctx, spec.Key())
	if err != nil {
		return err
	}
	if progress.SpecVersion != "" && progress.SpecVersion != spec.Version() {
		return errors.New("fork rule changed; use a new ID for a different branch relationship")
	}
	if progress.Queue != nil {
		err := s.collectForkQueue(ctx, spec, progress, g)
		if errors.Is(err, ErrRepositoryNotAllowed) || errors.Is(err, ErrGitHubAccessDenied) {
			return errors.Join(err, s.Store.SetBlocked(ctx, spec.Key(), true))
		}
		if err != nil {
			return err
		}
		return s.Store.SetBlocked(ctx, spec.Key(), false)
	}
	facts, err := g.CollectFork(ctx, spec, progress)
	if err != nil {
		if errors.Is(err, ErrRepositoryNotAllowed) || errors.Is(err, ErrGitHubAccessDenied) {
			return errors.Join(err, s.Store.SetBlocked(ctx, spec.Key(), true))
		}
		return err
	}
	if err = s.Store.ObserveFork(ctx, spec, progress, facts); err != nil {
		return err
	}
	return s.Store.SetBlocked(ctx, spec.Key(), false)
}

// PlanFork pins one upstream revision per round. New upstream commits only
// update the coalesced target; they cannot invalidate work already in progress.
func planFork(spec ForkSpec, p ForkProgress, o ForkObservation, now int64) ForkProgress {
	for _, repo := range o.PrivateRepos {
		if !slices.Contains(p.PrivateRepos, repo) {
			p.PrivateRepos = append(p.PrivateRepos, repo)
		}
	}
	p.SpecVersion = spec.Version()
	p.ObservedUpstream = o.Upstream
	// Adapted rounds are explicitly selected from the verified cursor. Never
	// fall back to merging the latest upstream after finishing a batch.
	if p.SyncMode == adaptedBatches && (p.Phase == "synced" || p.Phase == "restart") {
		return p
	}
	if p.Round == "" || p.Phase == "synced" || p.Phase == "restart" {
		if !o.ContainsLatest {
			last, private := p.LastSuccess, p.PrivateRepos
			round := hash([]byte(fmt.Sprintf("%s:%s:%s:%d", spec.Key(), o.Upstream, o.Target, p.Revision+1)))
			p = ForkProgress{PrivateRepos: private, Revision: p.Revision, SpecVersion: spec.Version(), Round: round, Phase: "working", Upstream: o.Upstream, InitialTarget: o.Target, ObservedUpstream: o.Upstream, Branch: "ghraft/sync/" + spec.ID + "/" + round[:12], LastSuccess: last, NextAttempt: now + int64((4 * time.Hour).Seconds())}
		}
		return p
	}
	if o.PR > 0 {
		p.PR = o.PR
	}
	if pull := o.Objects["pull"]; pull != nil && pull["state"] == "closed" && pull["merged"] != true {
		p.Phase, p.NextAttempt = "needs-decision", 0
	}
	if recoverForkValidation(&p, now) {
		return p
	}
	if p.NextAttempt > 0 && now >= p.NextAttempt {
		p.Wake++
		p.Retries++
		if p.Retries >= 5 {
			p.Phase, p.NextAttempt = "needs-decision", 0
		} else {
			p.NextAttempt = now + int64((time.Hour * time.Duration(p.Retries)).Seconds())
		}
	}
	return p
}

func forkSnapshot(spec ForkSpec, p ForkProgress, o ForkObservation) []byte {
	objects := Objects{}
	_ = json.Unmarshal(canonical(o.Objects), &objects)
	if objects == nil {
		objects = Objects{}
	}
	objects["fork"] = map[string]any{"id": spec.ID, "upstream_repo": spec.Upstream.Repo, "upstream_branch": spec.Upstream.Branch, "target_repo": spec.Target.Repo, "target_branch": spec.Target.Branch, "round": p.Round, "upstream_sha": p.Upstream, "initial_target_sha": p.InitialTarget, "target_sha": o.Target, "sync_branch": p.Branch, "pr": p.PR, "remote_head": o.SyncHead, "contains_pinned": o.ContainsPinned, "contains_validated": o.ContainsValidated, "merge_verified": o.MergeVerified, "merge_commit_sha": o.MergeCommit, "wake": p.Wake}
	if p.SyncMode == adaptedBatches {
		facts := objects["fork"]
		facts["sync_mode"], facts["batch_after"], facts["batch_commits"], facts["batch_reason"] = p.SyncMode, p.BatchAfter, p.BatchCommits, p.BatchReason
		facts["observed_upstream_sha"] = o.Upstream
		facts["adaptation_baseline"] = p.AdaptationBaseline
		facts["next_batch_ready"] = p.Phase == "synced" && p.Upstream != o.Upstream
	}
	if p.Queue != nil {
		objects["fork"]["target_branch"] = forkTargetBranch(spec, p)
		objects["fork"]["integration_target_sha"] = o.IntegrationTarget
		objects["fork"]["integration_branch"] = spec.Target.Branch
		objects["fork"]["batch_id"] = p.BatchID
		queueSnapshot(p, objects)
		objects["fork-queue"]["limit"] = spec.queueLimit()
	}
	return canonical(Snapshot{spec.Key(), o.Classification, objects})
}

func forkFacts(w WorkContext) map[string]any {
	var snap Snapshot
	_ = json.Unmarshal(w.Snapshot, &snap)
	return snap.Objects["fork"]
}

func (s *Service) checkForkAccess(ctx context.Context, f ForkSpec) error {
	p, err := s.Store.ForkProgress(ctx, f.Key())
	if err != nil {
		return err
	}
	if p.SpecVersion != "" && p.SpecVersion != f.Version() {
		return ErrWorkDenied
	}
	blocked, err := s.Store.WatchBlocked(ctx, f.Key())
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if blocked {
		return ErrWorkDenied
	}
	for _, repo := range p.PrivateRepos {
		if !privateRepositoryAllowed(s.Config.PrivateRepositories, repo) {
			return ErrWorkDenied
		}
	}
	g, ok := s.GH.(interface {
		RepositoryClassification(context.Context, string) (string, error)
	})
	if !ok {
		return ErrWorkDenied
	}
	if err := s.GH.CheckIdentity(ctx); err != nil {
		return sourceAccessError(err)
	}
	for _, repo := range []string{f.Upstream.Repo, f.Target.Repo} {
		if _, err := g.RepositoryClassification(ctx, repo); err != nil {
			return sourceAccessError(err)
		}
	}
	return nil
}

func (c Config) managedPR(w Watch, snapshot Snapshot) bool {
	pull := snapshot.Objects["pull"]
	for _, f := range c.Forks {
		queueBase := f.Queue != nil && strings.HasPrefix(str(pull["base"]), "ghraft/sync/"+f.ID+"/")
		if strings.EqualFold(w.Repo, f.Target.Repo) && strings.EqualFold(str(pull["head_repo"]), f.Target.Repo) && (pull["base"] == f.Target.Branch || queueBase) && strings.HasPrefix(str(pull["head_ref"]), "ghraft/sync/"+f.ID+"/") {
			return true
		}
	}
	return false
}

func (r *Raft) forkNotice(ev Event, f ForkSpec) (eventNotice, error) {
	var p struct {
		Classification string       `json:"data_classification"`
		Subject        eventSubject `json:"subject"`
		Changes        []Change     `json:"changes"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return eventNotice{}, err
	}
	if p.Classification == "private" && !r.Config.PrivateNotificationDetails {
		return eventNotice{"Fork 维护有更新 · 私有仓库", "维护进度或同步 PR 有变化；请从本机上下文读取详情。", "私有 fork 维护"}, nil
	}
	subject := noticeText(f.Target.Repo+" · "+f.Target.Branch, 160)
	if p.Subject.Kind == "Fork" && p.Subject.Title != "" {
		subject = noticeText(p.Subject.Title, 160)
	}
	lines := []string{"上游：" + noticeMarkdown(f.Upstream.Repo+" · "+f.Upstream.Branch), "目标：" + noticeMarkdown(subject)}
	for _, c := range p.Changes {
		if c.Object == "fork" && c.After != nil {
			a := obj(c.After)
			if before := obj(c.Before); before != nil && before["wake"] != a["wake"] {
				lines = append(lines, "定时跟进检查已到期。请核对当前维护进度、fork.validation、日志和 PR/CI 状态；输入未变且证明仍有效时无需重测，测试仍运行时不要重复启动，中断不能视为通过。")
			}
			if a["sync_mode"] == adaptedBatches {
				commits, _ := a["batch_commits"].([]any)
				lines = append(lines, fmt.Sprintf("本批按上游主线顺序移植 %d 项变更。", len(commits)))
			}
			if sha := str(a["upstream_sha"]); shaRE.MatchString(sha) {
				lines = append(lines, "本轮整合上游提交：`"+sha[:12]+"`")
			}
			if sha := str(a["target_sha"]); shaRE.MatchString(sha) {
				lines = append(lines, "当前目标提交：`"+sha[:12]+"`")
			}
		}
	}
	prChanges := []Change{}
	for _, c := range p.Changes {
		if c.Object != "fork" {
			prChanges = append(prChanges, c)
		}
	}
	if len(prChanges) > 0 {
		for i, c := range describeChanges(prChanges, "PR", false, p.Classification != "private") {
			if i == 6 {
				break
			}
			lines = append(lines, "- "+noticeMarkdown(c.text))
		}
	}
	lines = append(lines, "处理前读取当前维护进度；已有同步 PR 的评审与检查仍由本工作的负责人处理。", "[查看目标仓库](https://github.com/"+f.Target.Repo+")")
	return eventNotice{"Fork 维护有更新 · " + subject, strings.Join(lines, "\n\n"), subject}, nil
}
