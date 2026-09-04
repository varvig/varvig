package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/spec"
	"github.com/dividebyzero/claude-experiments/varvig/internal/trust"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// cmdPropose records a speculative change from the working tree (build spec
// P1.1). With no path arguments it proposes everything changed inside the
// declared write set — the *observed* set, computed the same way `diff
// --name-only` is — so a forgotten file is never silently dropped from a
// hand-assembled list. Explicit paths narrow that set; they can never name a path
// that was not observed. It always prints the path set it is about to submit
// (P0.4) unless --quiet. It signs a change and records it in the speculation
// pool; it never moves a ref.
//
//	varvig propose -m <msg> [--reasoning R] [--scope S ...] [--quiet] [paths...]
func cmdPropose(args []string) error {
	var msg, reasoning, scope string
	var paths []string
	quiet := false
	scope = "/"
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-m":
			if i+1 >= len(args) {
				return errUsagePropose()
			}
			msg, i = args[i+1], i+1
		case "--reasoning":
			if i+1 >= len(args) {
				return fmt.Errorf("propose: --reasoning requires a value")
			}
			reasoning, i = args[i+1], i+1
		case "--scope":
			if i+1 >= len(args) {
				return fmt.Errorf("propose: --scope requires a value")
			}
			scope, i = unionScope(scope, args[i+1]), i+1
		case "--quiet":
			quiet = true
		default:
			if strings.HasPrefix(a, "--") {
				return fmt.Errorf("propose: unknown flag %q", a)
			}
			paths = append(paths, strings.Trim(a, "/"))
		}
	}
	if strings.TrimSpace(msg) == "" {
		return errUsagePropose()
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	scopes := trust.NewScopeSet(scope)

	// The observed set: the working tree diffed against the base.
	baseChange, baseTreeID, err := baseChangeAndTree(r)
	if err != nil {
		return err
	}
	baseStates, err := worktree.FlattenStates(r.Objects, baseTreeID)
	if err != nil {
		return err
	}
	idx := worktree.OpenIndex(r.GitDir())
	work, err := worktree.Scan(r.Objects, r.Root(), idx)
	if err != nil {
		return err
	}
	_ = idx.Save()
	d := worktree.Compare(baseStates, work)

	edits, err := selectEdits(d, scopes, paths)
	if err != nil {
		return err
	}

	// Preview (P0.4): show exactly the path set about to be submitted, before it is.
	if !quiet {
		fmt.Printf("proposing %d path(s) within scope %s:\n", len(edits), scopes.String())
		printed := make([]proposeEdit, len(edits))
		copy(printed, edits)
		sort.Slice(printed, func(i, j int) bool { return printed[i].path < printed[j].path })
		for _, e := range printed {
			op := "write"
			if e.del {
				op = "delete"
			}
			fmt.Printf("  %-7s %s\n", op, e.path)
		}
	}

	// Overlay the selected edits onto the base tree to form the proposed tree.
	proposed := make(map[string]worktree.FileState, len(baseStates))
	for p, s := range baseStates {
		proposed[p] = s
	}
	for _, e := range edits {
		if e.del {
			delete(proposed, e.path)
		} else {
			proposed[e.path] = work[e.path]
		}
	}
	newTree, err := worktree.BuildTree(r.Objects, proposed)
	if err != nil {
		return fmt.Errorf("propose: build tree: %w", err)
	}

	// Sign a speculative change and record it in the pool. It never moves a ref.
	provID, err := r.Objects.Put(object.NewProvenance(object.Provenance{
		Authority: author(),
		TaskSpec:  msg,
		Reasoning: reasoning,
	}))
	if err != nil {
		return err
	}
	var parents []multihash.Multihash
	if baseChange != nil {
		parents = append(parents, baseChange)
	}
	change := object.NewChange(object.Change{
		Tree:       newTree,
		Parents:    parents,
		Message:    msg,
		Timestamp:  time.Now().Unix(),
		Author:     author(),
		Provenance: provID,
	})
	priv, err := provenance.LoadOrCreateIdentity(r.GitDir())
	if err != nil {
		return err
	}
	if err := provenance.Sign(change, priv); err != nil {
		return err
	}
	changeID, err := r.Objects.Put(change)
	if err != nil {
		return err
	}
	if err := spec.Open(r.GitDir()).Add(proposeTask, changeID, time.Now().Unix()); err != nil {
		return fmt.Errorf("propose: record proposal: %w", err)
	}
	fmt.Printf("proposed %s (tree %s)\n", changeID.Hex(), newTree.Hex())
	return nil
}

// proposeEdit is one path the proposal will write or delete. Renames are split
// into a delete of the old path and a write of the new.
type proposeEdit struct {
	path string
	del  bool
}

// selectEdits turns a working-tree diff into the set of edits to propose,
// applying the §A2 reconciliation: a change outside the declared write set is a
// refusal (not a truncated proposal); explicit paths narrow the observed set but
// can never name a path that was not observed; an empty result is a distinct
// error, never a silent empty success.
func selectEdits(d worktree.TreeDiff, scopes trust.ScopeSet, paths []string) ([]proposeEdit, error) {
	var edits []proposeEdit
	add := func(ps []string, del bool) {
		for _, p := range ps {
			edits = append(edits, proposeEdit{p, del})
		}
	}
	add(d.Added, false)
	add(d.Modified, false)
	add(d.ModeChanged, false)
	add(d.Removed, true)
	for _, rn := range d.Renamed {
		edits = append(edits, proposeEdit{rn.From, true}, proposeEdit{rn.To, false})
	}

	var outside []string
	for _, e := range edits {
		if !scopes.Covers(e.path) {
			outside = append(outside, e.path)
		}
	}
	if len(outside) > 0 {
		sort.Strings(outside)
		return nil, fmt.Errorf("propose: %d path(s) changed outside the declared scope %s: %s\n"+
			"widen --scope to include them (a scope boundary is a decision with an author), or revert them",
			len(outside), scopes.String(), strings.Join(outside, ", "))
	}

	if len(paths) > 0 {
		observed := map[string]bool{}
		for _, e := range edits {
			observed[e.path] = true
		}
		want := map[string]bool{}
		for _, p := range paths {
			if !observed[p] {
				return nil, fmt.Errorf("propose: %q was named but is not among the changed paths; nothing to propose for it", p)
			}
			want[p] = true
		}
		kept := edits[:0]
		for _, e := range edits {
			if want[e.path] {
				kept = append(kept, e)
			}
		}
		edits = kept
	}

	if len(edits) == 0 {
		return nil, fmt.Errorf("propose: nothing to propose — the working tree matches the base within scope %s", scopes.String())
	}
	return edits, nil
}

// proposeTask is the speculation-pool task a CLI (non-gate) proposal is recorded
// under. Gate proposals use the ephemeral task id; local ones share this bucket.
const proposeTask = "local"

func errUsagePropose() error {
	return fmt.Errorf("usage: varvig propose -m <msg> [--reasoning R] [--scope S ...] [--quiet] [paths...]")
}

// baseChangeAndTree resolves the current HEAD to its change id and tree, or
// (nil, nil) for an unborn HEAD.
func baseChangeAndTree(r *repo.Repo) (multihash.Multihash, multihash.Multihash, error) {
	headRef, err := r.Head()
	if err != nil {
		return nil, nil, nil
	}
	head, err := r.Refs.Resolve(headRef)
	if err != nil {
		return nil, nil, nil
	}
	tree, err := treeOf(r, head)
	if err != nil {
		return nil, nil, err
	}
	return head, tree, nil
}
