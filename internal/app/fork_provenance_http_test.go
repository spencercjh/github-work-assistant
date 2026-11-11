package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAdaptedProvenanceLargeHTTPBody(t *testing.T) {
	s, g, api := forkFixtureConfigured(t, true)
	s.Config.Repositories[0].GitHubActions = []string{"push", "open-pr", "edit-pr", "merge"}
	after := fixtureGit(t, g.repository, "", "rev-parse", g.upstream+"^")
	upstreamTree := fixtureGit(t, g.repository, "", "rev-parse", g.upstream+"^{tree}")
	mergedTree := fixtureGit(t, g.repository, "", "merge-tree", "--write-tree", g.target, g.upstream)
	upstream := []string{g.upstream}
	for len(upstream) < 50 {
		object := "tree " + upstreamTree + "\nparent " + g.upstream + "\nauthor Test Fixture <fixture@example.invalid> 1700000002 +0000\ncommitter Test Fixture <fixture@example.invalid> 1700000002 +0000\n\n" + strings.Repeat("s", 80) + "\n"
		g.upstream = fixtureGit(t, g.repository, object, "hash-object", "-w", "-t", "commit", "--stdin")
		upstream = append(upstream, g.upstream)
	}
	if err := s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, _ := currentForkInput(t, s)
	if _, err := s.StartForkBatch(t.Context(), ForkBatchInput{WorkInput: in, After: after, Count: 50, Reason: "Exercise the supported batch size through HTTP."}); err != nil {
		t.Fatal(err)
	}
	in, w := currentForkInput(t, s)
	ws, err := s.PrepareWorkspace(t.Context(), in, g.repository)
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]ForkCommitMapping, len(upstream))
	for i, sha := range upstream {
		tree := upstreamTree
		if i%2 == 1 {
			tree = mergedTree
		}
		ws.Head = mappedCommit(t, g.repository, tree, ws.Head, sha)
		rows[i] = ForkCommitMapping{Upstream: sha, Commit: ws.Head, Status: "ported", Reason: strings.Repeat("x", 1000)}
	}
	fixtureGit(t, ws.Path, "", "reset", "--hard", ws.Head)
	writeMapping(t, ws.MappingFile, rows)
	proof, err := s.TestWorkspace(t.Context(), in)
	if err != nil || !proof.Passed {
		t.Fatal("supported mapping failed validation", err)
	}
	body, err := os.ReadFile(proof.PRBodyFile)
	if err != nil {
		t.Fatal(err)
	}
	g.prBranch, g.remoteHead = w.Fork.Branch, ws.Head
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, _ = currentForkInput(t, s)
	check := GitHubActionInput{WorkInput: in, Head: ws.Head, Base: g.target, Ref: w.Fork.Branch, Action: "open-pr", Body: string(body)}
	if len(canonical(check)) <= 64<<10 {
		t.Fatal("fixture must exceed the original HTTP request limit")
	}

	// Exercise authentication and task ownership through local fixture services.
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/oauth/userinfo" {
			_ = json.NewEncoder(w).Encode(Identity{Type: "agent", Subject: s.Config.AgentID, ServerID: s.Config.ServerID, ClientID: s.Config.ClientID})
			return
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(identity.Close)
	s.Raft.Config.RaftAPI = identity.URL
	cookie := strings.Repeat("f", 64)
	if err = s.Store.SaveSecret(t.Context(), s.sessionKey(cookie), []byte("synthetic-session"), time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	request := func(action string, raw []byte, authenticated bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/actions/"+action, bytes.NewReader(raw))
		if authenticated {
			r.AddCookie(&http.Cookie{Name: "ghraft_session", Value: cookie})
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		return recorder
	}
	assertStatus := func(raw []byte, want int) {
		t.Helper()
		got := request("check-github-action", raw, true)
		if got.Code != want {
			t.Fatalf("request %d bytes: got %d %s, want %d", len(raw), got.Code, got.Body.String(), want)
		}
	}
	assertStatus(canonical(check), http.StatusOK)
	g.pr = 1
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	check.WorkInput, _ = currentForkInput(t, s)
	check.Action = "edit-pr"
	assertStatus(canonical(check), http.StatusOK)
	check.PRBase = check.Base
	assertStatus(canonical(check), http.StatusOK)
	check.PRBase = strings.Repeat("a", 40)
	assertStatus(canonical(check), http.StatusConflict)
	check.Action, check.Ref, check.PRBase = "merge", "main", check.Base
	assertStatus(canonical(check), http.StatusOK)
	check.PRBase = strings.Repeat("a", 40)
	assertStatus(canonical(check), http.StatusConflict)
	check.Action, check.Ref, check.PRBase = "edit-pr", w.Fork.Branch, check.Base
	// JSON escapes '<' to six bytes. The complete allowed body must fit even
	// with this expansion, rather than just when the source text is plain ASCII.
	check.Body += strings.Repeat("<", (128<<10)-len(check.Body))
	assertStatus(canonical(check), http.StatusOK)
	if got := request("check-github-action", canonical(check), false); got.Code != http.StatusUnauthorized {
		t.Fatal("large body bypassed authentication", got.Code)
	}
	check.Body += "x"
	assertStatus(canonical(check), http.StatusConflict)
	if _, err = s.CheckGitHubAction(t.Context(), check); err == nil {
		t.Fatal("local action accepted an oversized body")
	}
	check.Body = string(body)
	raw := canonical(check)
	assertStatus(append(raw, bytes.Repeat([]byte(" "), (1<<20)-len(raw))...), http.StatusOK)
	assertStatus(append(raw, bytes.Repeat([]byte(" "), (1<<20)+1-len(raw))...), http.StatusBadRequest)
	assertStatus(append(raw, []byte(" {}")...), http.StatusBadRequest)
	unknown := append(append([]byte{}, raw[:len(raw)-1]...), []byte(",\"unknown\":true}")...)
	assertStatus(unknown, http.StatusBadRequest)
	ordinary := append([]byte("{}"), bytes.Repeat([]byte(" "), 64<<10)...)
	if got := request("list-events", ordinary, true); got.Code != http.StatusBadRequest {
		t.Fatal("unrelated endpoint request limit changed", got.Code)
	}

	// A small mapping can expand beyond the supported PR body size when its
	// evidence is escaped for Markdown. Reject it before issuing a test proof.
	for i := range rows {
		rows[i].Reason = strings.Repeat("&", 1000)
	}
	writeMapping(t, ws.MappingFile, rows)
	if proof, err = s.TestWorkspace(t.Context(), check.WorkInput); err == nil || proof.Passed || !strings.Contains(err.Error(), "128 KiB") {
		t.Fatal("oversized generated table accepted", proof.Passed, err)
	}
	p, err := s.Store.ForkProgress(t.Context(), w.Event.Key)
	if err != nil || p.ValidatedHead != "" || p.ValidatedMapping != "" {
		t.Fatal("previous proof survived failed table validation", err)
	}
}
