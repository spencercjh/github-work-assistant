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
	workspace := Workspace{WorkInput: in, Resource: w.Event.Key, Repository: repository, Path: path, Branch: p.Branch, Head: forkSourceBase(p), Base: p.InitialTarget, TestCommands: w.Guide.TestCommands}
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
		args := []string{"worktree", "add", "--detach", path, p.Upstream}
		if p.SyncMode == adaptedBatches {
			args = []string{"worktree", "add", "-b", p.Branch, path, p.InitialTarget}
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
	actual, err := filepath.EvalSymlinks(path)
	if err != nil || actual != path {
		return workspace, errors.New("workspace path changed")
	}
	f, _ := s.Config.Fork(w.Event.Key)
	remote, err := gitLocal(ctx, path, "remote", "get-url", "origin")
	if err != nil || !githubOriginMatches(remote, f.Target.Repo) {
		return workspace, errors.New("workspace origin changed")
	}
	branch, err := gitLocal(ctx, path, "branch", "--show-current")
	wantBranch := ""
	if p.SyncMode == adaptedBatches {
		wantBranch = p.Branch
	}
	if err != nil || branch != wantBranch {
		return workspace, errors.New("source worktree branch differs from the sync round")
	}
	head, err := gitLocal(ctx, path, "rev-parse", "HEAD")
	if err != nil || !shaRE.MatchString(head) {
		return workspace, errors.New("source worktree has no valid commit")
	}
	if p.SyncMode == adaptedBatches {
		localSpec := f
		localSpec.LocalRepository = workspace.Repository
		repo, err := openForkGitRepository(ctx, localSpec)
		if err != nil {
			return workspace, err
		}
		contains, err := repo.contains(ctx, p.InitialTarget, head)
		repo.close()
		if err != nil || !contains {
			return workspace, errors.New("adapted source must retain its initial target history")
		}
	} else if head != p.Upstream {
		return workspace, errors.New("source worktree must contain the exact pinned upstream commit without integration commits")
	}
	workspace.Head = head
	if err = cleanCommitted(ctx, path); err != nil {
		return workspace, err
	}
	workspace.WorkInput, workspace.Base, workspace.TestCommands = in, str(forkFacts(w)["target_sha"]), w.Guide.TestCommands
	workspace.Conflicts = []string{}
	workspace.Verification = "Source is the exact pinned upstream commit. test-worktree validates a separate temporary merge with the current target; push only this source HEAD to the returned branch."
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
func (s *Service) TestWorkspace(ctx context.Context, in WorkInput) (WorkspaceTest, error) {
	var result WorkspaceTest
	w, err := s.CheckWork(ctx, in)
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
	p := *w.Fork
	p.ValidatedHead, p.ValidatedBase, p.ValidatedTree, p.ValidatedGuide, p.TestLog, p.TestLogHash = "", "", "", "", "", ""
	p.ValidatedMapping, p.ValidatedPRSection = "", ""
	if err = s.Store.SaveForkProgress(ctx, w.Event.Key, p.Revision, p, nil); err != nil {
		return result, err
	}
	var mappingHash, prSection string
	f, _ := s.Config.Fork(w.Event.Key)
	if p.SyncMode == adaptedBatches {
		mappingHash, prSection, err = validateForkMapping(ctx, f, &p, workspace)
		if err != nil {
			return result, err
		}
	}
	result.Head, result.Base = workspace.Head, workspace.Base
	testPath, cleanup, err := s.forkTestWorktree(ctx, workspace)
	if err != nil {
		return result, err
	}
	defer cleanup()
	result.Tree, err = forkTestTree(ctx, testPath, result.Base)
	if err != nil {
		return result, err
	}
	result.Log = metadata + fmt.Sprintf(".test-%d.log", time.Now().UnixNano())
	log, err := os.OpenFile(result.Log, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	defer log.Close()
	home, err := os.MkdirTemp(s.Config.WorkspaceRoot, "test-home-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(home)
	testCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	var output limitedBuffer
	fmt.Fprintf(&output, "source: %s\ntarget: %s\nmerge tree: %s\n", result.Head, result.Base, result.Tree)
	for _, argv := range w.Guide.TestCommands {
		fmt.Fprintf(&output, "command: %s\n", string(canonical(argv)))
		cmd := exec.CommandContext(testCtx, argv[0], argv[1:]...)
		cmd.Dir = testPath
		cmd.WaitDelay = 5 * time.Second
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "LANG=C.UTF-8", "CI=true", "PYTHONDONTWRITEBYTECODE=1"}
		cmd.Stdout, cmd.Stderr = &output, &output
		if err = cmd.Run(); err != nil {
			break
		}
	}
	_, writeErr := log.Write(output.Bytes())
	if writeErr != nil {
		return result, writeErr
	}
	if syncErr := log.Sync(); syncErr != nil {
		return result, syncErr
	}
	result.LogHash = hash(output.Bytes())
	if err != nil {
		return result, fmt.Errorf("regression command failed; inspect %s", result.Log)
	}
	if tree, err := forkTestTree(ctx, testPath, result.Base); err != nil || tree != result.Tree {
		return result, errors.New("tests changed the merge result")
	}
	if source, err := s.verifyForkWorkspace(ctx, w, in); err != nil || source.Head != result.Head {
		return result, errors.New("source changed during tests")
	}
	if p.SyncMode == adaptedBatches {
		if current, _, err := validateForkMapping(ctx, f, &p, workspace); err != nil || current != mappingHash {
			return result, errors.New("upstream mapping changed during tests")
		}
	}
	// Tests may outlive the evidence freshness window or an automatic retry.
	// Only a retry wake may differ; all source facts and authority stay bound
	// to the inputs accepted before running the regression commands.
	testedSnapshot := w.Snapshot
	w, err = s.RefreshWorkContext(ctx, in.EventID)
	if err != nil {
		return result, err
	}
	if err = s.checkWorkAuthority(ctx, w, in); err != nil {
		return result, err
	}
	if !forkTestEvidenceMatches(testedSnapshot, w.Snapshot) {
		return result, ErrStaleWork
	}
	if err = requireForkWork(w, in); err != nil {
		return result, err
	}
	if w.Fork == nil || w.Fork.Round != p.Round || str(forkFacts(w)["target_sha"]) != result.Base {
		return result, ErrStaleWork
	}
	p = *w.Fork
	p.ValidatedHead, p.ValidatedBase, p.ValidatedTree, p.ValidatedGuide, p.TestLog, p.TestLogHash = result.Head, result.Base, result.Tree, in.GuideVersion, result.Log, result.LogHash
	p.ValidatedMapping, p.ValidatedPRSection = mappingHash, prSection
	if p.SyncMode == adaptedBatches {
		result.PRBodyFile = metadata + ".pr-body.md"
		if err = os.WriteFile(result.PRBodyFile, []byte(prSection+"\n"), 0600); err != nil {
			return result, err
		}
	}
	if err = s.Store.SaveForkTestProof(ctx, w.Event.Key, p.Revision, p, w.CurrentVersion); err != nil {
		return result, err
	}
	result.Passed = true
	return result, nil
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
	delete(before.Objects["fork"], "wake")
	delete(after.Objects["fork"], "wake")
	return bytes.Equal(canonical(before), canonical(after))
}
