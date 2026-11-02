package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func forkRetryFixture(t *testing.T, mode string) (*Service, *forkFixtureSource, *taskAPI, WorkInput, WorkContext, Workspace) {
	t.Helper()
	if mode == "adapted" {
		return beginAdaptedFixture(t)
	}
	s, g, api := forkFixture(t)
	in, w := currentForkInput(t, s)
	ws, err := s.PrepareWorkspace(t.Context(), in, g.repository)
	if err != nil {
		t.Fatal(err)
	}
	return s, g, api, in, w, ws
}

func expireForkRetry(t *testing.T, store *Store, key string) {
	t.Helper()
	p, err := store.ForkProgress(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	p.NextAttempt = time.Now().Add(-time.Minute).Unix()
	if err = store.SaveForkProgress(t.Context(), key, p.Revision, p, nil); err != nil {
		t.Fatal(err)
	}
}

func TestForkValidationSurvivesRetryWake(t *testing.T) {
	for _, mode := range []string{"original", "adapted"} {
		t.Run(mode, func(t *testing.T) {
			s, g, _, in, w, ws := forkRetryFixture(t, mode)
			if err := s.RecordWorkResult(t.Context(), WorkResult{WorkInput: in, ReportID: fixtureReport, Outcome: "waiting", Summary: "Waiting for validation prerequisites"}); err != nil {
				t.Fatal(err)
			}
			store := s.Store.(*Store)
			s.Store = &expiredTestEvidenceStore{Persistence: store, store: store, afterInvalidation: func() {
				// The final refresh exercises the real retry planner while the
				// actual fixture regression commands still run normally.
				expireForkRetry(t, store, w.Event.Key)
			}}
			proof, err := s.TestWorkspace(t.Context(), in)
			if err != nil || !proof.Passed {
				t.Fatal("retry wake discarded passing validation", proof, err)
			}
			p, err := store.ForkProgress(t.Context(), w.Event.Key)
			if err != nil || p.Wake != w.Fork.Wake+1 || p.Retries != w.Fork.Retries+1 || p.Phase != "working" || p.NextAttempt <= time.Now().Unix() || p.ValidatedHead != ws.Head || p.ValidatedBase != g.target || p.ValidatedTree != proof.Tree || p.TestLogHash != proof.LogHash {
				t.Fatal("proof or retry progress lost", p, err)
			}
			// Saving a local test result does not authorize writes with the
			// stale event or silently rebind the existing task to the new one.
			if _, err = s.CheckWork(t.Context(), in); !errors.Is(err, ErrStaleWork) {
				t.Fatal("old event unexpectedly authorized", err)
			}
			check := GitHubActionInput{WorkInput: in, Action: "push", Head: ws.Head, Base: g.target, Ref: w.Fork.Branch}
			if _, err = s.CheckGitHubAction(t.Context(), check); !errors.Is(err, ErrStaleWork) {
				t.Fatal("stale write unexpectedly authorized", err)
			}
			check.WorkInput, _ = currentForkInput(t, s)
			if _, err = s.CheckGitHubAction(t.Context(), check); err != nil {
				t.Fatal("freshly linked event could not use saved proof", err)
			}
		})
	}
}

func TestForkRetryWakeDoesNotHideChangedValidationInputs(t *testing.T) {
	for _, scenario := range []string{"target", "source", "guide", "owner", "status", "access", "PR", "round"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, api, in, w, ws := forkRetryFixture(t, "adapted")
			store := s.Store.(*Store)
			s.Store = &expiredTestEvidenceStore{Persistence: store, store: store, afterInvalidation: func() {
				expireForkRetry(t, store, w.Event.Key)
				switch scenario {
				case "target":
					tree := fixtureGit(t, g.repository, "", "rev-parse", g.target+"^{tree}")
					g.target = syntheticCommit(t, g.repository, tree, g.target)
				case "source":
					tree := fixtureGit(t, ws.Path, "", "rev-parse", "HEAD^{tree}")
					fixtureGit(t, ws.Path, "", "reset", "--hard", syntheticCommit(t, g.repository, tree, ws.Head))
				case "guide":
					s.Config.Repositories[0].Goal += " changed"
				case "owner":
					api.owner = "successor"
				case "status":
					api.status = "in_review"
				case "access":
					g.denied = true
				case "PR":
					g.pr, g.remoteHead, g.prBranch = 1, ws.Head, w.Fork.Branch
				case "round":
					p, err := store.ForkProgress(t.Context(), w.Event.Key)
					if err != nil {
						t.Fatal(err)
					}
					p.Phase = "restart"
					if err = store.SaveForkProgress(t.Context(), w.Event.Key, p.Revision, p, nil); err != nil {
						t.Fatal(err)
					}
				}
			}}
			proof, err := s.TestWorkspace(t.Context(), in)
			p, readErr := store.ForkProgress(t.Context(), w.Event.Key)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if err == nil || proof.Passed || p.ValidatedHead != "" {
				t.Fatal("retry wake hid changed input or authority", proof, p, err)
			}
		})
	}
}

type changedProofSnapshotStore struct {
	Persistence
	store *Store
}

func (s *changedProofSnapshotStore) SaveForkTestProof(ctx context.Context, key string, revision int64, p ForkProgress, version string) error {
	body, _, err := s.store.Snapshot(ctx, key)
	if err != nil {
		return err
	}
	var snap Snapshot
	if err = json.Unmarshal(body, &snap); err != nil {
		return err
	}
	snap.Objects["comment:late"] = map[string]any{"body": "New review during proof persistence"}
	if _, err = s.store.Observe(ctx, key, canonical(snap)); err != nil {
		return err
	}
	// Progress is unchanged; the atomic evidence check must detect this write.
	return s.Persistence.SaveForkTestProof(ctx, key, revision, p, version)
}

func TestForkProofRejectsSnapshotChangeDuringSave(t *testing.T) {
	s, _, _, in, w, _ := forkRetryFixture(t, "original")
	store := s.Store.(*Store)
	s.Store = &changedProofSnapshotStore{Persistence: store, store: store}
	proof, err := s.TestWorkspace(t.Context(), in)
	if !errors.Is(err, ErrStaleWork) || proof.Passed {
		t.Fatal("concurrent evidence change accepted", proof, err)
	}
	p, err := store.ForkProgress(t.Context(), w.Event.Key)
	if err != nil || p.ValidatedHead != "" {
		t.Fatal("stale proof saved", p, err)
	}
}
