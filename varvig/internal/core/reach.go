package core

import (
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// CollectReach adds treeID and every tree/blob hash beneath it to set — the
// object-reachability walk the gate uses to enforce scope on directly-addressed
// hashes (MCP spec §9.4). It lives in core so the gate need not import the object
// vocabulary to walk a tree (design addendum, U1).
func CollectReach(r *repo.Repo, treeID multihash.Multihash, set map[string]bool) error {
	if treeID == nil {
		return nil
	}
	key := treeID.Hex()
	if set[key] {
		return nil // shared subtree already walked (Merkle DAG)
	}
	set[key] = true
	o, err := r.Objects.Get(treeID)
	if err != nil {
		return err
	}
	if o.Type() != object.TypeTree {
		return nil // a blob leaf reached directly; already recorded above
	}
	entries, err := o.TreeEntries()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Kind == object.TypeTree {
			if err := CollectReach(r, e.ID, set); err != nil {
				return err
			}
			continue
		}
		set[e.ID.Hex()] = true // blob
	}
	return nil
}
