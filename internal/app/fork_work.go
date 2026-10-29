package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

func requireForkContext(w WorkContext, in WorkInput) error {
	if w.Fork == nil || !idRE.MatchString(w.Fork.Round) || w.Fork.Phase == "restart" || forkFacts(w)["round"] != w.Fork.Round || w.Guide.Mode != "local-edit" || w.Task == nil || w.Task.TaskID != in.TaskID || w.Task.EvidenceVersion != in.EvidenceVersion || w.Task.GuideVersion != in.GuideVersion {
		return ErrWorkDenied
	}
	return nil
}

func requireForkWork(w WorkContext, in WorkInput) error {
	if err := requireForkContext(w, in); err != nil {
		return err
	}
	if w.Fork.Phase == "synced" {
		return ErrWorkDenied
	}
	if w.Fork.SyncMode != w.Guide.ForkSyncMode {
		return errors.New("sync mode changed; select an authorized batch before modifying work")
	}
	return nil
}

func (s *Service) checkForkAction(ctx context.Context, w WorkContext, in GitHubActionInput) (GitHubActionCheck, error) {
	var result GitHubActionCheck
	if err := requireForkWork(w, in.WorkInput); err != nil {
		return result, err
	}
	f, _ := s.Config.Fork(w.Event.Key)
	p, facts := w.Fork, forkFacts(w)
	var snapshot Snapshot
	_ = json.Unmarshal(w.Snapshot, &snapshot)
	pull := snapshot.Objects["pull"]
	if !shaRE.MatchString(in.Head) || in.Base != str(facts["target_sha"]) {
		return result, ErrStaleWork
	}
	if in.Action == "push" || in.Action == "open-pr" || in.Action == "merge" || (p.SyncMode == adaptedBatches && in.Action == "edit-pr") {
		if p.ValidatedHead != in.Head || p.ValidatedGuide != in.GuideVersion || p.ValidatedBase != in.Base || !validForkProofHead(*p) || !shaRE.MatchString(p.ValidatedTree) {
			return result, errors.New("run test-worktree for this commit, target and guide before writing")
		}
		local, err := s.verifyForkWorkspace(ctx, w, in.WorkInput)
		if err != nil || local.Head != in.Head {
			return result, errors.New("validated local commit is no longer current")
		}
		if err = cleanCommitted(ctx, local.Path); err != nil {
			return result, err
		}
		if p.SyncMode == adaptedBatches {
			mapping, section, err := validateForkMapping(ctx, f, p, local)
			if err != nil || mapping != p.ValidatedMapping || section != p.ValidatedPRSection {
				return result, errors.New("upstream mapping differs from the tested provenance; run test-worktree again")
			}
		}
	}
	if p.SyncMode == adaptedBatches {
		body := in.Body
		if in.Action == "merge" {
			body = str(snapshot.Objects["issue"]["body"])
		}
		if in.Action == "open-pr" || in.Action == "edit-pr" || in.Action == "merge" {
			if err := checkProvenanceBody(body, p.ValidatedPRSection); err != nil {
				return result, err
			}
		}
	}
	replacementHead := ""
	wantRef := p.Branch
	if in.Action == "merge" {
		wantRef = f.Target.Branch
	}
	if in.Ref != wantRef {
		return result, errors.New("write reference is outside the maintenance rule")
	}
	switch in.Action {
	case "push":
		if pull != nil && (pull["state"] != "open" || pull["merged"] == true) {
			return result, errors.New("sync PR is no longer open")
		}
		if remote := str(facts["remote_head"]); remote != "" {
			local, err := s.verifyForkWorkspace(ctx, w, in.WorkInput)
			if err != nil {
				return result, err
			}
			if _, err = gitLocal(ctx, local.Path, "merge-base", "--is-ancestor", remote, in.Head); err != nil {
				r := w.Guide.SyncBranchReplacement
				if r == nil || r.Branch != p.Branch || r.From != remote || r.To != in.Head {
					return result, errors.New("existing remote branch is not an ancestor of pinned upstream; request an exact branch replacement or restart decision")
				}
				replacementHead = remote
			}
		}
	case "open-pr":
		if p.PR != 0 || str(facts["remote_head"]) != in.Head {
			return result, errors.New("push the validated commit first; reuse an existing sync PR")
		}
	case "edit-pr":
		if p.PR == 0 || pull["state"] != "open" || pull["merged"] != false || pull["head"] != in.Head || pull["base_sha"] != in.Base || str(facts["remote_head"]) != in.Head || pull["head_ref"] != p.Branch || pull["base"] != f.Target.Branch || !strings.EqualFold(str(pull["head_repo"]), f.Target.Repo) {
			return result, errors.New("metadata updates require the current open sync PR and its exact branches and revisions")
		}
		result = prMetadataCheck(in, f.Target.Repo, p.PR)
		result.Branch, result.TargetBranch = p.Branch, f.Target.Branch
		return result, nil
	case "merge":
		if p.PR == 0 || pull["state"] != "open" || pull["draft"] != false || pull["merged"] == true || pull["head"] != in.Head || pull["base_sha"] != in.Base {
			return result, errors.New("merge requires the current tested open non-draft sync PR")
		}
	case "comment", "review", "close":
		if p.PR == 0 || pull["head"] != in.Head {
			return result, ErrStaleWork
		}
	default:
		return result, ErrWorkDenied
	}
	mergeMethod := "a merge commit"
	if p.SyncMode == adaptedBatches {
		mergeMethod = "squash"
	}
	return GitHubActionCheck{ExpectedRemoteHead: replacementHead, GitHubActionInput: in, Repository: f.Target.Repo, Number: p.PR, Permitted: true, Branch: p.Branch, TargetBranch: f.Target.Branch, Verification: "Current owner, delegation, branch and revisions checked. Only normal fast-forward pushes are permitted unless expected_remote_head is returned under an exact operator replacement grant; then use --force-with-lease=refs/heads/<branch>:<expected_remote_head>, never an unconditional force push; merge this PR using " + mergeMethod + " and expected head, without bypassing repository rules. Inspect current CI and reviews. This check performs no GitHub write."}, nil
}

func (s *Service) recordForkResult(ctx context.Context, w WorkContext, in WorkResult) error {
	if err := requireForkContext(w, in.WorkInput); err != nil {
		return err
	}
	p := *w.Fork
	if p.Phase == "synced" {
		if in.Outcome != "merged" || p.LastSuccess != p.Upstream || p.SyncMode != w.Guide.ForkSyncMode {
			return ErrWorkDenied
		}
		if err := s.verifyCompletedForkEvidence(ctx, w, in.WorkInput); err != nil {
			return err
		}
	} else if in.Outcome == "merged" || in.Outcome == "changes-ready" {
		if err := requireForkWork(w, in.WorkInput); err != nil {
			return err
		}
	}
	if forkFacts(w)["round"] != p.Round {
		return ErrStaleWork
	}
	var snapshot Snapshot
	_ = json.Unmarshal(w.Snapshot, &snapshot)
	if p.SyncMode == adaptedBatches && (in.Outcome == "merged" || in.Outcome == "changes-ready") {
		// Already completed historical rounds keep their original proof. No
		// unfinished round may use that exception to bypass the new contract.
		historical := p.Phase == "synced" && p.ValidatedMapping == "" && p.ValidatedPRSection == ""
		if !historical {
			if !idRE.MatchString(p.ValidatedMapping) {
				return errors.New("adapted results require tested per-commit provenance")
			}
			if err := checkProvenanceBody(str(snapshot.Objects["issue"]["body"]), p.ValidatedPRSection); err != nil {
				return err
			}
		}
	}
	switch in.Outcome {
	case "merged":
		pull, facts := snapshot.Objects["pull"], forkFacts(w)
		if pull["merged"] != true || !validForkProofHead(p) || !shaRE.MatchString(p.ValidatedTree) || pull["head"] != p.ValidatedHead || !s.forkCompletionGuideMatches(w) || (p.SyncMode != adaptedBatches && (facts["contains_pinned"] != true || facts["contains_validated"] != true)) || facts["merge_verified"] != true {
			return errors.New("sync completion requires the tested head and merge tree in the remote target, plus refreshed merged PR evidence")
		}
		p.LastSuccess, p.Phase, p.NextAttempt = p.Upstream, "synced", 0
	case "changes-ready":
		if !validForkProofHead(p) || !shaRE.MatchString(p.ValidatedTree) || p.ValidatedGuide != in.GuideVersion || p.ValidatedBase != str(forkFacts(w)["target_sha"]) {
			return errors.New("changes-ready requires current fork regression evidence")
		}
		if p.SyncMode == adaptedBatches && (p.PR == 0 || snapshot.Objects["pull"]["head"] != p.ValidatedHead || snapshot.Objects["pull"]["state"] != "open") {
			return errors.New("adapted changes-ready requires the tested head on the current open PR")
		}
		p.Phase, p.NextAttempt = "waiting", time.Now().Add(time.Hour).Unix()
	case "waiting":
		p.Phase, p.NextAttempt = "waiting", time.Now().Add(15*time.Minute).Unix()
	case "needs-decision":
		p.Phase, p.NextAttempt = "needs-decision", 0
	}
	return s.Store.SaveForkProgress(ctx, w.Event.Key, w.Fork.Revision, p, &in)
}

// Completed historical rounds retain their original generated instructions.
// Accept only exact known versions with today's policy otherwise unchanged;
// recordForkResult separately enforces provenance for unfinished rounds.
// Every write still needs the current guide.
func (s *Service) forkCompletionGuideMatches(w WorkContext) bool {
	if w.Fork.ValidatedGuide == w.Guide.Version {
		return true
	}
	if w.Fork.SyncMode != adaptedBatches {
		return false
	}
	d, err := s.Config.WorkDefinition(w.Event.Key)
	if err != nil {
		return false
	}
	for _, instruction := range []string{adaptedMergeInstruction, legacyAdaptedMergeInstruction} {
		previous, err := d.guide(s.Config, instruction)
		if err == nil && w.Fork.ValidatedGuide == previous.Version {
			return true
		}
	}
	return false
}

// Completed snapshots deliberately retain historical verification metadata
// across collector upgrades. Reconfirming a result must independently fetch and
// match today's actual merge proof, never turn that retained evidence into proof.
func (s *Service) verifyCompletedForkEvidence(ctx context.Context, w WorkContext, in WorkInput) error {
	g, ok := s.GH.(ForkSource)
	if !ok {
		return ErrWorkDenied
	}
	f, ok := s.Config.Fork(w.Event.Key)
	if !ok {
		return ErrWorkDenied
	}
	o, err := g.CollectFork(ctx, f, *w.Fork)
	if err != nil {
		return err
	}
	var fresh Snapshot
	if err = json.Unmarshal(forkSnapshot(f, *w.Fork, o), &fresh); err != nil {
		return err
	}
	// The readiness flag is derived from progress, not new GitHub evidence.
	if ready, exists := forkFacts(w)["next_batch_ready"]; exists {
		fresh.Objects["fork"]["next_batch_ready"] = ready
	}
	if hash(canonical(fresh)) != w.CurrentVersion {
		return ErrStaleWork
	}
	_, err = s.CheckWork(ctx, in)
	return err
}

// Restart after a closed PR, including explicit retirement of an already
// merged legacy integration head. Never record that legacy proof as success.
func (s *Service) RestartFork(ctx context.Context, in WorkInput) (ForkProgress, error) {
	if _, err := s.RefreshWorkContext(ctx, in.EventID); err != nil {
		return ForkProgress{}, err
	}
	w, err := s.CheckWork(ctx, in)
	if err != nil {
		return ForkProgress{}, err
	}
	if err = requireForkWork(w, in); err != nil {
		return ForkProgress{}, err
	}
	var snapshot Snapshot
	_ = json.Unmarshal(w.Snapshot, &snapshot)
	pull := snapshot.Objects["pull"]
	legacyMerged := w.Fork.SyncMode == "" && pull["merged"] == true && shaRE.MatchString(str(pull["head"])) && pull["head"] != w.Fork.Upstream && forkFacts(w)["contains_pinned"] == true
	if pull["state"] != "closed" || (pull["merged"] != false && !legacyMerged) {
		return ForkProgress{}, errors.New("restart requires a closed unmerged PR or explicit retirement of a merged legacy integration head already containing pinned upstream")
	}
	p := *w.Fork
	p.Phase, p.NextAttempt = "restart", 0
	err = s.Store.SaveForkProgress(ctx, w.Event.Key, p.Revision, p, nil)
	return p, err
}
