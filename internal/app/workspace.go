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
)

type Workspace struct {
	WorkInput
	Resource     string     `json:"resource_key"`
	Repository   string     `json:"repository"`
	Path         string     `json:"path"`
	Branch       string     `json:"branch"`
	Head         string     `json:"head"`
	Base         string     `json:"base"`
	Conflicts    []string   `json:"conflicts"`
	TestCommands [][]string `json:"test_commands"`
	Verification string     `json:"verification"`
}

func gitLocal(ctx context.Context, directory string, args ...string) (string, error) {
	// These operations are local and never create commits or push. Disable hooks for merge/worktree setup.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-C", directory}, args...)...)
	var stdout, stderr limitedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("local git %s failed", args[0])
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (s *Service) workspaceInput(ctx context.Context, in WorkInput) (WorkContext, string, string, error) {
	w, err := s.CheckWork(ctx, in)
	if err != nil {
		return w, "", "", err
	}
	if w.Guide.Mode != "local-edit" || w.Task == nil || w.Task.TaskID != in.TaskID || w.Task.GuideVersion != in.GuideVersion || w.Task.EvidenceVersion != in.EvidenceVersion {
		return w, "", "", ErrWorkDenied
	}
	var snapshot Snapshot
	if json.Unmarshal(w.Snapshot, &snapshot) != nil {
		return w, "", "", errors.New("invalid snapshot")
	}
	pull := snapshot.Objects["pull"]
	head, base := str(pull["head"]), str(pull["base_sha"])
	if !shaRE.MatchString(head) || !shaRE.MatchString(base) || pull["state"] != "open" || pull["merged"] == true {
		return w, "", "", errors.New("workspace requires an open PR with captured head/base commits")
	}
	return w, head, base, nil
}

func (s *Service) workspacePaths(id string) (string, string, error) {
	if !idRE.MatchString(id) || !filepath.IsAbs(s.Config.WorkspaceRoot) {
		return "", "", errors.New("absolute workspace_root and event ID required")
	}
	if err := os.MkdirAll(s.Config.WorkspaceRoot, 0700); err != nil {
		return "", "", err
	}
	root, err := filepath.EvalSymlinks(s.Config.WorkspaceRoot)
	if err != nil {
		return "", "", err
	}
	return filepath.Join(root, id), filepath.Join(root, id+".json"), nil
}

// PrepareWorkspace operates on an existing local clone. The Raft agent fetches through gh first.
// It leaves a merge uncommitted for the agent to resolve and test, preserving both original revisions.
func (s *Service) PrepareWorkspace(ctx context.Context, in WorkInput, repository string) (Workspace, error) {
	w, head, base, err := s.workspaceInput(ctx, in)
	if err != nil {
		return Workspace{}, err
	}
	path, metadata, err := s.workspacePaths(in.EventID)
	if err != nil {
		return Workspace{}, err
	}
	if _, err = os.Lstat(metadata); err == nil {
		return s.VerifyWorkspace(ctx, in)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Workspace{}, err
	}
	repository, err = filepath.Abs(repository)
	if err != nil {
		return Workspace{}, err
	}
	if _, err = gitLocal(ctx, repository, "cat-file", "-e", head+"^{commit}"); err != nil {
		return Workspace{}, err
	}
	if _, err = gitLocal(ctx, repository, "cat-file", "-e", base+"^{commit}"); err != nil {
		return Workspace{}, err
	}
	// Bind the local checkout to the configured repository; a folder name is not proof of identity.
	remote, err := gitLocal(ctx, repository, "remote", "get-url", "origin")
	watch, _ := resourceWatch(w.Event.Key)
	if err != nil || !githubOriginMatches(remote, watch.Repo) {
		return Workspace{}, errors.New("local origin does not match authorized GitHub repository")
	}
	branch := "ghraft/" + in.EventID
	if _, err = gitLocal(ctx, repository, "worktree", "add", "-b", branch, path, head); err != nil {
		return Workspace{}, err
	}
	workspace := Workspace{WorkInput: in, Resource: w.Event.Key, Repository: repository, Path: path, Branch: branch, Head: head, Base: base, TestCommands: w.Guide.TestCommands}
	// Persist the recovery record before merge: process interruption must not cause a second worktree.
	data, _ := json.MarshalIndent(workspace, "", "  ")
	f, err := os.OpenFile(metadata, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return workspace, err
	}
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return workspace, err
	}
	if closeErr != nil {
		return workspace, closeErr
	}
	_, mergeErr := gitLocal(ctx, path, "merge", "--no-commit", "--no-ff", base)
	workspace, err = s.VerifyWorkspace(ctx, in)
	if err != nil {
		return workspace, err
	}
	if mergeErr != nil && len(workspace.Conflicts) == 0 {
		return workspace, errors.New("merge failed without resolvable conflict; inspect isolated worktree")
	}
	return workspace, nil
}

func githubOriginMatches(remote, repo string) bool {
	remote = strings.TrimSuffix(strings.TrimSpace(remote), ".git")
	return strings.EqualFold(remote, "https://github.com/"+repo) || strings.EqualFold(remote, "git@github.com:"+repo) || strings.EqualFold(remote, "ssh://git@github.com/"+repo)
}

// VerifyWorkspace checks the local evidence; it deliberately does not claim that project tests ran.
// Run refresh-work-context first to check remote revisions, then execute the configured tests in Raft.
func (s *Service) VerifyWorkspace(ctx context.Context, in WorkInput) (Workspace, error) {
	w, head, base, err := s.workspaceInput(ctx, in)
	if err != nil {
		return Workspace{}, err
	}
	path, metadata, err := s.workspacePaths(in.EventID)
	if err != nil {
		return Workspace{}, err
	}
	b, err := os.ReadFile(metadata)
	if err != nil {
		return Workspace{}, err
	}
	var workspace Workspace
	if json.Unmarshal(b, &workspace) != nil || workspace.WorkInput != in || workspace.Path != path || workspace.Head != head || workspace.Base != base || workspace.Resource != w.Event.Key || workspace.Branch != "ghraft/"+in.EventID {
		return workspace, errors.New("workspace record does not match current work")
	}
	current, err := gitLocal(ctx, path, "rev-parse", "HEAD")
	if err != nil || current != head {
		return workspace, errors.New("workspace HEAD changed; inspect and revalidate before recording a result")
	}
	branch, err := gitLocal(ctx, path, "branch", "--show-current")
	if err != nil || branch != workspace.Branch {
		return workspace, errors.New("workspace branch changed")
	}
	conflicts, err := gitLocal(ctx, path, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return workspace, err
	}
	workspace.Conflicts = []string{}
	if conflicts != "" {
		workspace.Conflicts = strings.Split(conflicts, "\n")
	}
	if len(workspace.Conflicts) > 0 {
		workspace.Verification = "merge conflicts remain; the Raft agent must resolve semantic intent and run project tests"
		return workspace, nil
	}
	mergeHead, _ := gitLocal(ctx, path, "rev-parse", "--verify", "MERGE_HEAD")
	if mergeHead != base {
		if _, err = gitLocal(ctx, path, "merge-base", "--is-ancestor", base, head); err != nil {
			return workspace, errors.New("base merge is missing or was aborted; do not report this work as complete")
		}
	}
	if _, err = gitLocal(ctx, path, "diff", "--check"); err != nil {
		return workspace, errors.New("unstaged diff has whitespace errors or conflict markers")
	}
	if _, err = gitLocal(ctx, path, "diff", "--cached", "--check"); err != nil {
		return workspace, errors.New("staged diff has whitespace errors or conflict markers")
	}
	workspace.Verification = "local merge is prepared and has no unresolved index entries; project tests and fresh GitHub verification are still required; no commit or push performed"
	return workspace, nil
}
