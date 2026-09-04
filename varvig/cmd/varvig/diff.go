package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/core"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// cmdDiff is the local, no-network read that gives an agent an independent view
// of its own change (build spec P0.2) — the single most-requested missing verb.
// With no positional arguments it compares the working tree against the base
// (HEAD); with two it compares arbitrary trees or changes. Textual only.
//
//	varvig diff                    working tree vs base, unified
//	varvig diff --name-only        changed paths only (what propose observes)
//	varvig diff --stat             per-path added/removed counts
//	varvig diff <tree-a> <tree-b>  arbitrary comparison
func cmdDiff(args []string) error {
	var nameOnly, stat bool
	var pos []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--name-only":
			nameOnly = true
		case "--stat":
			stat = true
		default:
			if strings.HasPrefix(a, "--") {
				return fmt.Errorf("diff: unknown flag %q", a)
			}
			pos = append(pos, a)
		}
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}

	base, work, fromWorking, err := diffSides(r, pos)
	if err != nil {
		return err
	}
	// Byte readers for each side; the rendering itself lives in the shared core so
	// the CLI and the gate present an identical diff.
	old := func(p string) ([]byte, bool, error) { return blobBytes(r, base[p].Hash) }
	newer := func(p string) ([]byte, bool, error) {
		if fromWorking {
			return workingBytes(r.Root(), p, work[p].Mode)
		}
		return blobBytes(r, work[p].Hash)
	}
	res := core.Diff(base, work, old, newer)
	if res.Diff.Empty() {
		return nil // no changes: empty output, exit 0
	}

	switch {
	case nameOnly:
		for _, p := range res.Changed {
			fmt.Println(p)
		}
	case stat:
		for _, s := range res.Stat {
			if s.Note != "" {
				fmt.Printf("%s\t+0\t-0 (%s)\n", s.Path, s.Note)
				continue
			}
			fmt.Printf("%s\t+%d\t-%d\n", s.Path, s.Added, s.Removed)
		}
	default:
		fmt.Print(res.Unified)
	}
	return nil
}

// diffSides resolves the two comparison sides from the positional args: none →
// working tree vs base; two → arbitrary trees/changes. fromWorking reports
// whether the "new" side is the live working tree (read from disk) or a stored
// tree.
func diffSides(r *repo.Repo, pos []string) (base, work map[string]worktree.FileState, fromWorking bool, err error) {
	switch len(pos) {
	case 0:
		baseTree, err := baseTree(r)
		if err != nil {
			return nil, nil, false, err
		}
		base, err = worktree.FlattenStates(r.Objects, baseTree)
		if err != nil {
			return nil, nil, false, err
		}
		idx := worktree.OpenIndex(r.GitDir())
		work, err = worktree.Scan(r.Objects, r.Root(), idx)
		if err != nil {
			return nil, nil, false, err
		}
		_ = idx.Save()
		return base, work, true, nil
	case 2:
		ta, err := resolveTree(r, pos[0])
		if err != nil {
			return nil, nil, false, err
		}
		tb, err := resolveTree(r, pos[1])
		if err != nil {
			return nil, nil, false, err
		}
		if base, err = worktree.FlattenStates(r.Objects, ta); err != nil {
			return nil, nil, false, err
		}
		if work, err = worktree.FlattenStates(r.Objects, tb); err != nil {
			return nil, nil, false, err
		}
		return base, work, false, nil
	default:
		return nil, nil, false, fmt.Errorf("usage: varvig diff [--name-only|--stat] [<tree-a> <tree-b>]")
	}
}

// baseTree returns the tree of the current HEAD, or nil for an unborn HEAD.
func baseTree(r *repo.Repo) (multihash.Multihash, error) {
	headRef, err := r.Head()
	if err != nil {
		return nil, nil // no HEAD yet: base is the empty tree
	}
	head, err := r.Refs.Resolve(headRef)
	if err != nil {
		return nil, nil
	}
	return treeOf(r, head)
}

func resolveTree(r *repo.Repo, arg string) (multihash.Multihash, error) {
	id, err := resolve(r, arg)
	if err != nil {
		return nil, fmt.Errorf("diff: cannot resolve %q: %w", arg, err)
	}
	return treeOf(r, id)
}

// blobBytes reads a stored blob's content; ok=false when the hash is nil.
func blobBytes(r *repo.Repo, h multihash.Multihash) ([]byte, bool, error) {
	if h == nil {
		return nil, false, nil
	}
	obj, err := r.Objects.Get(h)
	if err != nil {
		return nil, false, err
	}
	c, ok := obj.BlobContent()
	return c, ok, nil
}

// workingBytes reads a working-tree path's content — a symlink yields its target,
// matching how the scan hashes it.
func workingBytes(root, rel string, mode uint32) ([]byte, bool, error) {
	full := filepath.Join(root, rel)
	if mode == 0o120000 { // symlink
		t, err := os.Readlink(full)
		if err != nil {
			return nil, false, err
		}
		return []byte(t), true, nil
	}
	c, err := os.ReadFile(full)
	if err != nil {
		return nil, false, err
	}
	return c, true, nil
}
