package app

import (
	"context"
	"errors"
)

// An operator-owned exception for one reviewed conflict-resolution merge.
// Original upstream commits remain unchanged as its second-parent history.
// This grants no GitHub action, force push, test waiver or merge permission.
type SyncMergeResolution struct {
	Upstream string `json:"upstream"`
	Base     string `json:"base"`
	Head     string `json:"head"`
	Tree     string `json:"tree"`
}

func (r SyncMergeResolution) valid() bool {
	return shaRE.MatchString(r.Upstream) && shaRE.MatchString(r.Base) && shaRE.MatchString(r.Head) && shaRE.MatchString(r.Tree) && r.Head != r.Upstream && r.Head != r.Base && r.Base != r.Upstream
}

func (r *SyncMergeResolution) matches(upstream, base, head, tree string) bool {
	return r != nil && r.valid() && r.Upstream == upstream && r.Base == base && r.Head == head && r.Tree == tree
}

// Read the commit object rather than accepting ancestry alone: extra commits,
// reversed parents and a different resolved tree are outside the exact grant.
func verifyMergeResolution(ctx context.Context, path string, r *SyncMergeResolution, upstream, base, head string) error {
	if r == nil || !r.matches(upstream, base, head, r.Tree) {
		return errors.New("source must be the pinned upstream commit or an exact operator sync_merge_resolution grant for the current target")
	}
	commit, err := gitLocal(ctx, path, "show", "-s", "--format=%P%n%T", head)
	if err != nil || commit != base+" "+upstream+"\n"+r.Tree {
		return errors.New("resolution commit must have exactly the authorized target/upstream parents and tree")
	}
	return nil
}

const mergeResolutionInstruction = `The operator's sync_merge_resolution is an exception to the exact-upstream-head rule only for its pinned upstream, current base, head and tree. It permits one reviewed merge commit with exactly [base, upstream] parents; never rewrite upstream commits or append other edits. prepare-worktree selects that commit for a new workspace when the source and target match. Existing workspaces are preserved: after reading their state, explicitly check out the granted head detached in a clean workspace. Test the prospective merge using the configured commands. A changed target, grant or resolved commit requires a fresh decision and validation. The grant does not waive tests, authorize GitHub writes or grant force-push or merge permission. Completion still verifies the remote merge parents and tested tree, and advances the source cursor to the original upstream SHA.`
