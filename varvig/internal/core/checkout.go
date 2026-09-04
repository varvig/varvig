package core

import (
	"fmt"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// CheckoutStat reports what provisioning a task checkout cost, so the price of a
// full checkout is visible per task (design addendum, F1: "provisioning cost is
// measured and reported").
type CheckoutStat struct {
	Objects  int           // objects replicated into the checkout
	Method   string        // how object bytes were replicated ("copy")
	Duration time.Duration // wall-clock time to provision
}

// TreeOf resolves a change id to its tree, or returns id unchanged if it is
// already a tree. A nil id is the empty tree.
func TreeOf(r *repo.Repo, id multihash.Multihash) (multihash.Multihash, error) {
	if id == nil {
		return nil, nil
	}
	o, err := r.Objects.Get(id)
	if err != nil {
		return nil, err
	}
	if o.Type() == object.TypeChange {
		c, err := o.AsChange()
		if err != nil {
			return nil, err
		}
		return c.Tree, nil
	}
	return id, nil
}

// ProvisionCheckout creates a full, ordinary varvig repository at dir: it
// replicates the object closure reachable from base, creates the base ref (and
// only that ref — base-only visibility, F2), points HEAD at it, and materializes
// the whole base tree as a working tree. The result is a repository in which
// `varvig diff|status|log|verify|commit|git-export` all work unmodified, which is
// what makes diff/status reachable from a task and removes the sparse-checkout
// machinery (design addendum, F1). An empty base yields an empty ordinary repo.
func ProvisionCheckout(src *repo.Repo, dir string, base multihash.Multihash) (*repo.Repo, CheckoutStat, error) {
	start := time.Now()
	dst, err := repo.Init(dir)
	if err != nil {
		return nil, CheckoutStat{}, err
	}
	stat := CheckoutStat{Method: "copy"}
	if base != nil {
		n, err := replicateClosure(src, dst, base)
		if err != nil {
			return nil, CheckoutStat{}, fmt.Errorf("core: replicate objects: %w", err)
		}
		stat.Objects = n

		tree, err := TreeOf(dst, base)
		if err != nil {
			return nil, CheckoutStat{}, err
		}
		// Only the base ref is created — sibling task refs, ticket refs, and other
		// branches are absent and unresolvable in the checkout (F2).
		if err := dst.Refs.Create("refs/heads/main", base, "task", "checkout"); err != nil {
			return nil, CheckoutStat{}, err
		}
		if tree != nil {
			if err := worktree.Checkout(dst.Objects, tree, dir); err != nil {
				return nil, CheckoutStat{}, fmt.Errorf("core: materialize working tree: %w", err)
			}
		}
	}
	stat.Duration = time.Since(start)
	return dst, stat, nil
}

// replicateClosure copies every object reachable from root (a change's DAG, its
// provenance, and its tree closure) from src to dst, returning the count. Objects
// are content-addressed and verified on store, so a copy is a faithful replica.
func replicateClosure(src, dst *repo.Repo, root multihash.Multihash) (int, error) {
	seen := map[string]bool{}
	var mark func(id multihash.Multihash) error
	mark = func(id multihash.Multihash) error {
		key := id.Hex()
		if seen[key] || id == nil {
			return nil
		}
		if !src.Objects.Has(id) {
			return nil // a link to an object not present locally; skip gracefully
		}
		seen[key] = true
		raw, err := src.Objects.GetRaw(id)
		if err != nil {
			return err
		}
		if err := dst.Objects.PutVerified(id, raw); err != nil {
			return err
		}
		obj, err := src.Objects.Get(id)
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
	if err := mark(root); err != nil {
		return 0, err
	}
	return len(seen), nil
}
