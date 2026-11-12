package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type countedQueueSource struct {
	*queueSource
	branches []string
}

func (g *countedQueueSource) CollectFork(ctx context.Context, f ForkSpec, p ForkProgress) (ForkObservation, error) {
	g.branches = append(g.branches, p.Branch)
	return g.queueSource.CollectFork(ctx, f, p)
}

func TestCompletedQueueSkipsHistoricalPRButChecksLiveFrontier(t *testing.T) {
	s, q, _, after := newQueueFixture(t)
	appendQueue(t, s, after)
	a := publishQueue(t, s, q)
	done := mergeQueue(t, s, q, a.BatchID)
	// No historical PR endpoint is configured. The accepted immutable proof
	// must survive deleted sync branches and later comment/CI changes.
	g, path := fakeGH(t, q.upstream, q.target)
	ref := func(sha string) any {
		return map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": sha, "type": "commit"}}
	}
	data := map[string]any{
		"user":                  map[string]any{"login": g.Login},
		"repos/source/fixture":  map[string]any{"full_name": "source/fixture", "private": false},
		"repos/example/fixture": map[string]any{"full_name": "example/fixture", "private": false},
		"repos/source/fixture/git/ref/heads/main":  ref(q.upstream),
		"repos/example/fixture/git/ref/heads/main": ref(q.target),
	}
	writeFixture(t, path, data)
	log := filepath.Join(t.TempDir(), "requests")
	t.Setenv("GHRAFT_REQUEST_LOG", log)
	s.GH, s.Config.Discover = g, false
	for range 2 {
		if err := s.Collect(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	calls, err := os.ReadFile(log)
	if err != nil || strings.Count(string(calls), "\n") != 12 || strings.Contains(string(calls), "/pulls") {
		t.Fatal("completed queue should use six live reads per cycle", string(calls), err)
	}
	got, err := s.Store.ForkProgress(t.Context(), s.Config.Forks[0].Key())
	if err != nil || got.TestLogHash != done.TestLogHash || string(canonical(got.Validation)) != string(canonical(done.Validation)) || got.Queue.MergedThrough != done.Queue.MergedThrough || got.Queue.MergedTarget != done.Queue.MergedTarget {
		t.Fatal("skipping historical requests altered accepted proof/frontier", err)
	}
	// Fresh upstream discovery is retained even when every batch is completed.
	next := syntheticCommit(t, q.repository, fixtureGit(t, q.repository, "", "rev-parse", q.upstream+"^{tree}"), q.upstream)
	data["repos/source/fixture/git/ref/heads/main"] = ref(next)
	writeFixture(t, path, data)
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err = s.Store.ForkProgress(t.Context(), s.Config.Forks[0].Key())
	if err != nil || got.ObservedUpstream != next || got.Upstream != done.Upstream || got.TestLogHash != done.TestLogHash || string(canonical(got.Validation)) != string(canonical(done.Validation)) {
		t.Fatal("new upstream was lost or rewrote completed proof", err)
	}
	// A force-reset integration branch may not reuse the accepted cursor.
	data["repos/example/fixture/git/ref/heads/main"] = ref(a.InitialTarget)
	writeFixture(t, path, data)
	if err = s.Collect(t.Context()); err == nil || !strings.Contains(err.Error(), "accepted queue frontier") {
		t.Fatal("accepted merge disappeared without rejection", err)
	}
	data["repos/example/fixture"] = map[string]any{"_http_status": 403}
	writeFixture(t, path, data)
	if err = s.Collect(t.Context()); !errors.Is(err, ErrGitHubAccessDenied) {
		t.Fatal("completed queue bypassed live repository access", err)
	}
}

func TestMixedQueueStillRefreshesEveryUnfinishedBatch(t *testing.T) {
	s, q, _, after := newQueueFixture(t)
	appendQueue(t, s, after)
	a := publishQueue(t, s, q)
	appendQueue(t, s, "")
	b := publishQueue(t, s, q)
	mergeQueue(t, s, q, a.BatchID)
	g := &countedQueueSource{queueSource: q}
	s.GH = g
	// Deleting only the completed predecessor must not cause another PR read.
	delete(q.prs, a.Branch)
	if err := s.Collect(t.Context()); err != nil || len(g.branches) != 1 || g.branches[0] != b.Branch {
		t.Fatal("historical PR scanned or active descendant skipped", g.branches, err)
	}
	// Incomplete work is never assumed unchanged: failure and missing response
	// still block the dependency chain despite the preceding accepted merge.
	q.missing = b.Branch
	if err := s.Collect(t.Context()); err == nil {
		t.Fatal("missing unfinished PR was silently reused")
	}
}
