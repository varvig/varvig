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
		// A whole-tree view is confined to scope and excludes deny-listed paths, so
		// a diff or status never renders content the task may not read (U5). A
		// targeted read of a denied path is refused loudly in resolvePath.
		if g.grant.Covers(p) && !g.deny.Denied(p) {
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

// --- affected set (design §1.3) ---

// toolAffected answers "what does this change affect" through the gate. It was
// CLI-only for its whole life — the shell held all the wiring — so an agent
// could not ask the one question most likely to keep it from breaking something
// downstream of its own scope (design addendum, U1).
//
// The analysis is core.Affected verbatim; the gate adds three things and nothing
// else: scope confinement, an honest count of what confinement withheld, and the
// coverage descriptor that keeps an unanalyzed language from reading as an
// absence of dependency.
//
// Computing the graph reads the whole tree, but the result is not folded into the
// task's read set. An affected set is derived, rebuildable index content, not
// authored content the change depends on; recording it as a read would make every
// task stale on every commit and the read set meaningless.
func toolAffected(g *Gate, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Change string `json:"change"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	id, err := g.resolveChange(a.Change)
	if err != nil {
		return nil, err
	}
	parent, err := core.FirstParent(g.repo, id)
	if err != nil {
		return nil, gerr(codeInvalidArgs, "cannot read %s as a change: %v", id.Hex(), err)
	}
	res, err := core.Affected(g.repo, parent, id)
	if err != nil {
		return nil, gerr(codeInternal, "cannot compute the affected set: %v", err)
	}

	changed, changedOut := g.partitionByScope(res.Changed)
	affected, affectedOut := g.partitionByScope(res.Affected)

	pulled := make([]string, 0, len(affected))
	for _, p := range affected {
		if res.Pulled(p) {
			pulled = append(pulled, p)
		}
	}

	return map[string]any{
		"base":     g.baseHex(),
		"change":   id.Hex(),
		"parent":   hexOrEmpty(parent),
		"changed":  changed,
		"affected": affected,
		// Pulled is the half an agent acts on: paths it did not edit that its
		// edit reaches anyway.
		"pulled": pulled,
		// Out-of-scope counts, never paths: a task learns that its change reaches
		// beyond its scope without learning the layout it may not read. A nonzero
		// affected count is grounds for report_blocked.
		"out_of_scope": map[string]any{
			"changed":  changedOut,
			"affected": affectedOut,
		},
		"coverage": coveragePayload(res.Coverage),
	}, nil
}

// partitionByScope splits paths into the ones the task may see and a count of
// the ones it may not. The count is deliberately not a path list — the whole
// point of confinement is that the layout outside scope stays unseen — but it is
// also not silence, because "your change affects things you cannot see" is
// exactly what a task needs to know (build spec P1.2).
func (g *Gate) partitionByScope(paths []string) (inScope []string, outOfScope int) {
	inScope = make([]string, 0, len(paths))
	for _, p := range paths {
		if g.grant.Covers(p) && !g.deny.Denied(p) {
			inScope = append(inScope, p)
			continue
		}
		outOfScope++
	}
	return inScope, outOfScope
}

// coveragePayload renders a coverage descriptor. It is always present in an
// affected result: a caller must be able to tell "nothing depends on this" from
// "no analyzer understands this language" (design §5), and an optional field is
// one an agent will not read.
func coveragePayload(c core.Coverage) map[string]any {
	exts := c.UnanalyzedExts
	if exts == nil {
		exts = []string{}
	}
	return map[string]any{
		"complete":         c.Complete(),
		"analyzed_files":   c.Analyzed,
		"unanalyzed_files": c.Unanalyzed,
		"unanalyzed_types": exts,
	}
}
