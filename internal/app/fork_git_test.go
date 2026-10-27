package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func independentForkFixture(t *testing.T) (GH, string, ForkSpec, string, string) {
	t.Helper()
	repo, target, upstream := cleanForkHistory(t)
	g, fixture := fakeGH(t, upstream, target)
	g.PrivateRepositories = []string{"example/fixture"}
	f := ForkSpec{ID: "independent", Upstream: BranchRef{"source/fixture", "main"}, Target: BranchRef{"example/fixture", "main"}, LocalRepository: repo}
	data := readFixture(t, fixture)
	data["repos/source/fixture"] = map[string]any{"full_name": "source/fixture", "private": false}
	data["repos/example/fixture"] = map[string]any{"full_name": "example/fixture", "private": true, "fork": false}
	for repo, sha := range map[string]string{f.Upstream.Repo: upstream, f.Target.Repo: target} {
		data["repos/"+repo+"/git/ref/heads/main"] = map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": sha, "type": "commit"}}
	}
	// No cross-repository compare endpoint exists, matching independent GitHub repos.
	writeFixture(t, fixture, data)
	return g, fixture, f, target, upstream
}

func TestIndependentForkCollectsAncestryAndRespectsPrivateAccess(t *testing.T) {
	g, fixture, f, target, upstream := independentForkFixture(t)
	o, err := g.CollectFork(t.Context(), f, ForkProgress{Upstream: upstream})
	if err != nil || o.ContainsLatest || o.ContainsPinned || o.Classification != "private" || len(o.PrivateRepos) != 1 {
		t.Fatal("independent fork collection failed", o, err)
	}
	tree := fixtureGit(t, f.LocalRepository, "", "rev-parse", target+"^{tree}")
	merged := syntheticCommit(t, f.LocalRepository, tree, target, upstream)
	data := readFixture(t, fixture)
	obj(obj(data["repos/example/fixture/git/ref/heads/main"])["object"])["sha"] = merged
	writeFixture(t, fixture, data)
	o, err = g.CollectFork(t.Context(), f, ForkProgress{Upstream: upstream})
	if err != nil || !o.ContainsLatest || !o.ContainsPinned {
		t.Fatal("merged upstream was not recognized", o, err)
	}
	// Access is checked before touching the cache; cached private objects grant nothing.
	g.PrivateRepositories = nil
	f.LocalRepository = filepath.Join(t.TempDir(), "missing")
	if _, err = g.CollectFork(t.Context(), f, ForkProgress{}); !errors.Is(err, ErrRepositoryNotAllowed) {
		t.Fatal("private access was not enforced before local reads", err)
	}
}

func TestIndependentForkFetchesPinnedObjectsWithoutUpdatingRefs(t *testing.T) {
	g, _, f, target, upstream := independentForkFixture(t)
	source := f.LocalRepository
	fixtureGit(t, source, "", "update-ref", "refs/heads/upstream", upstream)
	fixtureGit(t, source, "", "update-ref", "refs/heads/target", target)
	cache := filepath.Join(t.TempDir(), "cache.git")
	fixtureGit(t, t.TempDir(), "", "init", "--bare", cache)
	fixtureGit(t, cache, "", "remote", "add", "origin", "https://github.com/example/fixture.git")
	f.LocalRepository = cache
	actualGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	// Exercise real git fetch, replacing only the configured HTTPS transport with
	// a local fixture. Unexpected URLs fail rather than contacting the network.
	script := `#!/usr/bin/env python3
import os, sys
a=sys.argv[1:]
if 'fetch' in a:
    expected=['https://github.com/source/fixture.git','https://github.com/example/fixture.git']
    if a[-2] not in expected: sys.exit('unexpected remote')
    if any(x not in a for x in ['--no-tags','--no-write-fetch-head','--no-auto-maintenance','credential.helper=!gh auth git-credential']): sys.exit('missing fetch constraint')
    with open(os.environ['FORK_FETCH_LOG'],'a') as f: f.write(a[-2]+' '+a[-1]+'\n')
    a[-2]='file://'+os.environ['FORK_FETCH_SOURCE']
    a=[x.replace('protocol.file.allow=never','protocol.file.allow=always') for x in a]
os.execv(os.environ['FORK_REAL_GIT'],[os.environ['FORK_REAL_GIT']]+a)
`
	if err = os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(bin, "fetches")
	t.Setenv("FORK_REAL_GIT", actualGit)
	t.Setenv("FORK_FETCH_SOURCE", source)
	t.Setenv("FORK_FETCH_LOG", log)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for range 2 {
		if _, err = g.CollectFork(t.Context(), f, ForkProgress{Upstream: upstream}); err != nil {
			t.Fatal(err)
		}
	}
	fetches, err := os.ReadFile(log)
	if err != nil || strings.Count(string(fetches), "\n") != 2 {
		t.Fatal("pinned objects were not fetched exactly once", string(fetches), err)
	}
	if refs := fixtureGit(t, cache, "", "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatal("collection updated branch refs", refs)
	}
	if _, err = os.Stat(filepath.Join(cache, "FETCH_HEAD")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("collection wrote FETCH_HEAD", err)
	}
}

func TestIndependentForkRejectsIncompleteOrModifiedHistory(t *testing.T) {
	for _, scenario := range []string{"wrong-origin", "shallow", "graft", "missing-commit", "replacement", "locked"} {
		t.Run(scenario, func(t *testing.T) {
			_, _, f, target, upstream := independentForkFixture(t)
			common := fixtureGit(t, f.LocalRepository, "", "rev-parse", "--path-format=absolute", "--git-common-dir")
			switch scenario {
			case "wrong-origin":
				fixtureGit(t, f.LocalRepository, "", "remote", "set-url", "origin", "https://github.com/wrong/project")
			case "shallow":
				if err := os.WriteFile(filepath.Join(common, "shallow"), []byte(target+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "graft":
				if err := os.WriteFile(filepath.Join(common, "info", "grafts"), []byte(target+" "+upstream+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "locked":
				lock, err := workspaceLock(filepath.Join(common, "ghraft-ancestry"))
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			case "replacement":
				tree := fixtureGit(t, f.LocalRepository, "", "rev-parse", target+"^{tree}")
				fake := syntheticCommit(t, f.LocalRepository, tree, upstream)
				fixtureGit(t, f.LocalRepository, "", "replace", target, fake)
			}
			r, err := openForkGitRepository(t.Context(), f)
			if scenario != "missing-commit" && scenario != "replacement" {
				if err == nil {
					r.close()
					t.Fatal("unsafe history accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer r.close()
			if scenario == "missing-commit" {
				if _, err = r.contains(t.Context(), strings.Repeat("a", 40), target); err == nil {
					t.Fatal("missing object was interpreted as non-ancestry")
				}
			} else if yes, err := r.contains(t.Context(), upstream, target); err != nil || yes {
				t.Fatal("replacement ref changed ancestry", yes, err)
			}
		})
	}
}
