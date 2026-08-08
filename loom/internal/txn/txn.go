package txn

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
	"github.com/dividebyzero/claude-experiments/loom/internal/object"
	"github.com/dividebyzero/claude-experiments/loom/internal/refs"
	"github.com/dividebyzero/claude-experiments/loom/internal/repo"
)

// Txn is a unit of work. It declares the path prefixes it reads and writes, and
// an Apply function that operates on a capability-scoped workspace. Apply may
// run more than once (it is re-run when the ref moves underneath a commit), so
// it must be a deterministic function of the workspace it is given.
type Txn struct {
	Name   string
	Reads  []string
	Writes []string
	Apply  func(ws *Workspace) error
}

// Result reports the outcome of a transaction.
type Result struct {
	Name     string
	Change   multihash.Multihash // the committed change, if any
	Attempts int
	NoOp     bool // Apply made no changes
	Err      error
}

// Scheduler runs transactions against a repository ref, serializing conflicting
// ones and parallelizing the rest.
type Scheduler struct {
	repo        *repo.Repo
	ref         string
	lm          *lockManager
	concurrency int
	maxAttempts int
}

// NewScheduler returns a scheduler committing to ref (e.g. refs/heads/main).
func NewScheduler(r *repo.Repo, ref string) *Scheduler {
	return &Scheduler{
		repo:        r,
		ref:         ref,
		lm:          newLockManager(),
		concurrency: 16,
		maxAttempts: 100,
	}
}

// Run executes every transaction and returns their results in input order.
// Non-conflicting transactions compute concurrently; conflicting ones are
// serialized by their declared sets. A transaction whose commit loses the ref
// CAS re-derives from the new base and re-runs, so disjoint work converges
// without human intervention (design §1.4).
func (s *Scheduler) Run(ctx context.Context, txns []*Txn) []Result {
	results := make([]Result, len(txns))
	sem := make(chan struct{}, s.concurrency)
	var wg sync.WaitGroup
	for i, t := range txns {
		wg.Add(1)
		go func(i int, t *Txn) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = Result{Name: t.Name, Err: ctx.Err()}
				return
			}
			results[i] = s.runOne(ctx, t)
		}(i, t)
	}
	wg.Wait()
	return results
}

func (s *Scheduler) runOne(ctx context.Context, t *Txn) Result {
	c := claim{reads: normalize(t.Reads), writes: normalize(t.Writes)}
	if err := s.lm.acquire(ctx, c); err != nil {
		return Result{Name: t.Name, Err: err}
	}
	defer s.lm.release(c)

	res := Result{Name: t.Name}
	for res.Attempts < s.maxAttempts {
		res.Attempts++
		if err := ctx.Err(); err != nil {
			res.Err = err
			return res
		}

		// Derive the base from the ref's current tip each attempt.
		base, err := s.currentTip()
		if err != nil {
			res.Err = err
			return res
		}
		baseTree, err := s.treeOf(base)
		if err != nil {
			res.Err = err
			return res
		}
		flat, err := flatten(s.repo.Objects, baseTree)
		if err != nil {
			res.Err = err
			return res
		}

		ws := newWorkspace(s.repo.Objects, flat, c.reads, c.writes)
		if err := t.Apply(ws); err != nil {
			// Apply errors (including capability violations) are terminal.
			res.Err = err
			return res
		}
		if !ws.mutated() {
			res.NoOp = true
			return res
		}

		newTree, err := ws.finalize()
		if err != nil {
			res.Err = err
			return res
		}
		var parents []multihash.Multihash
		if base != nil {
			parents = []multihash.Multihash{base}
		}
		change := object.NewChange(object.Change{
			Tree:      newTree,
			Parents:   parents,
			Message:   t.Name,
			Timestamp: 0,
			Author:    t.Name,
		})
		id, err := s.repo.Objects.Put(change)
		if err != nil {
			res.Err = err
			return res
		}

		err = s.repo.Refs.CompareAndSwap(s.ref, base, id, t.Name, "txn")
		if err == nil {
			res.Change = id
			return res
		}
		if !errors.Is(err, refs.ErrConflict) {
			res.Err = err
			return res
		}
		// Lost the CAS: a disjoint transaction advanced the ref. Re-derive and
		// retry with a small backoff.
		select {
		case <-ctx.Done():
			res.Err = ctx.Err()
			return res
		case <-time.After(time.Duration(res.Attempts) * time.Millisecond):
		}
	}
	res.Err = fmt.Errorf("txn %q: exceeded %d attempts", t.Name, s.maxAttempts)
	return res
}

func (s *Scheduler) currentTip() (multihash.Multihash, error) {
	tip, err := s.repo.Refs.Resolve(s.ref)
	if err != nil {
		if errors.Is(err, refs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return tip, nil
}

func (s *Scheduler) treeOf(change multihash.Multihash) (multihash.Multihash, error) {
	if change == nil {
		return nil, nil
	}
	obj, err := s.repo.Objects.Get(change)
	if err != nil {
		return nil, err
	}
	if obj.Type() != object.TypeChange {
		return change, nil // already a tree
	}
	c, err := obj.AsChange()
	if err != nil {
		return nil, err
	}
	return c.Tree, nil
}
