package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

func (s *Service) prepareForkWorkspace(ctx context.Context, w WorkContext, in WorkInput, repository string) (Workspace, error) {
	if err := requireForkWork(w, in); err != nil {
		return Workspace{}, err
	}
	p := w.Fork
	f, _ := s.Config.Fork(w.Event.Key)
	path, metadata, err := s.workspacePaths(p.Round)
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
	for _, sha := range []string{p.InitialTarget, p.Upstream, str(forkFacts(w)["target_sha"])} {
		if !shaRE.MatchString(sha) {
			return Workspace{}, errors.New("missing pinned fork revision")
		}
		if _, err = gitLocal(ctx, repository, "cat-file", "-e", sha+"^{commit}"); err != nil {
			return Workspace{}, errors.New("fetch the pinned upstream and target commits into the local clone first")
		}
	}
	workspace := Workspace{WorkInput: in, Resource: w.Event.Key, Repository: repository, Path: path, Branch: p.Branch, Head: p.InitialTarget, Base: p.Upstream, TestCommands: w.Guide.TestCommands}
	if b, err := os.ReadFile(metadata); err == nil {
		var saved Workspace
		if json.Unmarshal(b, &saved) != nil || saved.Repository != repository || saved.Resource != w.Event.Key || saved.Path != path || saved.Branch != p.Branch || saved.Head != p.InitialTarget || saved.Base != p.Upstream {
			return Workspace{}, errors.New("workspace metadata differs from this sync round")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		// The durable intent precedes worktree creation, allowing recovery if the
		// process exits between git worktree add and the first merge.
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
		branchHead, branchErr := gitLocal(ctx, repository, "rev-parse", "--verify", "refs/heads/"+p.Branch)
		if branchErr == nil {
			if branchHead != p.InitialTarget {
				return Workspace{}, errors.New("existing sync branch differs from recovery intent")
			}
			_, err = gitLocal(ctx, repository, "worktree", "add", path, p.Branch)
		} else {
			_, err = gitLocal(ctx, repository, "worktree", "add", "-b", p.Branch, path, p.InitialTarget)
		}
		if err != nil {
			return Workspace{}, err
		}
	} else if err != nil {
		return Workspace{}, err
	}
	branch, err := gitLocal(ctx, path, "branch", "--show-current")
	if err != nil || branch != p.Branch {
		return Workspace{}, errors.New("worktree is on another branch")
	}
	mergeHead, _ := gitLocal(ctx, path, "rev-parse", "--verify", "MERGE_HEAD")
	if mergeHead == "" {
		if err := cleanCommitted(ctx, path); err != nil {
			return Workspace{}, err
		}
		for _, sha := range []string{p.Upstream, str(forkFacts(w)["target_sha"])} {
			if _, err := gitLocal(ctx, path, "merge-base", "--is-ancestor", sha, "HEAD"); err == nil {
				continue
			}
			_, mergeErr := gitLocal(ctx, path, "merge", "--no-commit", "--no-ff", sha)
			mergeHead, _ = gitLocal(ctx, path, "rev-parse", "--verify", "MERGE_HEAD")
			if mergeHead != "" {
				break
			} // The executor resolves and commits before another merge.
			if mergeErr != nil {
				return Workspace{}, errors.New("fork merge failed; inspect the isolated worktree")
			}
		}
	}
	return s.verifyForkWorkspace(ctx, w, in)
}

func (s *Service) verifyForkWorkspace(ctx context.Context, w WorkContext, in WorkInput) (Workspace, error) {
	if err := requireForkWork(w, in); err != nil {
		return Workspace{}, err
	}
	p := w.Fork
	path, metadata, err := s.workspacePaths(p.Round)
	if err != nil {
		return Workspace{}, err
	}
	data, err := os.ReadFile(metadata)
	if err != nil {
		return Workspace{}, err
	}
	var workspace Workspace
	if json.Unmarshal(data, &workspace) != nil || workspace.Path != path || workspace.Resource != w.Event.Key || workspace.Branch != p.Branch || workspace.Head != p.InitialTarget || workspace.Base != p.Upstream {
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
	if err != nil || branch != p.Branch {
		return workspace, errors.New("workspace branch changed")
	}
	head, err := gitLocal(ctx, path, "rev-parse", "HEAD")
	if err != nil || !shaRE.MatchString(head) {
		return workspace, errors.New("invalid workspace HEAD")
	}
	if _, err := gitLocal(ctx, path, "merge-base", "--is-ancestor", p.InitialTarget, head); err != nil {
		return workspace, errors.New("workspace discarded original fork history")
	}
	conflicts, err := gitLocal(ctx, path, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return workspace, err
	}
	workspace.WorkInput, workspace.Head, workspace.TestCommands = in, head, w.Guide.TestCommands
	workspace.Conflicts = []string{}
	if conflicts != "" {
		workspace.Conflicts = strings.Split(conflicts, "\n")
	}
	mergeHead, _ := gitLocal(ctx, path, "rev-parse", "--verify", "MERGE_HEAD")
	if mergeHead != "" || len(workspace.Conflicts) > 0 {
		workspace.Verification = "Merge remains uncommitted; resolve semantic intent and commit, then prepare again to include any moved target. No tests have been verified."
		return workspace, nil
	}
	for _, sha := range []string{p.Upstream, str(forkFacts(w)["target_sha"])} {
		if _, err := gitLocal(ctx, path, "merge-base", "--is-ancestor", sha, head); err != nil {
			return workspace, errors.New("pinned upstream or current target is missing; prepare-worktree again")
		}
	}
	workspace.Verification = "Committed history includes pinned upstream and current fork target; test-worktree must validate this exact commit before writing."
	return workspace, nil
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
	Head    string `json:"head"`
	Base    string `json:"base"`
	Passed  bool   `json:"passed"`
	Log     string `json:"log"`
	LogHash string `json:"log_hash"`
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
	_, metadata, err := s.workspacePaths(w.Fork.Round)
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
	if len(w.Guide.TestCommands) == 0 {
		return result, errors.New("no regression commands configured")
	}
	p := *w.Fork
	p.ValidatedHead, p.ValidatedBase, p.ValidatedGuide, p.TestLog, p.TestLogHash = "", "", "", "", ""
	if err = s.Store.SaveForkProgress(ctx, w.Event.Key, p.Revision, p, nil); err != nil {
		return result, err
	}
	result.Head, result.Base = workspace.Head, str(forkFacts(w)["target_sha"])
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
	for _, argv := range w.Guide.TestCommands {
		fmt.Fprintf(&output, "command: %s\n", string(canonical(argv)))
		cmd := exec.CommandContext(testCtx, argv[0], argv[1:]...)
		cmd.Dir = workspace.Path
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
	if err = cleanCommitted(ctx, workspace.Path); err != nil {
		return result, err
	}
	if after, err := gitLocal(ctx, workspace.Path, "rev-parse", "HEAD"); err != nil || after != result.Head {
		return result, errors.New("tests changed the commit")
	}
	// Tests may outlive the evidence freshness window. Collect again, then
	// reject changed inputs or ownership before saving the tested revisions.
	if _, err = s.RefreshWorkContext(ctx, in.EventID); err != nil {
		return result, err
	}
	w, err = s.CheckWork(ctx, in)
	if err != nil {
		return result, err
	}
	if w.Fork == nil || w.Fork.Round != p.Round || str(forkFacts(w)["target_sha"]) != result.Base {
		return result, ErrStaleWork
	}
	p = *w.Fork
	p.ValidatedHead, p.ValidatedBase, p.ValidatedGuide, p.TestLog, p.TestLogHash = result.Head, result.Base, in.GuideVersion, result.Log, result.LogHash
	if err = s.Store.SaveForkProgress(ctx, w.Event.Key, p.Revision, p, nil); err != nil {
		return result, err
	}
	result.Passed = true
	return result, nil
}
