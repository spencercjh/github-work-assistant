package app

import (
	"os"
	"strings"
	"testing"
)

func mappedCommit(t *testing.T, repo, tree, parent, upstream string) string {
	t.Helper()
	body := "tree " + tree + "\nparent " + parent + "\nauthor Test Fixture <fixture@example.invalid> 1700000002 +0000\ncommitter Test Fixture <fixture@example.invalid> 1700000002 +0000\n\nPort upstream change\n\nUpstream-Commit: " + upstream + "\n"
	return fixtureGit(t, repo, body, "hash-object", "-w", "-t", "commit", "--stdin")
}

func writeMapping(t *testing.T, path string, rows []ForkCommitMapping) {
	t.Helper()
	if err := os.WriteFile(path, canonical(rows), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAdaptedProvenanceRejectsInvalidHistory(t *testing.T) {
	for _, scenario := range []string{"missing item", "wrong upstream", "missing trailer", "multiple trailers", "extra commit", "empty commit", "merge commit", "skip with commit", "no evidence", "unknown status"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, _, in, _, ws := beginAdaptedFixture(t)
			rows, err := readForkMapping(ws.MappingFile)
			if err != nil {
				t.Fatal(err)
			}
			tree := fixtureGit(t, ws.Path, "", "rev-parse", "HEAD^{tree}")
			switch scenario {
			case "missing item":
				rows = nil
			case "wrong upstream":
				rows[0].Upstream = g.target
			case "missing trailer":
				rows[0].Commit = syntheticCommit(t, g.repository, tree, g.target)
			case "multiple trailers":
				rows[0].Commit = mappedCommit(t, g.repository, tree, g.target, g.upstream+"\nUpstream-Commit: "+g.upstream)
			case "extra commit":
				fixtureGit(t, ws.Path, "", "reset", "--hard", syntheticCommit(t, g.repository, tree, ws.Head))
			case "empty commit":
				baseTree := fixtureGit(t, ws.Path, "", "rev-parse", g.target+"^{tree}")
				rows[0].Commit = mappedCommit(t, g.repository, baseTree, g.target, g.upstream)
			case "merge commit":
				rows[0].Commit = syntheticCommit(t, g.repository, tree, g.target, g.upstream)
			case "skip with commit":
				rows[0].Status = "already-present"
			case "no evidence":
				rows[0].Reason = ""
			case "unknown status":
				rows[0].Status = "ignored"
			}
			if len(rows) > 0 && rows[0].Commit != ws.Head {
				fixtureGit(t, ws.Path, "", "reset", "--hard", rows[0].Commit)
			}
			writeMapping(t, ws.MappingFile, rows)
			if proof, err := s.TestWorkspace(t.Context(), in); err == nil || proof.Passed {
				t.Fatal("invalid provenance accepted", proof)
			}
		})
	}
}

func TestAdaptedProvenanceOrderedBatchAndSkippedItems(t *testing.T) {
	s, g, _, _, w, ws := beginAdaptedFixture(t)
	f, _ := s.Config.Fork(w.Event.Key)
	p := *w.Fork
	tree := fixtureGit(t, ws.Path, "", "rev-parse", ws.Head+"^{tree}")
	u2 := syntheticCommit(t, g.repository, tree, g.upstream)
	u3 := syntheticCommit(t, g.repository, tree, u2)
	u4 := syntheticCommit(t, g.repository, tree, u3)
	p.BatchCommits = []string{g.upstream, u2, u3, u4}
	// A second real adaptation reverses the first; this test is about the
	// provenance contract, not its semantics (covered by regression commands).
	baseTree := fixtureGit(t, ws.Path, "", "rev-parse", g.target+"^{tree}")
	head := mappedCommit(t, g.repository, baseTree, ws.Head, u4)
	fixtureGit(t, ws.Path, "", "reset", "--hard", head)
	rows := []ForkCommitMapping{
		{g.upstream, ws.Head, "ported", "Adapt scale without replacing custom offset"},
		{u2, "", "already-present", "Equivalent behavior in existing source"},
		{u3, "", "not-applicable", "Upstream-only distribution; see <policy> | details"},
		{u4, head, "ported", "Reconcile subsequent behavior"},
	}
	ws.Head = head
	writeMapping(t, ws.MappingFile, rows)
	_, section, err := validateForkMapping(t.Context(), f, &p, ws)
	if err != nil || !strings.Contains(section, "&lt;policy&gt; &#124; details") {
		t.Fatal(section, err)
	}
	for i := 1; i < len(rows); i++ {
		if strings.Index(section, rows[i-1].Upstream) >= strings.Index(section, rows[i].Upstream) {
			t.Fatal("PR table order differs from upstream mainline")
		}
	}
	for _, scenario := range []string{"reordered", "same local commit", "duplicate upstream"} {
		changed := append([]ForkCommitMapping(nil), rows...)
		switch scenario {
		case "reordered":
			changed[0], changed[3] = changed[3], changed[0]
		case "same local commit":
			changed[3].Commit = rows[0].Commit
		case "duplicate upstream":
			changed[3].Upstream = rows[0].Upstream
		}
		writeMapping(t, ws.MappingFile, changed)
		if _, _, err := validateForkMapping(t.Context(), f, &p, ws); err == nil {
			t.Fatal("accepted", scenario)
		}
	}
}

func TestAdaptedProvenanceEnforcedAtPRAndResultBoundaries(t *testing.T) {
	s, g, _, in, w, ws := beginAdaptedFixture(t)
	s.Config.Repositories[0].GitHubActions = []string{"push", "open-pr", "edit-pr", "merge"}
	in, _ = currentForkInput(t, s)
	proof, err := s.TestWorkspace(t.Context(), in)
	if err != nil {
		t.Fatal(err)
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
	check := GitHubActionInput{WorkInput: in, Head: ws.Head, Base: g.target, Ref: w.Fork.Branch, Action: "open-pr"}
	if _, err = s.CheckGitHubAction(t.Context(), check); err == nil {
		t.Fatal("PR without mapping accepted")
	}
	check.Body = "Problem and validation\n\n" + string(body)
	if _, err = s.CheckGitHubAction(t.Context(), check); err != nil {
		t.Fatal(err)
	}
	g.pr = 1
	badBody := "No upstream details"
	g.prBody = &badBody
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, _ = currentForkInput(t, s)
	check.WorkInput, check.Action = in, "edit-pr"
	if _, err = s.CheckGitHubAction(t.Context(), check); err != nil {
		t.Fatal("metadata repair rejected", err)
	}
	check.Body = badBody
	if _, err = s.CheckGitHubAction(t.Context(), check); err == nil {
		t.Fatal("metadata removed mapping")
	}
	check.Action, check.Ref = "merge", "main"
	if _, err = s.CheckGitHubAction(t.Context(), check); err == nil {
		t.Fatal("remote PR without mapping merged")
	}
	if err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, Outcome: "changes-ready", ReportID: fixtureReport, Summary: "Ready"}); err == nil {
		t.Fatal("missing remote mapping accepted as changes-ready")
	}
	goodBody := string(body)
	g.prBody = &goodBody
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, _ = currentForkInput(t, s)
	check.WorkInput = in
	if _, err = s.CheckGitHubAction(t.Context(), check); err != nil {
		t.Fatal(err)
	}
	rows, _ := readForkMapping(ws.MappingFile)
	rows[0].Reason += " changed after testing"
	writeMapping(t, ws.MappingFile, rows)
	check.Action, check.Ref = "push", w.Fork.Branch
	if _, err = s.CheckGitHubAction(t.Context(), check); err == nil {
		t.Fatal("modified mapping reused proof")
	}
}

func TestAdaptedMappingChangeDuringTestsInvalidatesProof(t *testing.T) {
	s, _, _, _, w, ws := beginAdaptedFixture(t)
	s.Config.Repositories[0].TestCommands = [][]string{{"python3", "-c", "import json,sys; p=sys.argv[1]; v=json.load(open(p)); v[0]['reason']='changed'; json.dump(v,open(p,'w'))", ws.MappingFile}}
	in, _ := currentForkInput(t, s)
	if proof, err := s.TestWorkspace(t.Context(), in); err == nil || proof.Passed {
		t.Fatal("mapping mutation accepted")
	}
	p, err := s.Store.ForkProgress(t.Context(), w.Event.Key)
	if err != nil || p.ValidatedHead != "" || p.ValidatedMapping != "" {
		t.Fatal("proof survived mapping mutation", err)
	}
}

func TestAdaptedHistoricalCompletionDoesNotAuthorizeUnmappedRound(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unfinished", true: "completed"}[completed], func(t *testing.T) {
			s, g, _, in, w, ws := beginAdaptedFixture(t)
			proof, err := s.TestWorkspace(t.Context(), in)
			if err != nil {
				t.Fatal(err)
			}
			p, _ := s.Store.ForkProgress(t.Context(), w.Event.Key)
			d, _ := s.Config.WorkDefinition(w.Event.Key)
			previous, _ := d.guide(s.Config, adaptedMergeInstruction)
			p.ValidatedGuide, p.ValidatedMapping, p.ValidatedPRSection = previous.Version, "", ""
			if completed {
				p.Phase, p.LastSuccess = "synced", p.Upstream
			}
			if err = s.Store.SaveForkProgress(t.Context(), w.Event.Key, p.Revision, p, nil); err != nil {
				t.Fatal(err)
			}
			g.pr, g.prBranch, g.remoteHead = 1, p.Branch, ws.Head
			g.mergeCommit = syntheticCommit(t, g.repository, proof.Tree, g.target)
			g.target, g.closed, g.merged = g.mergeCommit, true, true
			if err = s.Collect(t.Context()); err != nil {
				t.Fatal(err)
			}
			in, _ = currentForkInput(t, s)
			err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, Outcome: "merged", ReportID: fixtureReport, Summary: "Verify actual historical merge"})
			if (err == nil) != completed {
				t.Fatal("historical exemption mismatch", completed, err)
			}
		})
	}
}
