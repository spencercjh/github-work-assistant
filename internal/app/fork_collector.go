package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var compareReadRE = regexp.MustCompile(`^repos/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/compare/[a-f0-9]{40}\.\.\.[a-f0-9]{40}\?per_page=1$`)
var refReadRE = regexp.MustCompile(`^repos/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/git/(?:ref|matching-refs)/heads/(.+)$`)
var commitReadRE = regexp.MustCompile(`^repos/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/git/commits/[a-f0-9]{40}$`)
var pullsReadRE = regexp.MustCompile(`^repos/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pulls\?[^\s#]*$`)

func validForkReadPath(path string) bool {
	if (compareReadRE.MatchString(path) && !strings.Contains(strings.SplitN(path, "/compare/", 2)[0], "..")) || (commitReadRE.MatchString(path) && !strings.Contains(path, "..")) {
		return true
	}
	if m := refReadRE.FindStringSubmatch(path); m != nil {
		branch, err := url.PathUnescape(m[1])
		return err == nil && validBranch(branch) && !strings.Contains(path, "..")
	}
	return pullsReadRE.MatchString(path) && !strings.Contains(path, "..")
}

func (g GH) branchHead(ctx context.Context, ref BranchRef) (string, error) {
	row, err := g.get(ctx, "repos/"+ref.Repo+"/git/ref/heads/"+url.PathEscape(ref.Branch))
	if err != nil {
		return "", err
	}
	sha := str(obj(row["object"])["sha"])
	if row["ref"] != "refs/heads/"+ref.Branch || obj(row["object"])["type"] != "commit" || !shaRE.MatchString(sha) {
		return "", errors.New("branch did not resolve to the exact requested commit reference")
	}
	return sha, nil
}

func (g GH) containsCommit(ctx context.Context, repo, ancestor, head string) (bool, error) {
	if ancestor == "" {
		return false, nil
	}
	if !shaRE.MatchString(ancestor) || !shaRE.MatchString(head) {
		return false, errors.New("invalid ancestry revisions")
	}
	if ancestor == head {
		return true, nil
	}
	row, err := g.get(ctx, "repos/"+repo+"/compare/"+ancestor+"..."+head+"?per_page=1")
	if err != nil {
		return false, err
	}
	switch row["status"] {
	case "ahead", "identical":
		return true, nil
	case "behind", "diverged":
		return false, nil
	default:
		return false, errors.New("unknown comparison status")
	}
}

func (g GH) CollectFork(ctx context.Context, f ForkSpec, p ForkProgress) (ForkObservation, error) {
	o := ForkObservation{Classification: "public", Objects: Objects{}}
	for _, repo := range []string{f.Upstream.Repo, f.Target.Repo} {
		classification, err := g.RepositoryClassification(ctx, repo)
		if err != nil {
			return o, err
		}
		if classification == "private" {
			o.Classification = "private"
			o.PrivateRepos = append(o.PrivateRepos, strings.ToLower(repo))
		}
	}
	var err error
	if o.Upstream, err = g.branchHead(ctx, f.Upstream); err != nil {
		return o, err
	}
	if o.Target, err = g.branchHead(ctx, f.Target); err != nil {
		return o, err
	}
	if o.ContainsLatest, err = g.containsCommit(ctx, f.Target.Repo, o.Upstream, o.Target); err != nil {
		return o, err
	}
	if o.ContainsPinned, err = g.containsCommit(ctx, f.Target.Repo, p.Upstream, o.Target); err != nil {
		return o, err
	}
	// The deterministic per-round branch recovers a PR created before a crash,
	// including a response lost before its number could be saved locally.
	if p.Round != "" && p.Branch != "" {
		refs, err := g.api(ctx, "repos/"+f.Target.Repo+"/git/matching-refs/heads/"+url.PathEscape(p.Branch), false)
		if err != nil {
			return o, err
		}
		var matches []map[string]any
		if json.Unmarshal(refs, &matches) != nil {
			return o, errors.New("invalid sync ref response")
		}
		for _, ref := range matches {
			if ref["ref"] == "refs/heads/"+p.Branch {
				o.SyncHead = str(obj(ref["object"])["sha"])
				if !shaRE.MatchString(o.SyncHead) || obj(ref["object"])["type"] != "commit" {
					return o, errors.New("invalid sync branch commit")
				}
			}
		}
		q := url.Values{"state": {"all"}, "head": {strings.Split(f.Target.Repo, "/")[0] + ":" + p.Branch}, "base": {f.Target.Branch}, "per_page": {"100"}}
		rows, err := g.pages(ctx, "repos/"+f.Target.Repo+"/pulls?"+q.Encode(), "")
		if err != nil {
			return o, err
		}
		if len(rows) > 1 {
			return o, errors.New("multiple PRs for the sync branch; reconcile before continuing")
		}
		if len(rows) == 1 {
			number, ok := rows[0]["number"].(float64)
			if !ok || number < 1 || number != float64(int(number)) {
				return o, errors.New("invalid sync PR number")
			}
			o.PR = int(number)
			if p.PR != 0 && p.PR != o.PR {
				return o, errors.New("sync PR identity changed")
			}
			body, err := g.Collect(ctx, Watch{f.Target.Repo, o.PR})
			if err != nil {
				return o, err
			}
			var snap Snapshot
			if json.Unmarshal(body, &snap) != nil {
				return o, errors.New("invalid sync PR snapshot")
			}
			pull := snap.Objects["pull"]
			if pull["head_ref"] != p.Branch || pull["base"] != f.Target.Branch || !strings.EqualFold(str(pull["head_repo"]), f.Target.Repo) {
				return o, errors.New("PR does not match the configured sync branch and target")
			}
			o.Objects = snap.Objects
		} else if p.PR != 0 {
			return o, errors.New("previous sync PR was not returned; do not replace it")
		}
	}
	// A validated commit may still exist only in the local worktree. Query it
	// remotely only after the corresponding PR was actually merged.
	pull := o.Objects["pull"]
	if pull["merged"] == true && p.ValidatedHead != "" && pull["head"] == p.ValidatedHead {
		o.MergeCommit = str(pull["merge_commit_sha"])
		if !shaRE.MatchString(o.MergeCommit) {
			return o, errors.New("merged PR has no valid merge commit")
		}
		mergeInTarget, err := g.containsCommit(ctx, f.Target.Repo, o.MergeCommit, o.Target)
		if err != nil {
			return o, err
		}
		testedInMerge, err := g.containsCommit(ctx, f.Target.Repo, p.ValidatedHead, o.MergeCommit)
		if err != nil {
			return o, err
		}
		upstreamInMerge, err := g.containsCommit(ctx, f.Target.Repo, p.Upstream, o.MergeCommit)
		if err != nil {
			return o, err
		}
		mergeCommit, err := g.get(ctx, "repos/"+f.Target.Repo+"/git/commits/"+o.MergeCommit)
		if err != nil {
			return o, err
		}
		testedCommit, err := g.get(ctx, "repos/"+f.Target.Repo+"/git/commits/"+p.ValidatedHead)
		if err != nil {
			return o, err
		}
		tree, testedTree := str(obj(mergeCommit["tree"])["sha"]), str(obj(testedCommit["tree"])["sha"])
		if !shaRE.MatchString(tree) || !shaRE.MatchString(testedTree) {
			return o, errors.New("invalid commit tree")
		}
		o.ContainsValidated = mergeInTarget && testedInMerge
		o.MergeVerified = mergeInTarget && testedInMerge && upstreamInMerge && tree == testedTree
	}
	// Pin all fetched facts to one target revision; CI/reviews are already
	// protected against moving PR heads by the ordinary PR collector.
	if target, err := g.branchHead(ctx, f.Target); err != nil || target != o.Target {
		if err != nil {
			return o, err
		}
		return o, fmt.Errorf("fork target changed during collection; retry")
	}
	return o, nil
}
