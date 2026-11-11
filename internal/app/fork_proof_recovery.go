package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// A trusted operator must independently audit and pin the original work-context
// archive. A caller cannot supply a new "passed" proof or relax merge checks.
// This grants restoration only, never a merge or a completion cursor update.
type ForkProofRecovery struct {
	ForkID      string `json:"fork_id"`
	PR          int    `json:"pr"`
	MergeCommit string `json:"merge_commit"`
	ProofFile   string `json:"proof_file"`
	ProofSHA256 string `json:"proof_sha256"`
	Reason      string `json:"reason"`
}

func (c Config) validateForkProofRecoveries() error {
	seen := map[string]bool{}
	for _, a := range c.ForkProofRecoveries {
		f, ok := c.Fork("fork:" + a.ForkID)
		key := fmt.Sprintf("%s:%d", a.ForkID, a.PR)
		if !ok || a.PR < 1 || !shaRE.MatchString(a.MergeCommit) || !idRE.MatchString(a.ProofSHA256) || !filepath.IsAbs(a.ProofFile) || strings.ContainsAny(a.ProofFile, "\x00\r\n") || strings.TrimSpace(a.Reason) == "" || len(a.Reason) > 8000 || seen[key] {
			return errors.New("fork_proof_recoveries require a unique configured fork/PR, exact merge SHA, absolute proof_file, proof_sha256 and audit reason")
		}
		g, err := c.RepositoryGuide(f.Target.Repo)
		if err != nil {
			return err
		}
		if g.ForkSyncMode != adaptedBatches {
			return errors.New("fork_proof_recoveries apply only to already merged adapted batches")
		}
		seen[key] = true
	}
	return nil
}

type ForkProofRecoveryReceipt struct {
	ID          string            `json:"recovery_id"`
	Resource    string            `json:"resource_key"`
	Input       WorkInput         `json:"work"`
	Grant       ForkProofRecovery `json:"grant"`
	Archived    ForkProgress      `json:"archived_progress"`
	Previous    ForkProgress      `json:"previous_progress"`
	MergedAt    string            `json:"merged_at"`
	RecoveredAt int64             `json:"recovered_at"`
}

// Copy the proof, not the archived owner, round, cursor, phase or latest attempt.
// Validation still describes the last actual test; the receipt retains the
// historical passed attempt which produced the restored proof.
func restoreForkProof(current, archived ForkProgress) ForkProgress {
	current.ValidatedHead, current.ValidatedBase = archived.ValidatedHead, archived.ValidatedBase
	current.ValidatedTree, current.ValidatedGuide = archived.ValidatedTree, archived.ValidatedGuide
	current.ValidatedMapping, current.ValidatedPRSection = archived.ValidatedMapping, archived.ValidatedPRSection
	current.TestLog, current.TestLogHash = archived.TestLog, archived.TestLogHash
	return current
}

func sameForkProof(a, b ForkProgress) bool {
	return a.ValidatedHead == b.ValidatedHead && a.ValidatedBase == b.ValidatedBase && a.ValidatedTree == b.ValidatedTree && a.ValidatedGuide == b.ValidatedGuide && a.ValidatedMapping == b.ValidatedMapping && a.ValidatedPRSection == b.ValidatedPRSection && a.TestLog == b.TestLog && a.TestLogHash == b.TestLogHash
}

// Unlike a credential file, an archive/log must retain its exact bytes.
func readProofFile(path string, limit int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || before.Size() > limit {
		return nil, errors.New("proof archive/log must be a bounded private regular file, not a symlink")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		return nil, errors.New("proof file changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("proof file exceeds its size limit or cannot be read")
	}
	return b, nil
}

func checkArchivedForkProof(a, w WorkContext, in WorkInput, f ForkSpec) error {
	p, current := a.Fork, w.Fork
	if p == nil || current == nil || a.Stale || a.Event.Key != f.Key() || a.Event.Version != a.CurrentVersion || !idRE.MatchString(a.CurrentVersion) || a.Task == nil || a.Task.TaskID != in.TaskID || a.Task.GuideVersion != in.GuideVersion || a.Guide.Version != in.GuideVersion {
		return errors.New("archive must be original work context for this task and guide")
	}
	if p.SyncMode != adaptedBatches || p.SpecVersion != f.Version() || p.SpecVersion != current.SpecVersion || p.Round != current.Round || p.PR != current.PR || p.Branch != current.Branch || p.InitialTarget != current.InitialTarget || p.Upstream != current.Upstream || p.BatchAfter != current.BatchAfter || p.LastSuccess != current.LastSuccess || !slices.Equal(p.BatchCommits, current.BatchCommits) {
		return errors.New("archive belongs to a different fork round, PR or source batch")
	}
	if p.BatchID != current.BatchID || p.ParentBatch != current.ParentBatch || p.ParentHead != current.ParentHead || p.TargetBranch != current.TargetBranch || p.IntegrationBase != current.IntegrationBase {
		return errors.New("archive belongs to a different queue dependency generation")
	}
	v := p.Validation
	if !validForkProofHead(*p) || !shaRE.MatchString(p.ValidatedBase) || !shaRE.MatchString(p.ValidatedTree) || p.ValidatedGuide != in.GuideVersion || !idRE.MatchString(p.ValidatedMapping) || !idRE.MatchString(p.TestLogHash) || v == nil || v.State != "passed" || !idRE.MatchString(v.ID) || v.Round != p.Round || v.Head != p.ValidatedHead || v.Base != p.ValidatedBase || v.Guide != p.ValidatedGuide || v.Log != p.TestLog || v.StartedAt <= 0 || v.FinishedAt < v.StartedAt || v.FinishedAt >= v.Deadline || a.ObservedAt < v.FinishedAt {
		return errors.New("archive does not contain a completed, matching pre-merge test proof")
	}
	var snap Snapshot
	if json.Unmarshal(a.Snapshot, &snap) != nil || snap.Key != f.Key() {
		return errors.New("invalid archived snapshot")
	}
	pull, facts := snap.Objects["pull"], snap.Objects["fork"]
	if pull["state"] != "open" || pull["merged"] != false || pull["head"] != p.ValidatedHead || pull["base_sha"] != p.ValidatedBase || pull["head_ref"] != p.Branch || pull["head_repo"] != f.Target.Repo || pull["base"] != f.Target.Branch || facts["round"] != p.Round || facts["target_sha"] != p.ValidatedBase {
		return errors.New("archived PR does not match the tested pre-merge head and base")
	}
	if err := checkProvenanceBody(str(snap.Objects["issue"]["body"]), p.ValidatedPRSection); err != nil {
		return err
	}
	return nil
}

// Recovery may repair a stale proof only for the queue head already merged into
// integration. Ordinary execution still requires queueWorkAllowed. Exact grant,
// archived proof and live merge checks below remain mandatory for restoration.
func requireForkProofRecovery(w WorkContext, in WorkInput, f ForkSpec) error {
	if err := requireForkExecution(w, in); err != nil {
		return err
	}
	p := w.Fork
	if p.Queue != nil && (p.BatchID != queueHead(*p) || p.ParentBatch != "" || forkTargetBranch(f, *p) != f.Target.Branch) {
		return errors.New("historical recovery requires the queue head targeting integration")
	}
	if queueValidationRunning(*p) {
		return errors.New("cannot recover proof while a validation is running")
	}
	return nil
}

func (s *Service) RecoverForkProof(ctx context.Context, in WorkInput) (ForkProofRecoveryReceipt, error) {
	var empty ForkProofRecoveryReceipt
	w, err := s.CheckWork(ctx, in)
	if err != nil {
		return empty, err
	}
	f, _ := s.Config.Fork(w.Event.Key)
	if err = requireForkProofRecovery(w, in, f); err != nil {
		return empty, err
	}
	if err = s.Config.validateForkProofRecoveries(); err != nil {
		return empty, err
	}
	var grant *ForkProofRecovery
	for _, a := range s.Config.ForkProofRecoveries {
		if a.ForkID == f.ID && a.PR == w.Fork.PR {
			grant = &a
		}
	}
	if grant == nil {
		return empty, errors.New("no operator-pinned historical proof recovery for this PR")
	}
	_, metadata, err := s.forkWorkspacePaths(w.Fork.Round)
	if err != nil {
		return empty, err
	}
	lock, err := workspaceLock(metadata)
	if err != nil {
		return empty, err
	}
	defer lock.Close()
	if w.Fork.Validation != nil && w.Fork.Validation.State == "running" {
		return empty, errors.New("cannot recover proof while a validation is running")
	}
	b, err := readProofFile(grant.ProofFile, 4<<20)
	if err != nil {
		return empty, err
	}
	var archived WorkContext
	if hash(b) != grant.ProofSHA256 || json.Unmarshal(b, &archived) != nil {
		return empty, errors.New("proof archive differs from the operator-pinned SHA256")
	}
	if err = checkArchivedForkProof(archived, w, in, f); err != nil {
		return empty, err
	}
	p := *archived.Fork
	if !strings.HasPrefix(p.TestLog, metadata+".test-") || !strings.HasSuffix(p.TestLog, ".log") || filepath.Dir(p.TestLog) != filepath.Dir(metadata) {
		return empty, errors.New("original test log is outside this round's workspace")
	}
	log, err := readProofFile(p.TestLog, 16<<20)
	if err != nil {
		return empty, err
	}
	header := fmt.Sprintf("attempt: %s\nsource: %s\ntarget: %s\nmerge tree: %s\n", p.Validation.ID, p.ValidatedHead, p.ValidatedBase, p.ValidatedTree)
	if hash(log) != p.TestLogHash || !strings.HasPrefix(string(log), header) || strings.Contains(string(log), "validation did not save a passing proof:") {
		return empty, errors.New("original test log does not match the archived passing proof")
	}
	// Reuse the live collector's exact PR, parent, tree and reachability checks.
	source, ok := s.GH.(ForkSource)
	if !ok {
		return empty, errors.New("fork collector unavailable")
	}
	if err = s.GH.CheckIdentity(ctx); err != nil {
		return empty, err
	}
	observation, err := source.CollectFork(ctx, f, restoreForkProof(*w.Fork, p))
	if err != nil {
		return empty, err
	}
	pull := observation.Objects["pull"]
	mergedAt, err := time.Parse(time.RFC3339, str(pull["merged_at"]))
	if err != nil || p.Validation.FinishedAt > mergedAt.Unix() || archived.ObservedAt > mergedAt.Unix() || mergedAt.After(time.Now()) || observation.PR != grant.PR || !observation.MergeVerified || observation.MergeCommit != grant.MergeCommit || observation.Target != str(forkFacts(w)["target_sha"]) || pull["merged"] != true || pull["state"] != "closed" || pull["head"] != p.ValidatedHead || pull["base_sha"] != p.ValidatedBase || pull["head_ref"] != p.Branch || pull["head_repo"] != f.Target.Repo || pull["base"] != f.Target.Branch {
		return empty, errors.New("live merged PR, tested history or pre-merge proof time does not match recovery grant")
	}
	if err = checkProvenanceBody(str(observation.Objects["issue"]["body"]), p.ValidatedPRSection); err != nil {
		return empty, err
	}
	mapping, section, err := validateForkMapping(ctx, f, &p, Workspace{Path: f.LocalRepository, Head: p.ValidatedHead, MappingFile: metadata + ".commits.json"})
	if err != nil {
		return empty, err
	}
	if mapping != p.ValidatedMapping || section != p.ValidatedPRSection {
		return empty, errors.New("source mapping differs from the original proof")
	}
	// The collector does not mutate progress. Recheck task ownership and evidence
	// after network/file reads, then fence concurrent writers in the transaction.
	latest, err := s.CheckWork(ctx, in)
	if err != nil {
		return empty, err
	}
	if err = requireForkProofRecovery(latest, in, f); err != nil {
		return empty, err
	}
	if latest.Fork.Revision != w.Fork.Revision {
		return empty, ErrStaleWork
	}
	receipt := ForkProofRecoveryReceipt{ID: hash(canonical(*grant)), Resource: f.Key(), Input: in, Grant: *grant, Archived: p, Previous: *w.Fork, MergedAt: str(pull["merged_at"]), RecoveredAt: time.Now().Unix()}
	return s.Store.RestoreHistoricalForkProof(ctx, receipt)
}
