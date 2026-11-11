package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
	common, err := r.run(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || !filepath.IsAbs(common) {
		return nil, errors.New("cannot resolve fork local_repository metadata")
	}
	r.lock, err = forkAncestryLock(ctx, filepath.Join(common, "ghraft-ancestry"))
	if err != nil {
		return nil, err
	}
	valid := false
	defer func() {
		if !valid {
			r.close()
		}
	}()
	// Metadata can change while waiting. Validate under the acquired lock,
	// including that the configured path still resolves to the locked clone.
	remote, err := r.run(ctx, "remote", "get-url", "origin")
	if err != nil || !githubOriginMatches(remote, f.Target.Repo) {
		return nil, errors.New("fork local_repository origin does not match the target")
	}
	shallow, err := r.run(ctx, "rev-parse", "--is-shallow-repository")
	if err != nil || shallow != "false" {
		return nil, errors.New("fork ancestry requires a complete, non-shallow clone")
	}
	current, err := r.run(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || current != common {
		return nil, errors.New("fork local_repository metadata changed while waiting for its lock")
	}
	locked, lockErr := r.lock.Stat()
	currentLock, currentErr := os.Stat(filepath.Join(current, "ghraft-ancestry.lock"))
	if lockErr != nil || currentErr != nil || !os.SameFile(locked, currentLock) {
		return nil, errors.New("fork local_repository lock changed while waiting")
	}
	if _, err = os.Stat(filepath.Join(common, "info", "grafts")); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("fork ancestry cannot use local grafts")
	}
	valid = true
	return r, nil
}

// Ancestry checks share a repository across workspaces and the collector. Wait
// briefly for local Git work, without changing the fail-fast workspace lock.
const forkAncestryLockTimeout = 5 * time.Second

func forkAncestryLock(ctx context.Context, metadata string) (*os.File, error) {
	ctx, cancel := context.WithTimeout(ctx, forkAncestryLockTimeout)
	defer cancel()
	f, err := os.OpenFile(metadata+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open fork ancestry lock: %w", err)
	}
	for {
		if err = ctx.Err(); err != nil {
			f.Close()
			return nil, fmt.Errorf("wait for fork ancestry lock: %w", err)
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, fmt.Errorf("acquire fork ancestry lock: %w", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func withForkGitRepository(ctx context.Context, f ForkSpec, check func(*forkGitRepository) error) error {
	repo, err := openForkGitRepository(ctx, f)
	if err != nil {
		return err
	}
	defer repo.close()
	return check(repo)
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
