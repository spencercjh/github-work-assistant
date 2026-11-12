package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitForkMarker(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture did not reach its barrier", marker)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestForkCollectorReleasesAncestryBeforeRemoteReads(t *testing.T) {
	for _, scenario := range []string{"sync ref discovery", "final target read"} {
		t.Run(scenario, func(t *testing.T) {
			g, fixture, f, target, upstream := independentForkFixture(t)
			p := ForkProgress{Upstream: upstream}
			endpoint, occurrence := "repos/example/fixture/git/ref/heads/main", "2"
			if scenario == "sync ref discovery" {
				p.Round, p.Branch = strings.Repeat("a", 64), "sync-fixture"
				endpoint, occurrence = "repos/example/fixture/git/matching-refs/heads/sync-fixture", "1"
				data := readFixture(t, fixture)
				data[endpoint] = []any{}
				data["repos/example/fixture/pulls?base=main&head=example%3Async-fixture&per_page=100&state=all"] = []any{[]any{}}
				writeFixture(t, fixture, data)
			}
			entered, release := filepath.Join(t.TempDir(), "entered"), filepath.Join(t.TempDir(), "release")
			t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
			// Delay only the fake metadata transport, after initial local ancestry.
			gh := filepath.Join(filepath.Dir(fixture), "gh")
			script, err := os.ReadFile(gh)
			if err != nil {
				t.Fatal(err)
			}
			hook := `p=a[5]
if p == os.environ['FORK_BLOCK_ENDPOINT']:
    import pathlib, time
    count=pathlib.Path(os.environ['FORK_BLOCK_COUNT'])
    n=int(count.read_text())+1 if count.exists() else 1
    count.write_text(str(n))
    if n == int(os.environ['FORK_BLOCK_OCCURRENCE']):
        pathlib.Path(os.environ['FORK_BLOCK_ENTERED']).touch()
        until=time.monotonic()+10
        while not pathlib.Path(os.environ['FORK_BLOCK_RELEASE']).exists():
            if time.monotonic()>until: sys.exit('fixture barrier timed out')
            time.sleep(.01)
`
			if err = os.WriteFile(gh, []byte(strings.Replace(string(script), "p=a[5]\n", hook, 1)), 0700); err != nil {
				t.Fatal(err)
			}
			for key, value := range map[string]string{"FORK_BLOCK_ENDPOINT": endpoint, "FORK_BLOCK_COUNT": filepath.Join(t.TempDir(), "count"), "FORK_BLOCK_OCCURRENCE": occurrence, "FORK_BLOCK_ENTERED": entered, "FORK_BLOCK_RELEASE": release} {
				t.Setenv(key, value)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				o, err := g.CollectFork(ctx, f, p)
				if err == nil && (o.Target != target || o.Upstream != upstream || o.ContainsLatest || o.ContainsPinned) {
					err = errors.New("collection changed pinned ancestry facts")
				}
				done <- err
			}()
			waitForkMarker(t, entered)
			checkCtx, stop := context.WithTimeout(t.Context(), 300*time.Millisecond)
			r, lockErr := openForkGitRepository(checkCtx, f)
			stop()
			if lockErr == nil {
				r.close()
			}
			if err = os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			if lockErr != nil {
				t.Fatal("metadata request retained the shared ancestry lock", lockErr)
			}
		})
	}
}

func TestForkAncestryLockWaitsAndHonorsBounds(t *testing.T) {
	for _, scenario := range []string{"released", "cancelled", "caller deadline", "default bound", "open error"} {
		t.Run(scenario, func(t *testing.T) {
			metadata := filepath.Join(t.TempDir(), "ancestry")
			if scenario == "open error" {
				_, err := forkAncestryLock(t.Context(), filepath.Join(metadata, "missing"))
				if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "open fork ancestry lock") {
					t.Fatal("lost filesystem cause", err)
				}
				return
			}
			held, err := workspaceLock(metadata)
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if scenario == "caller deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 100*time.Millisecond)
				defer stop()
			}
			done := make(chan error, 1)
			start := time.Now()
			go func() {
				lock, err := forkAncestryLock(ctx, metadata)
				if lock != nil {
					lock.Close()
				}
				done <- err
			}()
			if scenario == "released" || scenario == "cancelled" {
				select {
				case err := <-done:
					t.Fatal("contention did not wait", err)
				case <-time.After(100 * time.Millisecond):
				}
				if scenario == "released" {
					held.Close()
				} else {
					cancel()
				}
			}
			err = <-done
			want := context.DeadlineExceeded
			if scenario == "released" {
				want = nil
			} else if scenario == "cancelled" {
				want = context.Canceled
			}
			if !errors.Is(err, want) || (err != nil && !strings.Contains(err.Error(), "wait for fork ancestry lock")) {
				t.Fatal("wrong lock outcome", err)
			}
			if scenario == "default bound" && (time.Since(start) < forkAncestryLockTimeout || time.Since(start) > forkAncestryLockTimeout+2*time.Second) {
				t.Fatal("lock exceeded its independent bound", time.Since(start))
			}
			// Neither cancellation nor timeout retains the descriptor's lock.
			held.Close()
			lock, err := workspaceLock(metadata)
			if err != nil {
				t.Fatal("wait leaked an ancestry lock", err)
			}
			lock.Close()
		})
	}
}

func TestForkValidationPostCheckContention(t *testing.T) {
	for _, scenario := range []string{"released", "cancelled", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, _, _, w, _ := beginAdaptedFixture(t)
			ready, release := filepath.Join(t.TempDir(), "ready"), filepath.Join(t.TempDir(), "release")
			code := "import pathlib,time; pathlib.Path(" + string(canonical(ready)) + ").touch()\nwhile not pathlib.Path(" + string(canonical(release)) + ").exists(): time.sleep(.01)"
			s.Config.Repositories[0].TestCommands = [][]string{{"python3", "-c", code}}
			in, _ := currentForkInput(t, s)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type outcome struct {
				proof WorkspaceTest
				err   error
			}
			done := make(chan outcome, 1)
			go func() { proof, err := s.TestWorkspace(ctx, in); done <- outcome{proof, err} }()
			waitForkMarker(t, ready)
			common := fixtureGit(t, g.repository, "", "rev-parse", "--path-format=absolute", "--git-common-dir")
			held, err := workspaceLock(filepath.Join(common, "ghraft-ancestry"))
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			if err = os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				t.Fatal("post-test contention did not wait", got.err)
			case <-time.After(200 * time.Millisecond):
			}
			if scenario == "released" {
				held.Close()
			} else if scenario == "cancelled" {
				cancel()
			}
			got := <-done
			p, err := s.Store.ForkProgress(t.Context(), w.Event.Key)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "released" {
				if got.err != nil || !got.proof.Passed || p.ValidatedHead != got.proof.Head || p.Validation.State != "passed" {
					t.Fatal("transient contention lost the valid proof", got.proof, got.err, p.Validation)
				}
			} else {
				want := context.DeadlineExceeded
				if scenario == "cancelled" {
					want = context.Canceled
				}
				if !errors.Is(got.err, want) || !strings.Contains(got.err.Error(), "verify source after tests: wait for fork ancestry lock") || got.proof.Passed || p.ValidatedHead != "" || p.Validation.State == "passed" {
					t.Fatal("lock failure lost its cause or saved a proof", got.proof, got.err, p.Validation)
				}
			}
		})
	}
}

func TestForkValidationPostCheckStillRejectsChangedSource(t *testing.T) {
	for _, scenario := range []string{"origin", "head"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, _, _, w, ws := beginAdaptedFixture(t)
			want := "verify source after tests: fork local_repository origin does not match the target"
			command := []string{"git", "-C", ws.Path, "remote", "set-url", "origin", "https://github.com/wrong/fixture.git"}
			if scenario == "head" {
				tree := fixtureGit(t, g.repository, "", "rev-parse", ws.Head+"^{tree}")
				head := syntheticCommit(t, g.repository, tree, ws.Head)
				command = []string{"git", "-C", ws.Path, "reset", "--hard", head}
				want = "source changed during tests"
			}
			s.Config.Repositories[0].TestCommands = [][]string{command}
			in, _ := currentForkInput(t, s)
			proof, err := s.TestWorkspace(t.Context(), in)
			p, loadErr := s.Store.ForkProgress(t.Context(), w.Event.Key)
			if err == nil || !strings.Contains(err.Error(), want) || proof.Passed || loadErr != nil || p.ValidatedHead != "" || p.Validation.State != "failed" {
				t.Fatal("changed source accepted or cause lost", proof, err, loadErr, p.Validation)
			}
		})
	}
}

func TestForkAncestryRechecksMetadataAfterWaiting(t *testing.T) {
	for _, scenario := range []string{"origin", "shallow", "graft", "directory replaced"} {
		t.Run(scenario, func(t *testing.T) {
			_, _, f, target, _ := independentForkFixture(t)
			common := fixtureGit(t, f.LocalRepository, "", "rev-parse", "--path-format=absolute", "--git-common-dir")
			held, err := workspaceLock(filepath.Join(common, "ghraft-ancestry"))
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			done := make(chan error, 1)
			go func() {
				r, err := openForkGitRepository(t.Context(), f)
				if r != nil {
					r.close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				t.Fatal("ancestry check did not wait", err)
			case <-time.After(200 * time.Millisecond):
			}
			switch scenario {
			case "origin":
				fixtureGit(t, f.LocalRepository, "", "remote", "set-url", "origin", "https://github.com/wrong/fixture.git")
			case "shallow":
				err = os.WriteFile(filepath.Join(common, "shallow"), []byte(target+"\n"), 0600)
			case "graft":
				err = os.WriteFile(filepath.Join(common, "info", "grafts"), []byte(target+"\n"), 0600)
			case "directory replaced":
				replacement, _, _ := cleanForkHistory(t)
				err = os.Rename(f.LocalRepository, f.LocalRepository+"-old")
				if err == nil {
					err = os.Rename(replacement, f.LocalRepository)
					common = strings.Replace(common, f.LocalRepository, f.LocalRepository+"-old", 1)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			held.Close()
			if err = <-done; err == nil {
				t.Fatal("accepted repository metadata changed during the wait")
			}
			// Rejection must release the originally acquired lock as well.
			lock, err := workspaceLock(filepath.Join(common, "ghraft-ancestry"))
			if err != nil {
				t.Fatal("metadata rejection retained the ancestry lock", err)
			}
			lock.Close()
		})
	}
}

func TestForkValidationRejectsSourceChangesDuringAncestryWait(t *testing.T) {
	for _, scenario := range []string{"head", "branch", "dirty"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, _, _, w, ws := beginAdaptedFixture(t)
			tree := fixtureGit(t, g.repository, "", "rev-parse", ws.Head+"^{tree}")
			next := syntheticCommit(t, g.repository, tree, ws.Head)
			common := fixtureGit(t, g.repository, "", "rev-parse", "--path-format=absolute", "--git-common-dir")
			dir := t.TempDir()
			ready, release, waiting := filepath.Join(dir, "ready"), filepath.Join(dir, "release"), filepath.Join(dir, "waiting")
			code := "import pathlib,time; pathlib.Path(" + string(canonical(ready)) + ").touch()\nwhile not pathlib.Path(" + string(canonical(release)) + ").exists(): time.sleep(.01)"
			s.Config.Repositories[0].TestCommands = [][]string{{"python3", "-c", code}}
			in, _ := currentForkInput(t, s)
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			// Signal entry to the post-test lock acquisition, so the source
			// mutation happens in the wait rather than racing a guessed delay.
			shim := "#!/usr/bin/env python3\nimport pathlib,subprocess,sys\na=sys.argv[1:]\nr=subprocess.run([" + string(canonical(realGit)) + "]+a,capture_output=True)\nif '-C' in a and a[a.index('-C')+1]==" + string(canonical(g.repository)) + " and a[-3:]==['rev-parse','--path-format=absolute','--git-common-dir'] and pathlib.Path(" + string(canonical(release)) + ").exists(): pathlib.Path(" + string(canonical(waiting)) + ").touch()\nsys.stdout.buffer.write(r.stdout)\nsys.stderr.buffer.write(r.stderr)\nsys.exit(r.returncode)\n"
			if err = os.WriteFile(filepath.Join(dir, "git"), []byte(shim), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			type outcome struct {
				proof WorkspaceTest
				err   error
			}
			done := make(chan outcome, 1)
			go func() { proof, err := s.TestWorkspace(ctx, in); done <- outcome{proof, err} }()
			waitForkMarker(t, ready)
			held, err := workspaceLock(filepath.Join(common, "ghraft-ancestry"))
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			if err = os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			waitForkMarker(t, waiting)
			switch scenario {
			case "head":
				fixtureGit(t, ws.Path, "", "reset", "--hard", next)
			case "branch":
				fixtureGit(t, ws.Path, "", "checkout", "-b", "unexpected")
			case "dirty":
				err = os.WriteFile(filepath.Join(ws.Path, "untracked-during-wait"), []byte("changed"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			held.Close()
			got := <-done
			p, loadErr := s.Store.ForkProgress(t.Context(), w.Event.Key)
			if got.err == nil || got.proof.Passed || loadErr != nil || p.ValidatedHead != "" || p.Validation.State == "passed" {
				t.Fatal("saved proof after source changed during lock wait", scenario, got.proof, got.err, p.Validation, loadErr)
			}
		})
	}
}
