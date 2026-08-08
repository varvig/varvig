package affected

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// treeFromFiles writes files into a fresh working tree and returns the repo and
// the resulting tree id.
func treeFromFiles(t *testing.T, files map[string]string) (*repo.Repo, multihash.Multihash) {
	t.Helper()
	dir := t.TempDir()
	r, err := repo.Init(dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tree, err := worktree.WriteTree(r.Objects, dir)
	if err != nil {
		t.Fatalf("WriteTree: %v", err)
	}
	return r, tree
}

func writeTree(t *testing.T, r *repo.Repo, dir string, files map[string]string) multihash.Multihash {
	t.Helper()
	// Rewrite dir contents from scratch.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() == ".varvig" {
			continue
		}
		os.RemoveAll(filepath.Join(dir, e.Name()))
	}
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tree, err := worktree.WriteTree(r.Objects, dir)
	if err != nil {
		t.Fatalf("WriteTree: %v", err)
	}
	return tree
}

func TestFlattenTree(t *testing.T) {
	r, tree := treeFromFiles(t, map[string]string{
		"a.txt":        "a",
		"dir/b.txt":    "b",
		"dir/sub/c.go": "c",
	})
	files, err := FlattenTree(r.Objects, tree)
	if err != nil {
		t.Fatalf("FlattenTree: %v", err)
	}
	want := []string{"a.txt", "dir/b.txt", "dir/sub/c.go"}
	for _, p := range want {
		if _, ok := files[p]; !ok {
			t.Errorf("missing %s in %v", p, files)
		}
	}
	if len(files) != 3 {
		t.Fatalf("files = %d, want 3", len(files))
	}
}

func TestDiffTrees(t *testing.T) {
	dir := t.TempDir()
	r, err := repo.Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	base := writeTree(t, r, dir, map[string]string{
		"keep.txt":     "same",
		"change.txt":   "v1",
		"gone.txt":     "bye",
		"pkg/deep.txt": "d1",
	})
	newT := writeTree(t, r, dir, map[string]string{
		"keep.txt":     "same", // unchanged
		"change.txt":   "v2",   // modified
		"added.txt":    "hi",   // added
		"pkg/deep.txt": "d1",   // unchanged (subtree pruned)
	})
	d, err := DiffTrees(r.Objects, base, newT)
	if err != nil {
		t.Fatalf("DiffTrees: %v", err)
	}
	if !reflect.DeepEqual(d.Added, []string{"added.txt"}) {
		t.Errorf("added = %v", d.Added)
	}
	if !reflect.DeepEqual(d.Modified, []string{"change.txt"}) {
		t.Errorf("modified = %v", d.Modified)
	}
	if !reflect.DeepEqual(d.Removed, []string{"gone.txt"}) {
		t.Errorf("removed = %v", d.Removed)
	}
}

func TestDiffIdenticalTreesEmpty(t *testing.T) {
	r, tree := treeFromFiles(t, map[string]string{"a.txt": "x"})
	d, err := DiffTrees(r.Objects, tree, tree)
	if err != nil {
		t.Fatalf("DiffTrees: %v", err)
	}
	if len(d.Changed()) != 0 {
		t.Fatalf("changed = %v, want empty", d.Changed())
	}
}

// TestAffectedTransitiveDependents builds a -> b -> c import chain and an
// unrelated file, and checks that changing the leaf pulls in its dependents.
func TestAffectedTransitiveDependents(t *testing.T) {
	r, tree := treeFromFiles(t, map[string]string{
		"src/a.ts":         `import { x } from "./b";`,
		"src/b.ts":         `import "./c";`,
		"src/c.ts":         `export const c = 1;`,
		"src/unrelated.ts": `import "react";`, // external, no repo edge
	})
	g, err := BuildGraph(r.Objects, tree, Options{Cache: NewMemCache()})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	// Edges resolved.
	if got := g.Deps("src/a.ts"); !reflect.DeepEqual(got, []string{"src/b.ts"}) {
		t.Fatalf("a deps = %v, want [src/b.ts]", got)
	}
	if got := g.Deps("src/b.ts"); !reflect.DeepEqual(got, []string{"src/c.ts"}) {
		t.Fatalf("b deps = %v, want [src/c.ts]", got)
	}
	// Changing the leaf affects the whole chain, not the unrelated file.
	got := g.Affected([]string{"src/c.ts"})
	want := []string{"src/a.ts", "src/b.ts", "src/c.ts"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("affected = %v, want %v", got, want)
	}
	if contains(got, "src/unrelated.ts") {
		t.Fatal("unrelated file wrongly reported as affected")
	}
}

// TestGracefulDegradation: a language with no analyzer contributes only itself.
func TestGracefulDegradation(t *testing.T) {
	r, tree := treeFromFiles(t, map[string]string{
		"main.go":  "package main",
		"other.go": "package main",
	})
	g, err := BuildGraph(r.Objects, tree, Options{})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	got := g.Affected([]string{"main.go"})
	if !reflect.DeepEqual(got, []string{"main.go"}) {
		t.Fatalf("affected = %v, want [main.go] (no false edges)", got)
	}
}

func TestDiskCacheRoundTripAndReuse(t *testing.T) {
	cache, err := NewDiskCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	id, _ := multihash.Sum(multihash.BLAKE3, []byte("blob"))
	if _, ok := cache.Get(id); ok {
		t.Fatal("empty cache reported a hit")
	}
	cache.Put(id, []string{"./a", "./b"})
	got, ok := cache.Get(id)
	if !ok || !reflect.DeepEqual(got, []string{"./a", "./b"}) {
		t.Fatalf("cache round-trip: %v ok=%v", got, ok)
	}
	// An analyzed-but-empty result is remembered as a hit (not re-analyzed).
	empty, _ := multihash.Sum(multihash.BLAKE3, []byte("noimports"))
	cache.Put(empty, nil)
	if _, ok := cache.Get(empty); !ok {
		t.Fatal("empty specifier list not cached as a hit")
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
