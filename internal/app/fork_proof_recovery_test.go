package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func proofRecoveryFixture(t *testing.T) (*Service, *forkFixtureSource, *taskAPI, WorkInput, WorkContext) {
	t.Helper()
	s, g, api, in, w, ws := beginAdaptedFixture(t)
	// Restoring proof of an already completed human merge needs no delegation
	// to perform a merge, and must not confer that permission.
	s.Config.Repositories[0].GitHubActions = []string{"push", "open-pr"}
	in, _ = currentForkInput(t, s)
	proof, err := s.TestWorkspace(t.Context(), in)
	if err != nil || !proof.Passed {
		t.Fatal(proof, err)
	}
	g.pr, g.prBranch, g.remoteHead, g.prBase = 1, w.Fork.Branch, ws.Head, g.target
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, archived := currentForkInput(t, s)
	file := filepath.Join(t.TempDir(), "pre-merge.json")
	b := canonical(archived)
	if err = os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	g.mergeCommit = syntheticCommit(t, g.repository, proof.Tree, g.target)
	g.target, g.closed, g.merged = g.mergeCommit, true, true
	g.mergedAt = time.Now().UTC().Format(time.RFC3339)
	// The actual last attempt tested the old head against the post-merge base.
	// Preserve that fact. Only the independently archived proof can bind the
	// original base. Production obtained this attempt by running test-worktree.
	p := *archived.Fork
	p.ValidatedBase = g.target
	v := *p.Validation
	v.Base, v.ID = g.target, strings.Repeat("c", 64)
	p.Validation = &v
	if err = s.Store.SaveForkProgress(t.Context(), w.Event.Key, p.Revision, p, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, _ = currentForkInput(t, s)
	s.Config.ForkProofRecoveries = []ForkProofRecovery{{ForkID: "custom", PR: 1, MergeCommit: g.mergeCommit, ProofFile: file, ProofSHA256: hash(b), Reason: "Operator approved recovery using original proof. Preserve source author discrepancy; no history rewrite."}}
	return s, g, api, in, archived
}

func TestRecoverForkProofThenOrdinaryMergedResult(t *testing.T) {
	s, _, api, in, archived := proofRecoveryFixture(t)
	before, err := s.Store.ForkProgress(t.Context(), archived.Event.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "merged", Summary: "Human merge"}); err == nil {
		t.Fatal("post-merge proof incorrectly accepted for original PR")
	}
	receipt, err := s.RecoverForkProof(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.Store.ForkProgress(t.Context(), archived.Event.Key)
	if err != nil {
		t.Fatal(err)
	}
	want := restoreForkProof(before, *archived.Fork)
	want.Revision++
	if !reflect.DeepEqual(after, want) || !reflect.DeepEqual(receipt.Previous, before) || !reflect.DeepEqual(receipt.Archived, *archived.Fork) || receipt.Input != in || receipt.Grant.Reason == "" || receipt.RecoveredAt == 0 {
		t.Fatal("recovery changed unrelated progress or lost audit evidence", after, receipt)
	}
	// Repeating an identical command returns the immutable first receipt.
	repeated, err := s.RecoverForkProof(t.Context(), in)
	if err != nil || !reflect.DeepEqual(receipt, repeated) {
		t.Fatal("recovery retry was not idempotent", err)
	}
	if _, err = s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: in, Action: "merge", Head: archived.Fork.ValidatedHead, Base: archived.Fork.ValidatedBase, Ref: "main"}); err == nil {
		t.Fatal("historical recovery granted merge permission")
	}
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	in, _ = currentForkInput(t, s)
	if err = s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "merged", Summary: "Verified original pre-merge proof, actual human merge and source mapping; author discrepancy retained in recovery receipt"}); err != nil {
		t.Fatal(err)
	}
	done, err := s.Store.ForkProgress(t.Context(), archived.Event.Key)
	if err != nil || done.LastSuccess != archived.Fork.Upstream || done.Phase != "synced" || api.creates != 1 {
		t.Fatal("ordinary result failed to advance original task", done, err)
	}
	if _, err = s.RecoverForkProof(t.Context(), in); err == nil {
		t.Fatal("completed round accepted a second recovery")
	}
	// Removing the grant cannot revoke the proof of a completed human merge.
	s.Config.ForkProofRecoveries = nil
	if err = s.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, current := currentForkInput(t, s)
	if forkFacts(current)["merge_verified"] != true || current.Fork.LastSuccess != archived.Fork.Upstream {
		t.Fatal("removing temporary grant invalidated completion")
	}
}

func TestRecoverForkProofRejectsMismatchedEvidence(t *testing.T) {
	for _, scenario := range []string{"no grant", "wrong PR", "wrong merge grant", "archive hash", "failed proof", "missing proof", "wrong round", "wrong task", "wrong head", "wrong base", "wrong tree", "source batch", "post-merge proof", "archive log hash", "log bytes", "archive symlink", "public archive", "mapping", "PR body", "remote head", "remote base", "remote tree", "remote parent", "unreachable merge", "missing merge time", "owner", "guide", "running", "used grant", "changed during collection"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, api, in, archive := proofRecoveryFixture(t)
			grant := &s.Config.ForkProofRecoveries[0]
			switch scenario {
			case "no grant":
				s.Config.ForkProofRecoveries = nil
			case "wrong PR":
				grant.PR++
			case "wrong merge grant":
				grant.MergeCommit = archive.Fork.InitialTarget
			case "archive hash":
				grant.ProofSHA256 = strings.Repeat("f", 64)
			case "failed proof":
				archive.Fork.Validation.State = "failed"
			case "missing proof":
				archive.Fork.Validation = nil
			case "wrong round":
				archive.Fork.Round = strings.Repeat("f", 64)
			case "wrong task":
				archive.Task.TaskID = fixtureReport
			case "wrong head":
				archive.Fork.ValidatedHead = archive.Fork.InitialTarget
			case "wrong base":
				archive.Fork.ValidatedBase = g.target
			case "wrong tree":
				archive.Fork.ValidatedTree = strings.Repeat("f", 40)
			case "source batch":
				archive.Fork.BatchCommits = []string{archive.Fork.InitialTarget}
			case "post-merge proof":
				g.mergedAt = time.Unix(archive.Fork.Validation.StartedAt-1, 0).UTC().Format(time.RFC3339)
			case "archive log hash":
				archive.Fork.TestLogHash = strings.Repeat("f", 64)
			case "log bytes":
				if err := os.WriteFile(archive.Fork.TestLog, []byte("different run"), 0600); err != nil {
					t.Fatal(err)
				}
			case "archive symlink":
				link := grant.ProofFile + ".link"
				if err := os.Symlink(grant.ProofFile, link); err != nil {
					t.Fatal(err)
				}
				grant.ProofFile = link
			case "public archive":
				if err := os.Chmod(grant.ProofFile, 0644); err != nil {
					t.Fatal(err)
				}
			case "mapping":
				_, meta, _ := s.forkWorkspacePaths(archive.Fork.Round)
				rows, err := readForkMapping(meta + ".commits.json")
				if err != nil {
					t.Fatal(err)
				}
				rows[0].Reason = "changed source explanation"
				writeMapping(t, meta+".commits.json", rows)
			case "PR body":
				body := "source mapping removed"
				g.prBody = &body
			case "remote head":
				g.remoteHead = syntheticCommit(t, g.repository, archive.Fork.ValidatedTree, g.remoteHead)
			case "remote base":
				g.prBase = g.target
			case "remote tree", "remote parent", "unreachable merge":
				parent, tree := archive.Fork.ValidatedBase, archive.Fork.ValidatedTree
				if scenario == "remote parent" {
					parent = g.target
				}
				if scenario == "remote tree" {
					tree = fixtureGit(t, g.repository, "", "rev-parse", parent+"^{tree}")
				}
				g.mergeCommit = syntheticCommit(t, g.repository, tree, parent)
				grant.MergeCommit = g.mergeCommit
				g.target = g.mergeCommit
				if scenario == "unreachable merge" {
					g.target = parent
				}
			case "missing merge time":
				g.mergedAt = ""
			case "owner":
				api.owner = "successor"
			case "guide":
				s.Config.Repositories[0].Goal += " changed requirement"
			case "running", "used grant":
				if scenario == "used grant" {
					if _, err := s.RecoverForkProof(t.Context(), in); err != nil {
						t.Fatal(err)
					}
				}
				p, err := s.Store.ForkProgress(t.Context(), archive.Event.Key)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "running" {
					p.Validation.State = "running"
				} else {
					p.ValidatedBase = g.target
				}
				if err = s.Store.SaveForkProgress(t.Context(), archive.Event.Key, p.Revision, p, nil); err != nil {
					t.Fatal(err)
				}
			case "changed during collection":
				s.GH = &recoveryChangingSource{forkFixtureSource: g, api: api}
			}
			if slices.Contains([]string{"failed proof", "missing proof", "wrong round", "wrong task", "wrong head", "wrong base", "wrong tree", "source batch", "archive log hash"}, scenario) {
				b := canonical(archive)
				if err := os.WriteFile(grant.ProofFile, b, 0600); err != nil {
					t.Fatal(err)
				}
				grant.ProofSHA256 = hash(b) // Even an audited grant must pass structural checks.
			}
			before, _ := s.Store.ForkProgress(t.Context(), archive.Event.Key)
			if _, err := s.RecoverForkProof(t.Context(), in); err == nil {
				t.Fatal("unsafe recovery accepted")
			}
			after, _ := s.Store.ForkProgress(t.Context(), archive.Event.Key)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("denial modified progress")
			}
		})
	}
}

type recoveryChangingSource struct {
	*forkFixtureSource
	api *taskAPI
}

func (g *recoveryChangingSource) CollectFork(ctx context.Context, f ForkSpec, p ForkProgress) (ForkObservation, error) {
	o, err := g.forkFixtureSource.CollectFork(ctx, f, p)
	g.api.owner = "successor"
	return o, err
}

func TestRecoveryStoreFencesAndAtomicAudit(t *testing.T) {
	for _, scenario := range []string{"revision", "evidence", "audit conflict"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, _, in, archive := proofRecoveryFixture(t)
			p, _ := s.Store.ForkProgress(t.Context(), archive.Event.Key)
			r := ForkProofRecoveryReceipt{ID: strings.Repeat("d", 64), Resource: archive.Event.Key, Input: in, Archived: *archive.Fork, Previous: p}
			switch scenario {
			case "revision":
				r.Previous.Revision--
			case "evidence":
				r.Input.EvidenceVersion = strings.Repeat("e", 64)
			case "audit conflict":
				// An existing receipt cannot be silently overwritten or
				// reinterpreted as a different recovery.
				if err := s.Store.PutMeta(t.Context(), "fork-proof-recovery:"+r.ID, "invalid receipt"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Store.RestoreHistoricalForkProof(t.Context(), r); err == nil || (scenario != "audit conflict" && !errors.Is(err, ErrStaleWork)) {
				t.Fatal("transaction fence failed", err)
			}
			after, _ := s.Store.ForkProgress(t.Context(), archive.Event.Key)
			if !reflect.DeepEqual(p, after) {
				t.Fatal("failed transaction wrote proof")
			}
		})
	}
}

func TestProofRecoveryConfiguration(t *testing.T) {
	s, _, _, _, _ := proofRecoveryFixture(t)
	valid := s.Config.ForkProofRecoveries[0]
	for _, scenario := range []string{"valid", "unknown fork", "duplicate", "relative path", "missing reason", "bad hash", "bad merge"} {
		t.Run(scenario, func(t *testing.T) {
			a := valid
			switch scenario {
			case "unknown fork":
				a.ForkID = "unknown"
			case "relative path":
				a.ProofFile = "proof.json"
			case "missing reason":
				a.Reason = " "
			case "bad hash":
				a.ProofSHA256 = "123"
			case "bad merge":
				a.MergeCommit = "123"
			}
			s.Config.ForkProofRecoveries = []ForkProofRecovery{a}
			if scenario == "duplicate" {
				s.Config.ForkProofRecoveries = append(s.Config.ForkProofRecoveries, a)
			}
			if err := s.Config.validateForks(); (err == nil) != (scenario == "valid") {
				t.Fatal(err)
			}
		})
	}
}

func TestProofArchiveSizeBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.json")
	if err := os.WriteFile(path, []byte(`{"proof":"too big"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProofFile(path, 4); err == nil {
		t.Fatal("oversize archive accepted")
	}
}

func TestRecoveryAuditFailureRollsBackProof(t *testing.T) {
	s, _, _, in, _ := proofRecoveryFixture(t)
	store := s.Store.(*Store)
	before, err := s.Store.ForkProgress(t.Context(), s.Config.Forks[0].Key())
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("recovery audit insert failed")
	callbacks := store.db.Session(t.Context()).Callback().Create()
	if err = callbacks.Before("gorm:create").Register("test:fail_recovery_audit", func(tx *gorm.DB) {
		if row, ok := tx.Statement.Dest.(*metaRecord); ok && strings.HasPrefix(row.Name, "fork-proof-recovery:") {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { callbacks.Remove("test:fail_recovery_audit") })
	if _, err = s.RecoverForkProof(t.Context(), in); !errors.Is(err, failure) {
		t.Fatal("audit failure was not returned", err)
	}
	after, err := s.Store.ForkProgress(t.Context(), s.Config.Forks[0].Key())
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("audit failure left an unaudited proof", after, err)
	}
}
