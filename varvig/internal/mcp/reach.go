package mcp

import (
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/store"
)

// Scope must be enforced on object reachability, not only on path strings (MCP
// spec §9.4 — "the easy miss"). trust.Scope.Covers is a pure path-prefix test,
// so a hash fetched directly could belong to an out-of-scope subtree. The gate
// closes that by computing the set of tree and blob hashes reachable from the
// task's scope subtree of its base, and checking direct-hash reads against it.

// reachable returns (and memoizes) the set of object hashes inside the task's
// scope: the scope subtree's own tree hash plus every tree and blob hash beneath
// it. Built from the raw query so it does not itself pollute the read log — the
// read log records what the agent requested, not this bookkeeping. An empty
// base (fresh repo) has an empty set.
func (g *Gate) reachable() (map[string]bool, error) {
	if g.reach != nil {
		return g.reach, nil
	}
	set := map[string]bool{}
	if g.base != nil {
		// The reachable set is the union of every scope's subtree, so an object is
		// in-scope if any declared scope covers it (build spec P0.5). A declared
		// scope with no files in the base yet is not an error — over-declaration is
		// allowed; it simply contributes nothing.
		for _, sc := range g.grant.Scopes {
			root := strings.Trim(string(sc), "/")
			listing, err := g.q.Tree(g.base, root)
			if err != nil {
				continue // scope path absent in the base: contributes nothing
			}
			subID, err := multihash.ParseHex(listing.Hash)
			if err != nil {
				return nil, gerr(codeInternal, "bad subtree hash for scope %q: %v", sc, err)
			}
			if err := collectReach(g.repo.Objects, subID, set); err != nil {
				return nil, gerr(codeInternal, "walking scope %q subtree: %v", sc, err)
			}
		}
	}
	g.reach = set
	return set, nil
}

// collectReach adds treeID and every tree/blob hash beneath it to set.
func collectReach(s *store.Store, treeID multihash.Multihash, set map[string]bool) error {
	if treeID == nil {
		return nil
	}
	key := treeID.Hex()
	if set[key] {
		return nil // shared subtree already walked (Merkle DAG)
	}
	set[key] = true
	o, err := s.Get(treeID)
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
			if err := collectReach(s, e.ID, set); err != nil {
				return err
			}
			continue
		}
		set[e.ID.Hex()] = true // blob
	}
	return nil
}

// inScopeObject reports whether a directly-addressed blob/tree hash lies within
// the task's scope by reachability. Non-subtree objects (changes, refs) are not
// gated here — their file content is reached through tree/blob reads, which are
// gated — so this returns true for a hash that is not a tree/blob in the base.
func (g *Gate) inScopeObject(id multihash.Multihash) (bool, error) {
	// Whole-repo scope reaches everything; skip the walk.
	if g.scopePath() == "" {
		return true, nil
	}
	set, err := g.reachable()
	if err != nil {
		return false, err
	}
	return set[id.Hex()], nil
}
