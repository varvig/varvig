package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/core"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
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
// Inside a task checkout it measures the write set against the task base and, by
// default, proposes one summarized change on that base — the task-local commit
// chain does not survive. --carry-chain instead parents the proposal on HEAD (the
// chain tip) so the whole chain is carried forward and retained under GC (F4).
//
//	varvig propose -m <msg> [--reasoning R] [--scope S ...] [--carry-chain] [--quiet] [paths...]
func cmdPropose(args []string) error {
	var msg, reasoning, scope string
	var paths []string
	quiet := false
	carryChain := false
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
		case "--carry-chain":
			carryChain = true
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
	marker, inCheckout, err := core.ReadTaskMarker(r)
	if err != nil {
		return err
	}
	// Inside a task checkout, a proposal defaults to the task's granted scope (the
	// marker sealed at `task start`) when the caller did not narrow it explicitly,
	// so the write-set filter and the recorded provenance scope match what the
	// scheduler granted (design addendum, F4). An explicit --scope still applies.
	if scope == "/" && inCheckout && marker.Scope != "" {
		scope = marker.Scope
	}
	scopes := trust.NewScopeSet(scope)

	// Choose the diff baseline and the proposal's parent (design addendum, F4).
	// Outside a checkout the base is HEAD, as always. Inside one, the write set is
	// measured against the *task base* (what the scheduler granted from), and the
	// proposal is a single summarized change on that base by default — the
	// task-local commit chain does not survive. With --carry-chain the parent is
	// instead HEAD, the chain tip, so the whole chain is carried forward and
	// retained under GC.
	baseChange, baseTreeID, err := baseChangeAndTree(r)
	if err != nil {
		return err
	}
	var chainTip multihash.Multihash
	if inCheckout {
		if carryChain {
			chainTip = baseChange // HEAD == the task-local chain tip
		}
		if marker.Base != "" {
			taskBase, err := multihash.ParseHex(marker.Base)
			if err != nil {
				return fmt.Errorf("propose: corrupt task base in marker: %w", err)
			}
			taskTree, err := treeOf(r, taskBase)
			if err != nil {
				return err
			}
			baseChange, baseTreeID = taskBase, taskTree
		}
	} else if carryChain {
		return fmt.Errorf("propose: --carry-chain is only meaningful inside a task checkout")
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

	edits, err := worktree.SelectEdits(d, scopes.Covers, scopes.String(), paths)
	if err != nil {
		return fmt.Errorf("propose: %w\n"+
			"(a change outside scope needs a wider --scope — a boundary is a decision with an author)", err)
	}

	// Preview (P0.4): show exactly the path set about to be submitted, before it is.
	if !quiet {
		fmt.Printf("proposing %d path(s) within scope %s:\n", len(edits), scopes.String())
		printed := make([]worktree.Edit, len(edits))
		copy(printed, edits)
		sort.Slice(printed, func(i, j int) bool { return printed[i].Path < printed[j].Path })
		for _, e := range printed {
			op := "write"
			if e.Del {
				op = "delete"
			}
			fmt.Printf("  %-7s %s\n", op, e.Path)
		}
	}

	proposed := worktree.Overlay(baseStates, work, edits)
	newTree, err := worktree.BuildTree(r.Objects, proposed)
	if err != nil {
		return fmt.Errorf("propose: build tree: %w", err)
	}

	// Finalize through the shared core: the same provenance attach, sign, store,
	// and pool record the gate uses. It never moves a ref.
	priv, err := provenance.LoadOrCreateIdentity(r.GitDir())
	if err != nil {
		return err
	}
	// In a daemon-minted checkout, sign the proposal as the task through the daemon
	// (F4); standalone checkouts were seeded with the key, so priv already is it.
	var remote core.Signer
	if inCheckout {
		rs, warn, err := taskRemoteSigner(marker)
		if err != nil {
			return err
		}
		if warn != "" {
			fmt.Fprintln(os.Stderr, "warning: "+warn)
		}
		remote = rs
	}
	res, err := core.Propose(r, core.CLICapabilities(), core.ProposeParams{
		Base:         baseChange,
		ChainTip:     chainTip,
		Tree:         newTree,
		Message:      msg,
		Reasoning:    reasoning,
		Author:       author(),
		Scope:        scopes.String(),
		Signer:       priv,
		RemoteSigner: remote,
		SpecTask:     proposeTask,
		Now:          time.Now().Unix(),
	})
	if err != nil {
		return err
	}
	if carryChain {
		fmt.Printf("proposed %s (tree %s; carrying task-local chain forward)\n", res.Change.Hex(), newTree.Hex())
	} else {
		fmt.Printf("proposed %s (tree %s)\n", res.Change.Hex(), newTree.Hex())
	}
	return nil
}

// proposeTask is the speculation-pool task a CLI (non-gate) proposal is recorded
// under. Gate proposals use the ephemeral task id; local ones share this bucket.
const proposeTask = "local"

func errUsagePropose() error {
	return fmt.Errorf("usage: varvig propose -m <msg> [--reasoning R] [--scope S ...] [--carry-chain] [--quiet] [paths...]")
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
