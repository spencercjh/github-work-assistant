package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"strings"
	"unicode"
)

// The manifest lives beside workspace metadata, not in the source branch:
// recording provenance must not require an unrelated bookkeeping commit.
type ForkCommitMapping struct {
	Upstream string `json:"upstream"`
	Commit   string `json:"commit,omitempty"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
}

const provenanceStart = "<!-- upstream-provenance:start -->"
const provenanceEnd = "<!-- upstream-provenance:end -->"
const adaptedProvenanceInstruction = "Every adapted source commit must correspond to exactly one listed upstream commit, in mainline order, with one full-SHA Upstream-Commit trailer. Fill the external mapping_file for every batch item (ported, already-present, or not-applicable, with evidence). Never combine upstream changes into one commit or add unrelated/empty commits. Keep multiple ordered commits in one appropriately sized PR and test the batch once. test-worktree verifies the mapping and generates pr_body_file; include its entire provenance section in the PR description. Pass the actual proposed body using check-github-action --body-file for open-pr/edit-pr; use the authorized GitHub client and create a draft unless explicitly authorized otherwise. Tests and human review still decide semantic correctness."

func prepareForkMapping(path string, p *ForkProgress) error {
	rows := make([]ForkCommitMapping, len(p.BatchCommits))
	for i, sha := range p.BatchCommits {
		rows[i].Upstream = sha
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil // Never overwrite the executor's inventory on retry.
	}
	if err != nil {
		return err
	}
	defer f.Close()
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(rows); err != nil {
		return err
	}
	return f.Sync()
}

func readForkMapping(path string) ([]ForkCommitMapping, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("fill the workspace mapping_file before testing adapted commits")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 128*1024+1))
	if err != nil || len(b) > 128*1024 {
		return nil, errors.New("upstream mapping exceeds 128 KiB or cannot be read")
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	var rows []ForkCommitMapping
	if err = decoder.Decode(&rows); err != nil {
		return nil, fmt.Errorf("invalid upstream mapping: %w", err)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("upstream mapping must contain one JSON array")
	}
	return rows, nil
}

func provenanceCell(s string) string {
	s = html.EscapeString(strings.Join(strings.Fields(s), " "))
	return strings.NewReplacer("\\", "&#92;", "|", "&#124;", "`", "&#96;", "[", "&#91;", "]", "&#93;", "*", "&#42;", "_", "&#95;", "@", "&#64;").Replace(s)
}

// This proves the declared one-to-one provenance and order, not that a human
// explanation of a port or skipped change is semantically correct.
func validateForkMapping(ctx context.Context, f ForkSpec, p *ForkProgress, ws Workspace) (string, string, error) {
	rows, err := readForkMapping(ws.MappingFile)
	if err != nil {
		return "", "", err
	}
	if len(rows) != len(p.BatchCommits) || len(rows) == 0 {
		return "", "", errors.New("mapping must account for every selected upstream commit")
	}
	// Linear parent chaining rejects merges, extra side history and unrelated
	// commits. Use all commits in the range, never only its first-parent view.
	text, err := gitLocal(ctx, ws.Path, "rev-list", "--reverse", "--topo-order", p.InitialTarget+".."+ws.Head)
	if err != nil {
		return "", "", err
	}
	commits := strings.Fields(text)
	previous, index := p.InitialTarget, 0
	var section strings.Builder
	section.WriteString(provenanceStart + "\n### Upstream commits\n\n")
	section.WriteString("Ordered oldest to newest along the upstream mainline. Skipped entries require review of the stated evidence.\n\n")
	section.WriteString("| Date | Upstream commit | Subject | Result | Adapted commit | Evidence / adaptation |\n| --- | --- | --- | --- | --- | --- |\n")
	for i, row := range rows {
		if row.Upstream != p.BatchCommits[i] || !shaRE.MatchString(row.Upstream) {
			return "", "", errors.New("mapping must retain the exact selected upstream order")
		}
		if strings.TrimSpace(row.Reason) == "" || len(row.Reason) > 1000 || strings.IndexFunc(row.Reason, unicode.IsControl) >= 0 {
			return "", "", errors.New("each mapping entry needs a single-line evidence or adaptation explanation (1..1000 bytes)")
		}
		mapped := "—"
		switch row.Status {
		case "ported":
			if index >= len(commits) || row.Commit != commits[index] || !shaRE.MatchString(row.Commit) {
				return "", "", errors.New("each adapted commit must map to exactly one upstream item in order")
			}
			parents, e := gitLocal(ctx, ws.Path, "show", "-s", "--format=%P", row.Commit)
			if e != nil || parents != previous {
				return "", "", errors.New("adapted commits must form a linear sequence from the initial target")
			}
			trailer, e := gitLocal(ctx, ws.Path, "show", "-s", "--format=%(trailers:key=Upstream-Commit,valueonly)", row.Commit)
			if e != nil || trailer != row.Upstream {
				return "", "", errors.New("each adapted commit needs exactly one Upstream-Commit trailer containing its full upstream SHA")
			}
			changed, e := gitLocal(ctx, ws.Path, "diff-tree", "--no-commit-id", "--name-only", "-r", previous, row.Commit)
			if e != nil || changed == "" {
				return "", "", errors.New("do not create empty commits for already-present or inapplicable changes")
			}
			mapped = fmt.Sprintf("[%s](https://github.com/%s/commit/%s)", row.Commit, f.Target.Repo, row.Commit)
			previous, index = row.Commit, index+1
		case "already-present", "not-applicable":
			if row.Commit != "" {
				return "", "", errors.New("skipped upstream entries must not name an adapted commit")
			}
		default:
			return "", "", errors.New("mapping status must be ported, already-present or not-applicable")
		}
		meta, e := gitLocal(ctx, ws.Path, "show", "-s", "--format=%cs%n%s", row.Upstream)
		parts := strings.SplitN(meta, "\n", 2)
		if e != nil || len(parts) != 2 {
			return "", "", errors.New("fetch every selected upstream commit before validation")
		}
		fmt.Fprintf(&section, "| %s | [%s](https://github.com/%s/commit/%s) | %s | %s | %s | %s |\n", provenanceCell(parts[0]), row.Upstream, f.Upstream.Repo, row.Upstream, provenanceCell(parts[1]), row.Status, mapped, provenanceCell(row.Reason))
	}
	if index != len(commits) || index == 0 || previous != ws.Head {
		return "", "", errors.New("all adapted commits must have a unique upstream mapping; no unrelated commits or empty batch")
	}
	section.WriteString(provenanceEnd)
	// Include the trailing newline written to pr_body_file. Reject an expanded
	// table before test-worktree can issue proof for a body the action rejects.
	if err := checkPRBodySize(section.String() + "\n"); err != nil {
		return "", "", err
	}
	return hash(canonical(rows)), section.String(), nil
}

func checkProvenanceBody(body, section string) error {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	if section == "" || strings.Count(body, provenanceStart) != 1 || strings.Count(body, provenanceEnd) != 1 || !strings.Contains(body, section) {
		return errors.New("PR body must include the complete tested upstream provenance section exactly once; use pr_body_file from test-worktree")
	}
	return nil
}
