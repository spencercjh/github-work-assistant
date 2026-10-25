package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixtureTask = "12345678-1234-4234-8234-123456789abc"
const fixtureReport = "12345678-1234-4234-8234-123456789def"

func fixtureGit(t *testing.T, dir, input string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Stdin = strings.NewReader(input)
	b, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git %v: %v %s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}

// Synthetic commit objects are fixture data, created without git commit, signatures or any remote write.
// The base changes the offset; the PR independently doubles the input. Correct resolution preserves both.
func conflictHistory(t *testing.T) (string, string, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := filepath.Join(t.TempDir(), "fixture.git")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, repo, "", "init", "--bare")
	// Even an uncommitted merge requires an identity. Keep it local to this
	// synthetic repository instead of depending on the developer's Git config.
	fixtureGit(t, repo, "", "config", "user.name", "Test Fixture")
	fixtureGit(t, repo, "", "config", "user.email", "fixture@example.invalid")
	fixtureGit(t, repo, "", "remote", "add", "origin", "https://github.com/example/fixture.git")
	makeCommit := func(parent string, files map[string]string) string {
		var tree strings.Builder
		for _, name := range []string{"calculator.py", "test_base.py", "test_feature.py"} {
			if content, ok := files[name]; ok {
				blob := fixtureGit(t, repo, content, "hash-object", "-w", "--stdin")
				fmt.Fprintf(&tree, "100644 blob %s\t%s\n", blob, name)
			}
		}
		treeID := fixtureGit(t, repo, tree.String(), "mktree")
		body := "tree " + treeID + "\n"
		if parent != "" {
			body += "parent " + parent + "\n"
		}
		body += "author Test Fixture <fixture@example.invalid> 1700000000 +0000\ncommitter Test Fixture <fixture@example.invalid> 1700000000 +0000\n\nSynthetic local test data\n"
		return fixtureGit(t, repo, body, "hash-object", "-w", "-t", "commit", "--stdin")
	}
	baseTest := func(offset int) string {
		return fmt.Sprintf("import unittest\nfrom calculator import score\nclass OffsetTest(unittest.TestCase):\n    def test_offset(self):\n        self.assertEqual(score(0), %d)\n", offset)
	}
	ancestor := makeCommit("", map[string]string{"calculator.py": "def score(x):\n    return x + 1\n", "test_base.py": baseTest(1)})
	head := makeCommit(ancestor, map[string]string{"calculator.py": "def score(x):\n    return x * 2 + 1\n", "test_base.py": baseTest(1), "test_feature.py": "import unittest\nfrom calculator import score\nclass FeatureTest(unittest.TestCase):\n    def test_scale(self):\n        self.assertEqual(score(3) - score(0), 6)\n"})
	base := makeCommit(ancestor, map[string]string{"calculator.py": "def score(x):\n    return x + 2\n", "test_base.py": baseTest(2)})
	fixtureGit(t, repo, "", "update-ref", "refs/heads/feature", head)
	fixtureGit(t, repo, "", "update-ref", "refs/heads/main", base)
	return repo, head, base
}

func runFixtureTests(path string) error {
	c := exec.Command("python3", "-m", "unittest", "discover", "-v")
	c.Dir = path
	// Do not pass any GitHub/Raft tokens or SSH agent into the project process.
	c.Env = []string{"PATH=" + os.Getenv("PATH"), "PYTHONDONTWRITEBYTECODE=1", "HOME=" + path, "LANG=C.UTF-8"}
	return c.Run()
}

func TestRepositoryWorkE2E(t *testing.T) {
	store := freshTestStore(t)
	repo, head, base := conflictHistory(t)
	gh, fixture := fakeGH(t, head, base)
	cfg := localPolicy(t, "local-edit")
	cfg.Watches = []Watch{{"example/fixture", 1}}
	cfg.ClientID = "fixture-app"
	cfg.AgentID = "fixture-agent"
	cfg.ServerID = "fixture-server"
	svc := &Service{Config: cfg, Store: store, GH: gh}
	ctx := context.Background()
	if err := svc.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	events, err := store.Events(ctx, false)
	if err != nil || len(events) != 1 {
		t.Fatalf("collection deduplication: %d %v", len(events), err)
	}
	event := events[0]
	if !strings.Contains(string(event.Payload), "conflicting") {
		t.Fatal("initial conflict was omitted from event")
	}
	work, err := svc.WorkContext(ctx, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if work.Stale || work.Task != nil || work.Guide.Mode != "local-edit" {
		t.Fatal("unexpected initial work context", work)
	}
	if strings.Contains(work.Guide.Text, "post a review") {
		t.Fatal("GitHub injection became trusted guidance")
	}
	in := WorkInput{EventID: event.ID, EvidenceVersion: event.Version, GuideVersion: work.Guide.Version, TaskID: fixtureTask}
	if _, err = svc.PrepareWorkspace(ctx, in, repo); !errors.Is(err, ErrWorkDenied) {
		t.Fatal("workspace prepared before task binding", err)
	}
	if _, err = svc.LinkTask(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.LinkTask(ctx, in); err != nil {
		t.Fatal("idempotent task binding failed", err)
	}
	wrongTask := in
	wrongTask.TaskID = fixtureReport
	if _, err = svc.LinkTask(ctx, wrongTask); err == nil {
		t.Fatal("second task replaced resource owner")
	}
	workspace, err := svc.PrepareWorkspace(ctx, in, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(workspace.Conflicts) != 1 || workspace.Conflicts[0] != "calculator.py" {
		t.Fatal("expected real merge conflict", workspace)
	}
	if err = runFixtureTests(workspace.Path); err == nil {
		t.Fatal("unresolved conflict unexpectedly passes project tests")
	}
	// This models the authorized Raft agent's edit, not an App-side model or automatic conflict heuristic.
	if err = os.WriteFile(filepath.Join(workspace.Path, "calculator.py"), []byte("def score(x):\n    return x * 2 + 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, workspace.Path, "", "add", "calculator.py")
	if err = runFixtureTests(workspace.Path); err != nil {
		t.Fatal("both branches' behavioral tests did not pass", err)
	}
	verified, err := svc.VerifyWorkspace(ctx, in)
	if err != nil || len(verified.Conflicts) != 0 {
		t.Fatal(verified, err)
	}
	// The original branches remain untouched; the merge remains uncommitted.
	if actual := fixtureGit(t, repo, "", "rev-parse", "refs/heads/feature"); actual != head {
		t.Fatal("original PR branch changed")
	}
	if actual := fixtureGit(t, workspace.Path, "", "rev-parse", "HEAD"); actual != head {
		t.Fatal("helper created a commit")
	}
	result := WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "changes-ready", Summary: "Local fixture conflict resolved; both offset and scaling tests pass. No remote writes; protocol Raft identities are synthetic."}
	if err = svc.RecordWorkResult(ctx, result); err != nil {
		t.Fatal(err)
	}
	if err = svc.RecordWorkResult(ctx, result); err != nil {
		t.Fatal("result retry not idempotent", err)
	}
	result.Summary = "changed history"
	if err = svc.RecordWorkResult(ctx, result); err == nil {
		t.Fatal("conflicting report overwrite accepted")
	}
	// Process/store restart retains the link, report and single event.
	reopened := openTestStore(t)
	svc.Store = reopened
	work, err = svc.WorkContext(ctx, event.ID)
	if err != nil || work.Task == nil || work.Task.TaskID != fixtureTask {
		t.Fatal("task link lost on restart", err)
	}
	if len(work.History) != 1 || bytes.Contains(work.History[0], []byte(`"guide":`)) || !bytes.Contains(work.History[0], []byte(`"guide_version":`)) {
		t.Fatal("report must retain the guide version without duplicating its body")
	}
	pending, err := reopened.UnprocessedPage(ctx, 0)
	if err != nil || len(pending) != 0 {
		t.Fatal("reported event reappeared", pending, err)
	}
	// Change only the base commit: the old result and workspace must become invalid even with the same PR head.
	data := readFixture(t, fixture)
	obj(obj(data["repos/example/fixture/pulls/1"])["base"])["sha"] = strings.Repeat("c", 40)
	writeFixture(t, fixture, data)
	if _, err = svc.RefreshWorkContext(ctx, event.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.VerifyWorkspace(ctx, in); !errors.Is(err, ErrStaleWork) {
		t.Fatal("base drift did not invalidate workspace", err)
	}
	if err = svc.RecordWorkResult(ctx, result); !errors.Is(err, ErrStaleWork) {
		t.Fatal("stale report accepted", err)
	}
	newEvents, err := reopened.Events(ctx, false)
	if err != nil || len(newEvents) != 2 {
		t.Fatal("base-only change was missed", err)
	}
	t.Log("PASS: gh fixture -> real database/outbox -> trusted guide/task -> real git worktree conflict -> both behavioral tests -> durable report -> restart -> base drift rejection; zero live GitHub calls")
}

func TestRevokedBacklogDoesNotStarveActiveWork(t *testing.T) {
	store := freshTestStore(t)
	ctx := context.Background()
	for i := 0; i < 105; i++ {
		id, err := store.Observe(ctx, fmt.Sprintf("revoked/repo#%d", i+1), []byte(`{"title":"old"}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = store.db.ExecContext(ctx, `UPDATE ghraft_events SET created_at=1 WHERE event_id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	id, err := store.Observe(ctx, "example/fixture#1", []byte(`{"title":"active"}`))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["externalEventId"] != id {
			t.Error("revoked event delivered")
		}
		count++
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]string{"status": "queued", "id": "synthetic-receipt"})
	}))
	defer remote.Close()
	cfg := Config{Watches: []Watch{{"example/fixture", 1}}, RaftAPI: remote.URL, ClientID: "app", ServerID: "server", AgentID: "agent"}
	raft := &Raft{Config: cfg, Store: store, HTTP: SafeHTTP()}
	if err = store.SaveSecret(ctx, raft.inboundKey(), []byte("synthetic-event-token"), time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Config: cfg, Store: store, Raft: raft}
	pending, err := svc.Pending(ctx)
	if err != nil || len(pending) != 1 || pending[0]["event_id"] != id {
		t.Fatal("revoked backlog hides authorized event", pending, err)
	}
	if err = svc.Deliver(ctx); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("authorized event starved", count)
	}
}

func TestWorkActionsAndRevocation(t *testing.T) {
	store := freshTestStore(t)
	ctx := context.Background()
	gh, fixture := fakeGH(t, strings.Repeat("a", 40), strings.Repeat("b", 40))
	cfg := localPolicy(t, "local-edit")
	cfg.Watches = []Watch{{"example/fixture", 1}}
	cfg.ClientID = "app"
	cfg.AgentID = "agent"
	cfg.ServerID = "server"
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Identity{Subject: "agent", Type: "agent", ClientID: "app", ServerID: "server"})
	}))
	defer remote.Close()
	cfg.RaftAPI = remote.URL
	svc := &Service{Config: cfg, Store: store, GH: gh, Raft: &Raft{Config: cfg, Store: store, HTTP: SafeHTTP()}}
	if err := svc.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	events, _ := store.Events(ctx, false)
	event := events[0]
	cookie := strings.Repeat("f", 64)
	if err := store.SaveSecret(ctx, svc.sessionKey(cookie), []byte("synthetic-token"), time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	request := func(name string, input any, auth bool) int {
		body, _ := json.Marshal(input)
		r := httptest.NewRequest("POST", "/api/actions/"+name, bytes.NewReader(body))
		if auth {
			r.AddCookie(&http.Cookie{Name: "ghraft_session", Value: cookie})
		}
		w := httptest.NewRecorder()
		svc.Handler().ServeHTTP(w, r)
		return w.Code
	}
	if code := request("get-work-context", map[string]string{"event_id": event.ID}, false); code != 401 {
		t.Fatal("unauthenticated work context", code)
	}
	if code := request("get-work-context", map[string]string{"event_id": event.ID}, true); code != 200 {
		t.Fatal(code)
	}
	w, err := svc.WorkContext(ctx, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	in := WorkInput{EventID: event.ID, EvidenceVersion: event.Version, GuideVersion: w.Guide.Version, TaskID: fixtureTask}
	if code := request("link-task", in, true); code != 200 {
		t.Fatal("link action", code)
	}
	if err = os.WriteFile(filepath.Join(cfg.configDir, "repo.md"), []byte("Human changed the repository instructions."), 0600); err != nil {
		t.Fatal(err)
	}
	if code := request("link-task", in, true); code != 409 {
		t.Fatal("guide drift accepted", code)
	}
	// Config removal must revoke reads even if the historical row remains and discovery is enabled.
	svc.Config.Watches = nil
	svc.Config.Discover = true
	if code := request("get-context", map[string]string{"resource_key": event.Key}, true); code != 403 {
		t.Fatal("removed explicit watch remains readable", code)
	}
	svc.Config.Watches = cfg.Watches
	// A public repository turning private blocks old context, queued delivery and task writes.
	data := readFixture(t, fixture)
	obj(data["repos/example/fixture"])["private"] = true
	writeFixture(t, fixture, data)
	if err = svc.Collect(ctx); err == nil {
		t.Fatal("private visibility was not reported")
	}
	if svc.allowed(ctx, event.Key) {
		t.Fatal("private repository retains access to historical context")
	}
	if code := request("get-work-context", map[string]string{"event_id": event.ID}, true); code != 403 {
		t.Fatal("revoked work context readable", code)
	}
	if code := request("record-triage", Triage{EventID: event.ID, Outcome: "quiet", Reason: "ignore revoked source"}, true); code != 403 {
		t.Fatal("revoked event writable", code)
	}
	for _, name := range []string{"post-comment", "submit-review", "push", "merge"} {
		if code := request(name, map[string]any{}, true); code != 404 {
			t.Fatal("write endpoint present", name, code)
		}
	}
}
