package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/trust"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// The task record is the scheduler's own record of a task (design addendum, F4):
// {which key, what scope, what base}, written where the scheduler runs — the
// source repository's metadata dir, which the task's sandboxed checkout cannot
// write. It is the trusted counterpart to a change's self-description
// (provenance.Scope), so "self-described scope disagrees with its record" is a
// check the scheduler can actually make. A checkout-local marker is only a hint;
// this is the record.

// taskRecordFile is the per-repo registry of minted tasks, keyed by the task
// key's fingerprint.
const taskRecordFile = "tasks.json"

// TaskRecord is what the scheduler remembers about a minted task.
type TaskRecord struct {
	Fingerprint string `json:"fingerprint"` // the task key's fingerprint (== provenance authority)
	Scope       string `json:"scope"`       // the scope the task was granted, verbatim
	Base        string `json:"base"`        // the base the task reads and proposes from (hex, may be empty)
}

func taskRecordPath(r *repo.Repo) string { return filepath.Join(r.GitDir(), taskRecordFile) }

func loadTaskRecords(r *repo.Repo) (map[string]TaskRecord, error) {
	b, err := os.ReadFile(taskRecordPath(r))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]TaskRecord{}, nil
		}
		return nil, err
	}
	out := map[string]TaskRecord{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("core: corrupt task record file: %w", err)
	}
	return out, nil
}

// RecordTask persists a task record in the source repository, keyed by
// fingerprint. It is written by `task start` at mint time — as the operator, not
// the task — so it is the scheduler's own trusted record.
func RecordTask(r *repo.Repo, rec TaskRecord) error {
	recs, err := loadTaskRecords(r)
	if err != nil {
		return err
	}
	recs[rec.Fingerprint] = rec
	b, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(taskRecordPath(r), b, 0o600)
}

// LookupTask returns the task record for a fingerprint, if one was minted.
func LookupTask(r *repo.Repo, fingerprint string) (TaskRecord, bool, error) {
	recs, err := loadTaskRecords(r)
	if err != nil {
		return TaskRecord{}, false, err
	}
	rec, ok := recs[fingerprint]
	return rec, ok, nil
}

// VerifyTaskScope re-verifies a change against the scheduler's task record (F4):
// if the change's authority names a recorded task, its self-described scope must
// equal the record's scope, and every path it changed must fall within that
// scope. A change whose authority is not a recorded task (a human commit, a
// foreign import) is unaffected. It is the scope half of the U4 invariant — the
// authority half is VerifyAuthority — and, like it, an integrity check the
// scheduler runs unconditionally.
func VerifyTaskScope(r *repo.Repo, changeID multihash.Multihash) error {
	obj, err := r.Objects.Get(changeID)
	if err != nil {
		return err
	}
	c, err := obj.AsChange()
	if err != nil || c.Provenance == nil {
		return nil // not a change, or no provenance to check
	}
	provObj, err := r.Objects.Get(c.Provenance)
	if err != nil {
		return err
	}
	pv, err := provObj.AsProvenance()
	if err != nil {
		return err
	}
	rec, ok, err := LookupTask(r, pv.Authority)
	if err != nil {
		return err
	}
	if !ok {
		return nil // not a change produced by a recorded task
	}
	// The self-description must match the record: a hand-widened scope in the
	// checkout does not agree with what the scheduler granted.
	if pv.Scope != rec.Scope {
		return fmt.Errorf("core: change %s claims scope %q but its task was granted %q — "+
			"self-described scope disagrees with the scheduler's record", changeID.Hex(), pv.Scope, rec.Scope)
	}
	// Every path the change touched must lie within the granted scope.
	changed, err := changedPathsOf(r, c.Tree, c.Parents)
	if err != nil {
		return err
	}
	scopes := trust.NewScopeSet(rec.Scope)
	for _, p := range changed {
		if !scopes.Covers(p) {
			return fmt.Errorf("core: change %s wrote %q, outside its task scope %q", changeID.Hex(), p, rec.Scope)
		}
	}
	return nil
}

// changedPathsOf returns the paths a change touched relative to its first parent.
func changedPathsOf(r *repo.Repo, tree multihash.Multihash, parents []multihash.Multihash) ([]string, error) {
	var parentTree multihash.Multihash
	if len(parents) > 0 {
		pt, err := TreeOf(r, parents[0])
		if err != nil {
			return nil, err
		}
		parentTree = pt
	}
	base, err := worktree.FlattenStates(r.Objects, parentTree)
	if err != nil {
		return nil, err
	}
	work, err := worktree.FlattenStates(r.Objects, tree)
	if err != nil {
		return nil, err
	}
	return ChangedPaths(worktree.Compare(base, work)), nil
}
