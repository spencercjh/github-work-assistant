package app

import (
	"context"
	"errors"
	"testing"
)

type queueValidationSource struct {
	*queueSource
	branch string
	extra  Objects
}

func (q *queueValidationSource) CollectFork(ctx context.Context, f ForkSpec, p ForkProgress) (ForkObservation, error) {
	o, err := q.queueSource.CollectFork(ctx, f, p)
	if p.Branch == q.branch {
		for key, value := range q.extra {
			o.Objects[key] = value
		}
	}
	return o, err
}

func TestQueueValidationWhileEarlierPRReports(t *testing.T) {
	for _, scenario := range []string{"report", "ci", "report-and-ci", "review", "changes-requested", "failed-ci", "parent-head", "parent-closed", "target", "unknown-evidence"} {
		t.Run(scenario, func(t *testing.T) {
			s, q, _, after := newQueueFixtureMode(t, false)
			s.Config.Forks[0].Channel = "#github-work"
			api := newProjectAPI(t, s)
			appendQueue(t, s, after)
			first := publishQueue(t, s, q)
			source := &queueValidationSource{queueSource: q, branch: first.Branch}
			s.GH = source
			one := prDiscussionItem(t, s, first)
			state := api.tasks[one.Task.MessageID]
			state.Owner = "reviewer"
			api.tasks[one.Task.MessageID] = state
			second := appendQueue(t, s, "")
			in, w := queueInput(t, s)
			if _, err := s.PrepareWorkspace(t.Context(), in, q.repository); err != nil {
				t.Fatal(err)
			}
			store := s.Store.(*Store)
			s.Store = &expiredTestEvidenceStore{Persistence: store, store: store, afterInvalidation: func() {
				current, _ := queueInput(t, s)
				if scenario == "report" || scenario == "report-and-ci" {
					reviewer := participantInput(t, s, current.EventID, one.Task.MessageID, "@reviewer")
					api.reportTask, api.reportAuthor = one.Task.MessageID, "reviewer"
					if err := s.RecordContribution(t.Context(), ContributionInput{WorkInput: reviewer, ReportID: projectReport, Outcome: "investigated", Summary: "Earlier PR investigation while the next batch tests.", Head: first.ValidatedHead, Base: first.ValidatedBase}); err != nil {
						t.Fatal(err)
					}
				}
				switch scenario {
				case "ci", "report-and-ci":
					q.prs[first.Branch].check = "pending"
				case "review":
					source.extra = Objects{"comment:late": {"body": "Follow-up on the earlier PR."}, "review:late": {"state": "COMMENTED"}}
				case "changes-requested":
					source.extra = Objects{"review:late": {"state": "CHANGES_REQUESTED"}}
				case "failed-ci":
					q.prs[first.Branch].check = "failure"
				case "parent-head":
					q.prs[first.Branch].head = q.upstream
				case "parent-closed":
					q.prs[first.Branch].closed = true
				case "target":
					tree := fixtureGit(t, q.repository, "", "rev-parse", q.target+"^{tree}")
					q.target = syntheticCommit(t, q.repository, tree, q.target)
				case "unknown-evidence":
					source.extra = Objects{"policy:new": {"required": "additional validation"}}
				}
			}}
			proof, err := s.TestWorkspace(t.Context(), in)
			p, readErr := store.ForkProgress(t.Context(), w.Event.Key)
			if readErr != nil {
				t.Fatal(readErr)
			}
			allowed := scenario == "report" || scenario == "ci" || scenario == "report-and-ci" || scenario == "review"
			if !allowed {
				if err == nil || proof.Passed || p.ValidatedHead != "" {
					t.Fatal("changed dependency was accepted", proof, err)
				}
				return
			}
			if err != nil || !proof.Passed || p.BatchID != second.BatchID {
				t.Fatalf("earlier PR activity discarded current validation: passed=%v batch=%s error=%v", proof.Passed, p.BatchID, err)
			}
			if scenario != "report" {
				if _, err := s.CheckWork(t.Context(), in); !errors.Is(err, ErrStaleWork) {
					t.Fatal("saving tests authorized a write on old feedback", err)
				}
			}
			fresh, current := queueInput(t, s)
			if _, err := s.CheckGitHubAction(t.Context(), GitHubActionInput{WorkInput: fresh, Action: "push", Ref: current.Fork.Branch, Head: proof.Head, Base: proof.Base}); err != nil {
				t.Fatal("fresh context could not use the saved proof", err)
			}
		})
	}
}

func TestQueueTestProjectionKeepsOwnFeedbackAndSourceContext(t *testing.T) {
	selected := ForkProgress{BatchID: "selected"}
	sibling := ForkQueuedBatch{Progress: ForkProgress{BatchID: "sibling"}, Observation: ForkObservation{Objects: Objects{
		"pull":     {"mergeable": true, "conflict_status": "clean", "mergeable_state": "unstable"},
		"check:ci": {"status": "pending"},
	}}}
	selected.Queue = &ForkQueueState{Batches: []ForkQueuedBatch{sibling}}
	objects := Objects{"fork": {"wake": 0, "batch_id": selected.BatchID}, "comment:own": {"body": "Current PR feedback"}}
	queueSnapshot(selected, objects)
	w := WorkContext{Fork: &selected, Snapshot: canonical(Snapshot{Objects: objects})}
	before := string(canonical(w))
	projection := forkTestSnapshot(w)
	if string(canonical(w)) != before {
		t.Fatal("test projection changed the full work context")
	}
	selected.Queue.Batches[0].Observation.Objects["comment:concurrent"] = map[string]any{"body": "A newer observation"}
	if string(forkTestSnapshot(w)) != string(w.Snapshot) {
		t.Fatal("mixed snapshot and observation were projected")
	}
	delete(selected.Queue.Batches[0].Observation.Objects, "comment:concurrent")
	sibling.Observation.Objects["pull"]["mergeable_state"] = "clean"
	selected.Queue.Batches[0] = sibling
	queueSnapshot(selected, objects)
	w.Snapshot = canonical(Snapshot{Objects: objects})
	if !forkTestEvidenceMatches(projection, forkTestSnapshot(w)) {
		t.Fatal("successful dependency CI invalidated the test tree")
	}
	objects["comment:own"]["body"] = "Unread current PR feedback"
	w.Snapshot = canonical(Snapshot{Objects: objects})
	if forkTestEvidenceMatches(projection, forkTestSnapshot(w)) {
		t.Fatal("current PR feedback was hidden")
	}
}
