package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Independent repositories cannot use GitHub's cross-fork comparison API.
// A trusted, full local clone provides ancestry for API-pinned commit objects;
// repository access and both branch heads are still checked on GitHub first.
type forkGitRepository struct {
	directory string
	lock      *os.File
}

func (r *forkGitRepository) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-replace-objects", "-c", "core.hooksPath=/dev/null", "-C", r.directory}, args...)...)
	var stdout, stderr limitedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return strings.TrimSpace(stdout.String()), err
}

func openForkGitRepository(ctx context.Context, f ForkSpec) (*forkGitRepository, error) {
	r := &forkGitRepository{directory: f.LocalRepository}
	if !filepath.IsAbs(r.directory) {
		return nil, errors.New("fork local_repository must be absolute")
	}
	remote, err := r.run(ctx, "remote", "get-url", "origin")
	if err != nil || !githubOriginMatches(remote, f.Target.Repo) {
		return nil, errors.New("fork local_repository origin does not match the target")
	}
	shallow, err := r.run(ctx, "rev-parse", "--is-shallow-repository")
	if err != nil || shallow != "false" {
		return nil, errors.New("fork ancestry requires a complete, non-shallow clone")
	}
	common, err := r.run(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || !filepath.IsAbs(common) {
		return nil, errors.New("cannot resolve fork local_repository metadata")
	}
	if _, err = os.Stat(filepath.Join(common, "info", "grafts")); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("fork ancestry cannot use local grafts")
	}
	r.lock, err = workspaceLock(filepath.Join(common, "ghraft-ancestry"))
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (r *forkGitRepository) close() { r.lock.Close() }

func (r *forkGitRepository) ensureCommit(ctx context.Context, repo, sha string) error {
	if sha == "" {
		return nil
	}
	if !shaRE.MatchString(sha) || !repoRE.MatchString(repo) || strings.Contains(repo, "..") {
		return errors.New("invalid fork revision source")
	}
	if _, err := r.run(ctx, "cat-file", "-e", sha+"^{commit}"); err == nil {
		return nil
	}
	// Fetch only the observed object from its configured GitHub repository.
	// No branch, worktree, tag or FETCH_HEAD is updated. Credentials stay in gh.
	fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	_, err := r.run(fetchCtx, "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential",
		"-c", "protocol.file.allow=never", "fetch", "--no-tags", "--no-write-fetch-head", "--no-auto-maintenance",
		"https://github.com/"+repo+".git", sha)
	if err != nil {
		return errors.New("fetching a pinned fork revision failed; check GitHub access and local clone health")
	}
	if _, err = r.run(ctx, "cat-file", "-e", sha+"^{commit}"); err != nil {
		return errors.New("pinned fork revision is unavailable after fetch")
	}
	return nil
}

func (r *forkGitRepository) contains(ctx context.Context, ancestor, head string) (bool, error) {
	if ancestor == "" {
		return false, nil
	}
	if !shaRE.MatchString(ancestor) || !shaRE.MatchString(head) {
		return false, errors.New("invalid local ancestry revisions")
	}
	_, err := r.run(ctx, "merge-base", "--is-ancestor", ancestor, head)
	if err == nil {
		return true, nil
	}
	var status *exec.ExitError
	if errors.As(err, &status) && status.ExitCode() == 1 && ctx.Err() == nil {
		return false, nil
	}
	return false, errors.New("local fork ancestry could not be verified")
}
