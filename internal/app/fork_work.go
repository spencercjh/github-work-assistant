package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

func requireForkWork(w WorkContext, in WorkInput) error {
	if w.Fork == nil || !idRE.MatchString(w.Fork.Round) || w.Fork.Phase == "synced" || w.Fork.Phase == "restart" || forkFacts(w)["round"] != w.Fork.Round || w.Guide.Mode != "local-edit" || w.Task == nil || w.Task.TaskID != in.TaskID || w.Task.EvidenceVersion != in.EvidenceVersion || w.Task.GuideVersion != in.GuideVersion {
		return ErrWorkDenied
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
	if in.Action == "push" || in.Action == "open-pr" || in.Action == "merge" {
		if p.ValidatedHead != in.Head || p.ValidatedGuide != in.GuideVersion || p.ValidatedBase != in.Base || p.ValidatedHead != p.Upstream || !shaRE.MatchString(p.ValidatedTree) {
			return result, errors.New("run test-worktree for this commit, target and guide before writing")
		}
		local, err := s.verifyForkWorkspace(ctx, w, in.WorkInput)
		if err != nil || local.Head != in.Head {
			return result, errors.New("validated local commit is no longer current")
		}
		if err = cleanCommitted(ctx, local.Path); err != nil {
			return result, err
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
	return GitHubActionCheck{ExpectedRemoteHead: replacementHead, GitHubActionInput: in, Repository: f.Target.Repo, Number: p.PR, Permitted: true, Branch: p.Branch, TargetBranch: f.Target.Branch, Verification: "Current owner, delegation, branch and revisions checked. Only normal fast-forward pushes are permitted unless expected_remote_head is returned under an exact operator replacement grant; then use --force-with-lease=refs/heads/<branch>:<expected_remote_head>, never an unconditional force push; merge this PR using a merge commit and expected head, without bypassing repository rules. Inspect current CI and reviews. This check performs no GitHub write."}, nil
}

func (s *Service) recordForkResult(ctx context.Context, w WorkContext, in WorkResult) error {
	if err := requireForkWork(w, in.WorkInput); err != nil {
		return err
	}
	p := *w.Fork
	if forkFacts(w)["round"] != p.Round {
		return ErrStaleWork
	}
	var snapshot Snapshot
	_ = json.Unmarshal(w.Snapshot, &snapshot)
	switch in.Outcome {
	case "merged":
		pull, facts := snapshot.Objects["pull"], forkFacts(w)
		if pull["merged"] != true || p.ValidatedHead != p.Upstream || !shaRE.MatchString(p.ValidatedTree) || pull["head"] != p.ValidatedHead || p.ValidatedGuide != in.GuideVersion || facts["contains_pinned"] != true || facts["contains_validated"] != true || facts["merge_verified"] != true {
			return errors.New("sync completion requires the tested commit and pinned upstream in the remote target, plus refreshed merged PR evidence")
		}
		p.LastSuccess, p.Phase, p.NextAttempt = p.Upstream, "synced", 0
	case "changes-ready":
		if p.ValidatedHead != p.Upstream || !shaRE.MatchString(p.ValidatedTree) || p.ValidatedGuide != in.GuideVersion || p.ValidatedBase != str(forkFacts(w)["target_sha"]) {
			return errors.New("changes-ready requires current fork regression evidence")
		}
		p.Phase, p.NextAttempt = "waiting", time.Now().Add(time.Hour).Unix()
	case "waiting":
		p.Phase, p.NextAttempt = "waiting", time.Now().Add(15*time.Minute).Unix()
	case "needs-decision":
		p.Phase, p.NextAttempt = "needs-decision", 0
	}
	return s.Store.SaveForkProgress(ctx, w.Event.Key, w.Fork.Revision, p, &in)
}

// Restart only after a prior PR was deliberately closed without merging. This
// never force-pushes, deletes a workspace or treats an abandoned round as success.
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
	if pull["state"] != "closed" || pull["merged"] != false {
		return ForkProgress{}, errors.New("restart requires a closed unmerged sync PR")
	}
	p := *w.Fork
	p.Phase = "restart"
	err = s.Store.SaveForkProgress(ctx, w.Event.Key, p.Revision, p, nil)
	return p, err
}
