package mcp

import (
	"encoding/json"

	"github.com/dividebyzero/claude-experiments/varvig/internal/core"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// This file makes the diff and status read verbs reachable through the gate
// (design addendum, U1). They existed only in the CLI, and a task checkout could
// not reach the CLI, so an agent had no independent view of its own change — the
// originating bug. The rendering lives in the shared core; the gate adds only
// scope confinement (a diff never shows content outside the task's scope) and the
// JSON envelope.

// maxUnifiedBytes caps the unified diff a single call returns, so one large diff
// cannot blow the context budget. When it binds, the text is cut and the response
// is marked truncated rather than silently shortened (§6).
const maxUnifiedBytes = 96 << 10

// diffSidesGate resolves the two sides of a gate diff, both confined to the
// task's scope. With a change argument it is that change against its first parent
// (what the change did); with no argument and a bound checkout it is the checkout
// against the base (the observed set); otherwise it errors, naming the options.
func (g *Gate) diffSidesGate(changeArg string) (baseTree, newTree multihash.Multihash, base, work map[string]worktree.FileState, err error) {
	switch {
	case changeArg != "":
		id, e := g.resolveChange(changeArg)
		if e != nil {
			return nil, nil, nil, nil, e
		}
		obj, e := g.repo.Objects.Get(id)
		if e != nil {
			return nil, nil, nil, nil, gerr(codeNotFound, "cannot read change %s", id.Hex())
		}
		c, e := obj.AsChange()
		if e != nil {
			return nil, nil, nil, nil, gerr(codeInvalidArgs, "%s is not a change", id.Hex())
		}
		newTree = c.Tree
		if len(c.Parents) > 0 {
			if baseTree, e = treeOfChange(g.repo, c.Parents[0]); e != nil {
				return nil, nil, nil, nil, gerr(codeInternal, "cannot read parent tree: %v", e)
			}
		}
	case g.checkout != "":
		if g.base != nil {
			if baseTree, err = treeOfChange(g.repo, g.base); err != nil {
				return nil, nil, nil, nil, gerr(codeInternal, "cannot read base tree: %v", err)
			}
		}
		idx := worktree.OpenIndex(g.checkout)
		w, e := worktree.Scan(g.repo.Objects, g.checkout, idx)
		if e != nil {
			return nil, nil, nil, nil, gerr(codeInternal, "cannot scan checkout: %v", e)
		}
		work = g.scopeStates(w)
	default:
		return nil, nil, nil, nil, gerr(codeInvalidArgs,
			"diff needs a change to inspect, or a working tree to observe — pass change, or start the gate with --checkout")
	}

	if base, err = g.flattenScoped(baseTree); err != nil {
		return nil, nil, nil, nil, err
	}
	if work == nil {
		if work, err = g.flattenScoped(newTree); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	return baseTree, newTree, base, work, nil
}

// flattenScoped flattens a tree and drops every path outside the task's scope, so
// a diff or status can never surface out-of-scope content — the read-confinement
// the gate must preserve even while diffing whole trees.
func (g *Gate) flattenScoped(tree multihash.Multihash) (map[string]worktree.FileState, error) {
	all, err := worktree.FlattenStates(g.repo.Objects, tree)
	if err != nil {
		return nil, gerr(codeInternal, "cannot flatten tree: %v", err)
	}
	return g.scopeStates(all), nil
}

func (g *Gate) scopeStates(m map[string]worktree.FileState) map[string]worktree.FileState {
	out := make(map[string]worktree.FileState, len(m))
	for p, s := range m {
		if g.grant.Covers(p) {
			out[p] = s
		}
	}
	return out
}

// blobReader reads a path's content from a flattened state map (the blob is
// already in the store — a scanned checkout stores its blobs too), and folds it
// into the task's read set, since a diff is a read of that content.
func (g *Gate) blobReader(states map[string]worktree.FileState) core.BytesFn {
	return func(p string) ([]byte, bool, error) {
		s, ok := states[p]
		if !ok || s.Hash == nil {
			return nil, false, nil
		}
		obj, err := g.repo.Objects.Get(s.Hash)
		if err != nil {
			return nil, false, err
		}
		c, ok := obj.BlobContent()
		if ok {
			g.record(s.Hash.Hex())
		}
		return c, ok, nil
	}
}

func toolDiff(g *Gate, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Change string `json:"change"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	baseTree, newTree, base, work, err := g.diffSidesGate(a.Change)
	if err != nil {
		return nil, err
	}
	res := core.Diff(base, work, g.blobReader(base), g.blobReader(work))

	stat := make([]map[string]any, 0, len(res.Stat))
	for _, s := range res.Stat {
		e := map[string]any{"path": s.Path}
		if s.Note != "" {
			e["note"] = s.Note
		} else {
			e["added"], e["removed"] = s.Added, s.Removed
		}
		stat = append(stat, e)
	}

	unified := res.Unified
	truncated := false
	if len(unified) > maxUnifiedBytes {
		unified = unified[:maxUnifiedBytes]
		truncated = true
	}

	out := map[string]any{
		"base":      g.baseHex(),
		"tree_a":    hexOrEmpty(baseTree),
		"tree_b":    hexOrEmpty(newTree),
		"changed":   res.Changed,
		"stat":      stat,
		"unified":   unified,
		"truncated": truncated,
	}
	if truncated {
		out["truncated_field"] = "unified"
		out["code"] = codeTruncated
	}
	return out, nil
}

func toolStatus(g *Gate, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Change string `json:"change"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	baseTree, newTree, base, work, err := g.diffSidesGate(a.Change)
	if err != nil {
		return nil, err
	}
	d := worktree.Compare(base, work)
	renames := make([]map[string]string, 0, len(d.Renamed))
	for _, rn := range d.Renamed {
		renames = append(renames, map[string]string{"from": rn.From, "to": rn.To})
	}
	return map[string]any{
		"base":     g.baseHex(),
		"tree_a":   hexOrEmpty(baseTree),
		"tree_b":   hexOrEmpty(newTree),
		"clean":    d.Empty(),
		"added":    d.Added,
		"modified": d.Modified,
		"deleted":  d.Removed,
		"mode":     d.ModeChanged,
		"renamed":  renames,
	}, nil
}

// hexOrEmpty is the hex of an id, or "" for a nil (unborn base / empty tree).
func hexOrEmpty(m multihash.Multihash) string {
	if m == nil {
		return ""
	}
	return m.Hex()
}
