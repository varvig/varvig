// Package affected answers "what does this change actually affect" (design
// §1.3) — the index that later powers semantic conflict detection (§1.3) and
// guided bisect (§2). This first cut is textual + build-graph: it diffs two
// trees to find directly changed files, then walks a file-dependency graph
// (extracted from import/include directives) to their transitive dependents.
//
// It degrades gracefully (design §5): a file whose language has no analyzer
// contributes only itself when it changes — never a false claim of safety.
// Semantic analyzers arrive later as wasm modules (§3.3) without changing this
// interface.
package affected

import (
	"fmt"
	"path"
	"sort"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
)

// ObjectStore is the read surface the index needs.
type ObjectStore interface {
	Get(multihash.Multihash) (*object.Object, error)
}

// FlattenTree returns every file in a tree as path -> blob id, using
// slash-separated paths.
func FlattenTree(objs ObjectStore, treeID multihash.Multihash) (map[string]multihash.Multihash, error) {
	out := map[string]multihash.Multihash{}
	if treeID == nil {
		return out, nil
	}
	if err := walk(objs, treeID, "", out); err != nil {
		return nil, err
	}
	return out, nil
}

func walk(objs ObjectStore, treeID multihash.Multihash, prefix string, out map[string]multihash.Multihash) error {
	obj, err := objs.Get(treeID)
	if err != nil {
		return err
	}
	entries, err := obj.TreeEntries()
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := e.Name
		if prefix != "" {
			p = prefix + "/" + e.Name
		}
		if e.Kind == object.TypeTree {
			if err := walk(objs, e.ID, p, out); err != nil {
				return err
			}
		} else {
			out[p] = e.ID
		}
	}
	return nil
}

// Diff is a path-level change set between two trees.
type Diff struct {
	Added    []string
	Modified []string
	Removed  []string
}

// Changed returns all changed paths (added, modified, and removed), sorted.
func (d Diff) Changed() []string {
	var all []string
	all = append(all, d.Added...)
	all = append(all, d.Modified...)
	all = append(all, d.Removed...)
	sort.Strings(all)
	return all
}

// DiffTrees computes the path-level diff between two trees. Subtrees with equal
// identities are skipped whole — the Merkle-DAG advantage, so unchanged regions
// cost nothing (design §1.3, "in milliseconds").
func DiffTrees(objs ObjectStore, base, new multihash.Multihash) (Diff, error) {
	var d Diff
	if err := diffTrees(objs, base, new, "", &d); err != nil {
		return Diff{}, err
	}
	sort.Strings(d.Added)
	sort.Strings(d.Modified)
	sort.Strings(d.Removed)
	return d, nil
}

func diffTrees(objs ObjectStore, base, new multihash.Multihash, prefix string, d *Diff) error {
	if base != nil && new != nil && base.Equal(new) {
		return nil // identical subtree: prune
	}
	baseEntries, err := entriesOf(objs, base)
	if err != nil {
		return err
	}
	newEntries, err := entriesOf(objs, new)
	if err != nil {
		return err
	}
	bm := indexByName(baseEntries)
	nm := indexByName(newEntries)

	for name, be := range bm {
		p := join(prefix, name)
		ne, ok := nm[name]
		if !ok {
			if err := collect(objs, be, p, &d.Removed); err != nil {
				return err
			}
			continue
		}
		switch {
		case be.Kind == object.TypeTree && ne.Kind == object.TypeTree:
			if !be.ID.Equal(ne.ID) {
				if err := diffTrees(objs, be.ID, ne.ID, p, d); err != nil {
					return err
				}
			}
		case be.Kind == object.TypeBlob && ne.Kind == object.TypeBlob:
			if !be.ID.Equal(ne.ID) {
				d.Modified = append(d.Modified, p)
			}
		default:
			// Kind changed (file <-> directory): treat as remove + add.
			if err := collect(objs, be, p, &d.Removed); err != nil {
				return err
			}
			if err := collect(objs, ne, p, &d.Added); err != nil {
				return err
			}
		}
	}
	for name, ne := range nm {
		if _, ok := bm[name]; ok {
			continue
		}
		if err := collect(objs, ne, join(prefix, name), &d.Added); err != nil {
			return err
		}
	}
	return nil
}

// collect appends the file path(s) under an entry: a blob is one path; a tree
// contributes all its descendant file paths.
func collect(objs ObjectStore, e object.Entry, p string, dst *[]string) error {
	if e.Kind == object.TypeBlob {
		*dst = append(*dst, p)
		return nil
	}
	sub := map[string]multihash.Multihash{}
	if err := walk(objs, e.ID, p, sub); err != nil {
		return err
	}
	for path := range sub {
		*dst = append(*dst, path)
	}
	return nil
}

func entriesOf(objs ObjectStore, id multihash.Multihash) ([]object.Entry, error) {
	if id == nil {
		return nil, nil
	}
	obj, err := objs.Get(id)
	if err != nil {
		return nil, err
	}
	if obj.Type() != object.TypeTree {
		return nil, fmt.Errorf("affected: expected tree, got %s", obj.Type())
	}
	return obj.TreeEntries()
}

func indexByName(entries []object.Entry) map[string]object.Entry {
	m := make(map[string]object.Entry, len(entries))
	for _, e := range entries {
		m[e.Name] = e
	}
	return m
}

func join(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return path.Join(prefix, name)
}
