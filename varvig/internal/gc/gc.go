// Package gc is Varvig's garbage collector (design §1.5: retention is a
// first-order problem, so GC is designed in, not bolted on). It is a
// mark-and-sweep over the object store from a fixed root set:
//
//   - every ref (branches, tags, notes, hooks, remotes) and its target
//   - every id ever recorded in a reflog (old and new), so anything recoverable
//     through the reflog survives — universal undo is preserved (design §2)
//   - every live speculation candidate in the pool
//
// Anything not reachable from those roots is unreferenced and swept. Retention
// (spec.Prune) decides what stops being a root; GC reclaims what retention let
// go. GC is an offline maintenance operation, not run concurrently with writers.
package gc

import (
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/spec"
)

// Report summarizes a collection.
type Report struct {
	Roots   int
	Scanned int
	Kept    int
	Deleted int
	// DeletedIDs lists reclaimed objects (populated on dry runs and real runs).
	DeletedIDs []multihash.Multihash
	// ExternalUnreachable lists the external artifacts whose artifact-ref became
	// unreachable this pass (federation §1.3). varvig reports these; deleting the
	// bytes from a registry is the operator's decision — varvig has no
	// credentials there. Populated on both dry and real runs.
	ExternalUnreachable []ExternalArtifact
}

// ExternalArtifact is the identity of an external artifact that GC found newly
// unreachable — enough for an operator to locate and delete the bytes.
type ExternalArtifact struct {
	ContentHash multihash.Multihash
	MediaType   string
	Locators    []string
}

// Roots returns the garbage-collection roots: ref targets, all reflog ids, and
// live speculation candidates. pool may be nil.
func Roots(r *repo.Repo, pool *spec.Pool) ([]multihash.Multihash, error) {
	var roots []multihash.Multihash

	names, err := r.Refs.List()
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if id, err := r.Refs.Resolve(n); err == nil {
			roots = append(roots, id)
		}
	}

	logs, err := r.Refs.LogNames()
	if err != nil {
		return nil, err
	}
	for _, n := range logs {
		entries, err := r.Refs.ReadLog(n)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.Old != nil {
				roots = append(roots, e.Old)
			}
			if e.New != nil {
				roots = append(roots, e.New)
			}
		}
	}

	if pool != nil {
		changes, err := pool.AllChanges()
		if err != nil {
			return nil, err
		}
		roots = append(roots, changes...)
	}
	return roots, nil
}

// Collect marks everything reachable from the roots and sweeps the rest. When
// dryRun is true nothing is deleted; the report still lists what would be.
func Collect(r *repo.Repo, pool *spec.Pool, dryRun bool) (Report, error) {
	roots, err := Roots(r, pool)
	if err != nil {
		return Report{}, err
	}

	live := map[string]bool{}
	var mark func(id multihash.Multihash) error
	mark = func(id multihash.Multihash) error {
		key := id.Hex()
		if live[key] {
			return nil
		}
		// A root may name an object that no longer exists (e.g. a reflog id
		// from a prior GC); skip such gracefully.
		if !r.Objects.Has(id) {
			return nil
		}
		live[key] = true
		obj, err := r.Objects.Get(id)
		if err != nil {
			return err
		}
		links, err := obj.Links()
		if err != nil {
			return err
		}
		for _, l := range links {
			if err := mark(l); err != nil {
				return err
			}
		}
		return nil
	}
	for _, root := range roots {
		if err := mark(root); err != nil {
			return Report{}, err
		}
	}

	rep := Report{Roots: len(roots), Kept: len(live)}
	var doomed []multihash.Multihash
	err = r.Objects.Walk(func(id multihash.Multihash) error {
		rep.Scanned++
		if !live[id.Hex()] {
			doomed = append(doomed, id)
		}
		return nil
	})
	if err != nil {
		return Report{}, err
	}

	for _, id := range doomed {
		// Before reclaiming, classify: a doomed artifact-ref means its external
		// bytes just lost their last reachable referent. Record the identity so
		// `gc --report-external` can surface it; decode while the object still
		// exists (real runs delete it below).
		if obj, err := r.Objects.Get(id); err == nil && obj.Type() == object.TypeArtifactRef {
			if a, err := obj.AsArtifactRef(); err == nil {
				rep.ExternalUnreachable = append(rep.ExternalUnreachable, ExternalArtifact{
					ContentHash: a.ContentHash,
					MediaType:   a.MediaType,
					Locators:    a.Locators,
				})
			}
		}
		if !dryRun {
			if err := r.Objects.Delete(id); err != nil {
				return rep, err
			}
		}
		rep.Deleted++
		rep.DeletedIDs = append(rep.DeletedIDs, id)
	}
	return rep, nil
}
