package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

func workspaceLock(metadata string) (*os.File, error) {
	f, err := os.OpenFile(metadata+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("workspace operation already running; retry after it completes")
	}
	return f, nil
}

// Use a separate workspace identity so an upgrade never rewrites an existing
// integration branch or its uncommitted work. Each selected batch gets a new
// round identity; existing source worktrees and their edits are never reset.
func (s *Service) forkWorkspacePaths(round string) (string, string, error) {
	return s.workspacePaths(hash([]byte(round + ":upstream-only")))
}

func forkLocalBranch(p *ForkProgress) string {
	if p.RebuildFromBase != "" {
		return p.Branch + "-generation-" + p.Round[:12]
	}
	return p.Branch
}

func forkSourceBase(p *ForkProgress) string {
	if p.SyncMode == adaptedBatches {
		return p.InitialTarget
	}
	return p.Upstream
}

func (s *Service) prepareForkWorkspace(ctx context.Context, w WorkContext, in WorkInput, repository string) (Workspace, error) {
	if err := requireForkWork(w, in); err != nil {
		return Workspace{}, err
	}
	p := w.Fork
	f, _ := s.Config.Fork(w.Event.Key)
	path, metadata, err := s.forkWorkspacePaths(p.Round)
	if err != nil {
		return Workspace{}, err
	}
	lock, err := workspaceLock(metadata)
	if err != nil {
		return Workspace{}, err
	}
	defer lock.Close()
	repository, err = filepath.Abs(repository)
	if err != nil {
		return Workspace{}, err
	}
	remote, err := gitLocal(ctx, repository, "remote", "get-url", "origin")
	if err != nil || !githubOriginMatches(remote, f.Target.Repo) {
		return Workspace{}, errors.New("local origin does not match the fork target")
	}
	for _, sha := range []string{p.Upstream, str(forkFacts(w)["target_sha"])} {
		if !shaRE.MatchString(sha) {
			return Workspace{}, errors.New("missing pinned fork revision")
		}
		if _, err = gitLocal(ctx, repository, "cat-file", "-e", sha+"^{commit}"); err != nil {
			return Workspace{}, errors.New("fetch the pinned upstream and current target commits into the local clone first")
		}
	}
	workspace := Workspace{WorkInput: in, Resource: w.Event.Key, Repository: repository, Path: path, Branch: p.Branch, LocalBranch: forkLocalBranch(p), RebuildSourceHead: p.RebuildSourceHead, RebuildFromBase: p.RebuildFromBase, Head: forkSourceBase(p), Base: p.InitialTarget, TestCommands: w.Guide.TestCommands}
	if b, err := os.ReadFile(metadata); err == nil {
		var saved Workspace
		if json.Unmarshal(b, &saved) != nil || saved.Repository != repository || saved.Resource != w.Event.Key || saved.Path != path || saved.Branch != p.Branch || saved.Head != forkSourceBase(p) || saved.Base != p.InitialTarget {
			return Workspace{}, errors.New("workspace metadata differs from this sync round")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		out, e := os.OpenFile(metadata, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return Workspace{}, e
		}
		_, e = out.Write(canonical(workspace))
		if e == nil {
			e = out.Sync()
		}
		closeErr := out.Close()
		if e != nil {
			return Workspace{}, e
		}
		if closeErr != nil {
			return Workspace{}, closeErr
		}
	} else {
		return Workspace{}, err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		head := p.Upstream
		if r := w.Guide.SyncMergeResolution; p.SyncMode == "" && r != nil && r.Upstream == p.Upstream && r.Base == str(forkFacts(w)["target_sha"]) {
			if err = verifyMergeResolution(ctx, repository, r, p.Upstream, r.Base, r.Head); err != nil {
				return Workspace{}, err
			}
			head = r.Head
		}
		args := []string{"worktree", "add", "--detach", path, head}
		if p.SyncMode == adaptedBatches {
			args = []string{"worktree", "add", "-b", forkLocalBranch(p), path, p.InitialTarget}
		}
		if _, err = gitLocal(ctx, repository, args...); err != nil {
			return Workspace{}, err
		}
	} else if err != nil {
		return Workspace{}, err
	}
	if p.SyncMode == adaptedBatches {
		if err = prepareForkMapping(metadata+".commits.json", p); err != nil {
			return Workspace{}, err
		}
	}
	return s.verifyForkWorkspace(ctx, w, in)
}

func (s *Service) verifyForkWorkspace(ctx context.Context, w WorkContext, in WorkInput) (Workspace, error) {
	if err := requireForkWork(w, in); err != nil {
		return Workspace{}, err
	}
	p := w.Fork
	path, metadata, err := s.forkWorkspacePaths(p.Round)
	if err != nil {
		return Workspace{}, err
	}
	data, err := os.ReadFile(metadata)
	if err != nil {
		return Workspace{}, err
	}
	var workspace Workspace
	if json.Unmarshal(data, &workspace) != nil || workspace.Path != path || workspace.Resource != w.Event.Key || workspace.Branch != p.Branch || workspace.Head != forkSourceBase(p) || workspace.Base != p.InitialTarget {
		return workspace, errors.New("workspace belongs to another sync round")
	}
	f, _ := s.Config.Fork(w.Event.Key)
	var repo *forkGitRepository
	if p.SyncMode == adaptedBatches {
		localSpec := f
		localSpec.LocalRepository = workspace.Repository
		repo, err = openForkGitRepository(ctx, localSpec)
		if err != nil {
			return workspace, err
		}
		defer repo.close()
	}
	// Read source identity and cleanliness only after any ancestry-lock wait.
	actual, err := filepath.EvalSymlinks(path)
	if err != nil || actual != path {
		return workspace, errors.New("workspace path changed")
	}
	remote, err := gitLocal(ctx, path, "remote", "get-url", "origin")
	if err != nil || !githubOriginMatches(remote, f.Target.Repo) {
		return workspace, errors.New("workspace origin changed")
	}
	branch, err := gitLocal(ctx, path, "branch", "--show-current")
	wantBranch := ""
	if p.SyncMode == adaptedBatches {
		wantBranch = forkLocalBranch(p)
	}
	if err != nil || branch != wantBranch {
		return workspace, errors.New("source worktree branch differs from the sync round")
	}
	head, err := gitLocal(ctx, path, "rev-parse", "HEAD")
	if err != nil || !shaRE.MatchString(head) {
		return workspace, errors.New("source worktree has no valid commit")
	}
	if p.SyncMode == adaptedBatches {
		contains, err := repo.contains(ctx, p.InitialTarget, head)
		if err != nil || !contains {
			return workspace, errors.New("adapted source must retain its initial target history")
		}
	} else if head != p.Upstream {
		if err := verifyMergeResolution(ctx, path, w.Guide.SyncMergeResolution, p.Upstream, str(forkFacts(w)["target_sha"]), head); err != nil {
			return workspace, err
		}
	}
	workspace.Head = head
	if err = cleanCommitted(ctx, path); err != nil {
		return workspace, err
	}
	workspace.WorkInput, workspace.Base, workspace.TestCommands = in, str(forkFacts(w)["target_sha"]), w.Guide.TestCommands
	workspace.Conflicts = []string{}
	workspace.Verification = "Source is the exact pinned upstream commit. test-worktree validates a separate temporary merge with the current target; push only this source HEAD to the returned branch."
	if p.SyncMode == "" && head != p.Upstream {
		workspace.Verification = "Source is the exact operator-authorized resolution merge, preserving the pinned upstream history. test-worktree must validate this head against the authorized current target; push only this HEAD to the returned branch."
	}
	if p.SyncMode == adaptedBatches {
		workspace.MappingFile = metadata + ".commits.json"
		workspace.Verification = "Adapted source retains the initial target. " + adaptedProvenanceInstruction
	}
	return workspace, nil
}

// Tests run against the prospective merge tree, without creating a commit or
// changing the source ref. Conflicts require a decision, never silent edits.
func (s *Service) forkTestWorktree(ctx context.Context, source Workspace) (string, func(), error) {
	path, err := os.MkdirTemp(s.Config.WorkspaceRoot, "fork-test-")
	if err != nil {
		return "", nil, err
	}
	if err = os.Remove(path); err != nil {
		return "", nil, err
	}
	cleanup := func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = gitLocal(c, source.Path, "worktree", "remove", "--force", path)
	}
	if _, err = gitLocal(ctx, source.Path, "worktree", "add", "--detach", path, source.Base); err != nil {
		cleanup()
		return "", nil, err
	}
	if _, err = gitLocal(ctx, path, "merge", "--no-commit", "--no-ff", source.Head); err != nil {
		cleanup()
		return "", nil, errors.New("sync source cannot merge cleanly with the current target; follow the configured synchronization mode to resolve it")
	}
	return path, cleanup, nil
}

func forkTestTree(ctx context.Context, path, base string) (string, error) {
	if head, err := gitLocal(ctx, path, "rev-parse", "HEAD"); err != nil || head != base {
		return "", errors.New("test worktree target changed")
	}
	if _, err := gitLocal(ctx, path, "diff", "--quiet"); err != nil {
		return "", errors.New("tests changed tracked files")
	}
	if untracked, err := gitLocal(ctx, path, "ls-files", "--others", "--exclude-standard"); err != nil || untracked != "" {
		return "", errors.New("test worktree contains untracked files")
	}
	tree, err := gitLocal(ctx, path, "write-tree")
	if err != nil || !shaRE.MatchString(tree) {
		return "", errors.New("test worktree has no resolved merge tree")
	}
	return tree, nil
}

func cleanCommitted(ctx context.Context, path string) error {
	if merge, _ := gitLocal(ctx, path, "rev-parse", "--verify", "MERGE_HEAD"); merge != "" {
		return errors.New("commit the merge before validation")
	}
	status, err := gitLocal(ctx, path, "status", "--porcelain", "--untracked-files=all")
	if err != nil || status != "" {
		return errors.New("validation requires a clean committed worktree")
	}
	return nil
}

type WorkspaceTest struct {
	Attempt    string `json:"attempt,omitempty"`
	Head       string `json:"head"`
	Tree       string `json:"tree"`
	Base       string `json:"base"`
	Passed     bool   `json:"passed"`
	Log        string `json:"log"`
	LogHash    string `json:"log_hash"`
	PRBodyFile string `json:"pr_body_file,omitempty"`
}

// Test commands come only from the trusted local guide. Tests still execute
// repository code: this is a credential-reduced process, not an OS sandbox.
func (s *Service) TestWorkspace(ctx context.Context, in WorkInput) (result WorkspaceTest, retErr error) {
	w, err := s.checkWork(ctx, in, "test")
	if err != nil {
		return result, err
	}
	if err = requireForkWork(w, in); err != nil {
		return result, err
	}
	_, metadata, err := s.forkWorkspacePaths(w.Fork.Round)
	if err != nil {
		return result, err
	}
	lock, err := workspaceLock(metadata)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	workspace, err := s.verifyForkWorkspace(ctx, w, in)
	if err != nil {
		return result, err
	}
	if err = cleanCommitted(ctx, workspace.Path); err != nil {
		return result, err
	}
	if w.Fork.SyncMode == adaptedBatches && workspace.Head == w.Fork.InitialTarget {
		return result, errors.New("adapt and commit this batch before validation")
	}
	if len(w.Guide.TestCommands) == 0 {
		return result, errors.New("no regression commands configured")
	}
	result.Head, result.Base = workspace.Head, workspace.Base
	result.Log = metadata + fmt.Sprintf(".test-%d.log", time.Now().UnixNano())
	result.Attempt = hash([]byte(result.Log))
	log, err := os.OpenFile(result.Log, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	defer log.Close()
	output := &validationLog{dst: log}
	fmt.Fprintf(output, "attempt: %s\nsource: %s\ntarget: %s\n", result.Attempt, result.Head, result.Base)
	if err = output.Error(); err != nil {
		return result, err
	}
	if err = log.Sync(); err != nil {
		return result, err
	}
	testCtx, cancel := context.WithTimeout(ctx, forkTestTimeout)
	defer cancel()
	ctx = testCtx
	p := *w.Fork
	if p.Queue != nil {
		p.PendingPublishFrom = str(forkFacts(w)["remote_head"])
	}
	p.ValidatedHead, p.ValidatedBase, p.ValidatedTree, p.ValidatedGuide, p.TestLog, p.TestLogHash = "", "", "", "", "", ""
	p.ValidatedMapping, p.ValidatedPRSection = "", ""
	p.ValidatedResolution = nil
	now := time.Now()
	p.Validation = &ForkValidation{Executor: in.Executor, TaskID: in.TaskID, ID: result.Attempt, Round: p.Round, Head: result.Head, Base: result.Base, Guide: in.GuideVersion, Log: result.Log, State: "running", StartedAt: now.Unix(), HeartbeatAt: now.Unix(), Deadline: now.Add(forkTestTimeout + forkTestLease).Unix()}
	// Starting validation resumes execution, even after an earlier review wait.
	p.Phase, p.NextAttempt = "working", p.Validation.Deadline
	if err = s.Store.SaveForkProgress(ctx, w.Event.Key, p.Revision, p, nil); err != nil {
		return result, err
	}
	key, round := w.Event.Key, p.Round
	stopHeartbeat := s.keepForkValidation(ctx, key, round, result.Attempt, cancel)
	defer func() {
		stopHeartbeat()
		if retErr == nil {
			return
		}
		fmt.Fprintf(output, "validation did not save a passing proof: %v\n", retErr)
		_ = log.Sync()
		if logged, e := os.ReadFile(result.Log); e == nil {
			result.LogHash = hash(logged)
		}
		state := "failed"
		if ctx.Err() != nil {
			state = "interrupted"
		}
		// Cancellation must not prevent recording the interruption. SIGKILL is
		// covered separately by the persisted heartbeat/deadline.
		c, end := context.WithTimeout(context.Background(), 5*time.Second)
		defer end()
		if e := s.Store.UpdateForkValidation(c, key, round, result.Attempt, state); e != nil && !errors.Is(e, ErrStaleWork) {
			retErr = errors.Join(retErr, fmt.Errorf("record validation interruption: %w", e))
		}
	}()
	var mappingHash, prSection string
	f, _ := s.Config.Fork(w.Event.Key)
	if p.SyncMode == adaptedBatches {
		mappingHash, prSection, err = validateForkMapping(ctx, f, &p, workspace)
		if err != nil {
			return result, err
		}
	}
	testPath, cleanup, err := s.forkTestWorktree(ctx, workspace)
	if err != nil {
		return result, err
	}
	defer cleanup()
	result.Tree, err = forkTestTree(ctx, testPath, result.Base)
	if err != nil {
		return result, err
	}
	var resolution *SyncMergeResolution
	if p.SyncMode == "" && result.Head != p.Upstream {
		r := w.Guide.SyncMergeResolution
		if !r.matches(p.Upstream, result.Base, result.Head, result.Tree) {
			return result, errors.New("prospective merge tree differs from the authorized resolution")
		}
		copy := *r
		resolution = &copy
	}
	home, err := os.MkdirTemp(s.Config.WorkspaceRoot, "test-home-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(home)
	fmt.Fprintf(output, "merge tree: %s\n", result.Tree)
	for _, argv := range w.Guide.TestCommands {
		fmt.Fprintf(output, "command: %s\n", string(canonical(argv)))
		cmd := exec.CommandContext(testCtx, argv[0], argv[1:]...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			// Trusted test wrappers often spawn compilers and test workers.
			// Cancel the group so an interrupted wrapper does not leave them
			// running after its workspace lock has been released.
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		cmd.Dir = testPath
		cmd.WaitDelay = 5 * time.Second
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "LANG=C.UTF-8", "CI=true", "PYTHONDONTWRITEBYTECODE=1"}
		cmd.Stdout, cmd.Stderr = output, output
		if err = cmd.Run(); err != nil {
			break
		}
	}
	if logErr := output.Error(); logErr != nil {
		return result, logErr
	}
	if syncErr := log.Sync(); syncErr != nil {
		return result, syncErr
	}
	logged, readErr := os.ReadFile(result.Log)
	if readErr != nil {
		return result, readErr
	}
	result.LogHash = hash(logged)
	if err != nil {
		return result, fmt.Errorf("regression command failed; inspect %s", result.Log)
	}
	if tree, err := forkTestTree(ctx, testPath, result.Base); err != nil || tree != result.Tree {
		return result, errors.New("tests changed the merge result")
	}
	source, err := s.verifyForkWorkspace(ctx, w, in)
	if err != nil {
		return result, fmt.Errorf("verify source after tests: %w", err)
	}
	if source.Head != result.Head {
		return result, errors.New("source changed during tests")
	}
	if p.SyncMode == adaptedBatches {
		if current, _, err := validateForkMapping(ctx, f, &p, workspace); err != nil || current != mappingHash {
			return result, errors.New("upstream mapping changed during tests")
		}
	}
	// Tests may outlive the evidence freshness window or an automatic retry.
	// Queue siblings may receive CI and review feedback while this tree is
	// tested. Source/dependency facts and live authority remain pinned; writes
	// still require the complete, current observation after saving the proof.
	testedSnapshot := forkTestSnapshot(w)
	w, err = s.RefreshWorkContext(ctx, in.EventID)
	if err != nil {
		return result, err
	}
	if err = s.checkWorkAuthority(ctx, &w, in, "test"); err != nil {
		return result, err
	}
	if !forkTestEvidenceMatches(testedSnapshot, forkTestSnapshot(w)) {
		return result, ErrStaleWork
	}
	if err = requireForkWork(w, in); err != nil {
		return result, err
	}
	if w.Fork == nil || w.Fork.Round != p.Round || str(forkFacts(w)["target_sha"]) != result.Base {
		return result, ErrStaleWork
	}
	p = *w.Fork
	if p.Validation == nil || p.Validation.ID != result.Attempt || p.Validation.State != "running" {
		return result, ErrStaleWork
	}
	p.ValidatedHead, p.ValidatedBase, p.ValidatedTree, p.ValidatedGuide, p.TestLog, p.TestLogHash = result.Head, result.Base, result.Tree, in.GuideVersion, result.Log, result.LogHash
	p.ValidatedMapping, p.ValidatedPRSection = mappingHash, prSection
	p.ValidatedResolution = resolution
	if p.SyncMode == adaptedBatches {
		result.PRBodyFile = metadata + ".pr-body.md"
		if err = os.WriteFile(result.PRBodyFile, []byte(prSection+"\n"), 0600); err != nil {
			return result, err
		}
	}
	// Keep the lease alive during remote checks, then stop and join before
	// the terminal write so no heartbeat can race the completed attempt.
	stopHeartbeat()
	if err = s.Store.SaveForkTestProof(ctx, w.Event.Key, p.Revision, p, w.CurrentVersion); err != nil {
		return result, err
	}
	result.Passed = true
	return result, nil
}

// Only the in-memory test comparison uses this projection. Persisted snapshots,
// event versions and write checks retain every PR's complete feedback. A sibling
// report or successful CI completion does not change the tree under test, while
// queueWorkAllowed still rejects failed or closed predecessors after refresh.
func forkTestSnapshot(w WorkContext) []byte {
	if w.Fork == nil || w.Fork.Queue == nil {
		return w.Snapshot
	}
	var snapshot Snapshot
	if json.Unmarshal(w.Snapshot, &snapshot) != nil {
		return w.Snapshot
	}
	for _, batch := range w.Fork.Queue.Batches {
		if batch.Progress.BatchID == w.Fork.BatchID {
			continue
		}
		entry := snapshot.Objects["queue:"+batch.Progress.BatchID]
		if entry == nil {
			continue
		}
		// Snapshot and progress are read separately. Do not project a newer
		// observation over an older snapshot during concurrent collection.
		if entry["evidence"] != hash(canonical(batch.Observation)) {
			return w.Snapshot
		}
		var observation ForkObservation
		if json.Unmarshal(canonical(batch.Observation), &observation) != nil {
			return w.Snapshot
		}
		for key := range observation.Objects {
			kind, _, _ := strings.Cut(key, ":")
			if (kind == "review" || kind == "timeline") && strings.EqualFold(str(observation.Objects[key]["state"]), "CHANGES_REQUESTED") {
				continue
			}
			if slices.Contains([]string{"check", "status", "comment", "review", "review_comment", "timeline"}, kind) {
				delete(observation.Objects, key)
			}
		}
		pull := observation.Objects["pull"]
		if pull["mergeable"] == true && pull["conflict_status"] == "clean" && slices.Contains([]string{"clean", "unstable", "blocked", "has_hooks"}, str(pull["mergeable_state"])) {
			delete(pull, "mergeable_state")
		}
		entry["evidence"] = hash(canonical(observation))
	}
	return canonical(snapshot)
}

func forkTestEvidenceMatches(tested, current []byte) bool {
	var before, after Snapshot
	if json.Unmarshal(tested, &before) != nil || json.Unmarshal(current, &after) != nil {
		return false
	}
	oldWake, oldOK := before.Objects["fork"]["wake"].(float64)
	newWake, newOK := after.Objects["fork"]["wake"].(float64)
	if !oldOK || !newOK || oldWake < 0 || newWake < oldWake {
		return false
	}
	// Validation itself changes the selected batch phase. Remote/dependency
	// evidence remains compared after the test-specific feedback projection.
	if id := str(before.Objects["fork"]["batch_id"]); id != "" {
		for _, snap := range []*Snapshot{&before, &after} {
			delete(snap.Objects["queue:"+id], "phase")
		}
	}
	delete(before.Objects["fork"], "wake")
	delete(after.Objects["fork"], "wake")
	return bytes.Equal(canonical(before), canonical(after))
}
