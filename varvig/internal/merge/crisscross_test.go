package merge

import (
	"bytes"
	"context"
	"sort"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
)

// changeP builds a change with arbitrary parents (for merge commits).
func changeP(t *testing.T, objs ObjectStore, parents []multihash.Multihash, msg string, files map[string]string) multihash.Multihash {
	t.Helper()
	ents := map[string]fileEnt{}
	for p, c := range files {
		id, err := objs.Put(object.NewBlob([]byte(c)))
		if err != nil {
			t.Fatalf("put blob: %v", err)
		}
		ents[p] = fileEnt{ID: id, Mode: modeFile}
	}
	tree, err := buildTree(objs, ents)
	if err != nil {
		t.Fatalf("buildTree: %v", err)
	}
	// NewChange canonicalizes (sorts) parents, so two merges with the same
	// parent set must differ by message to be distinct commits.
	id, err := objs.Put(object.NewChange(object.Change{Tree: tree, Parents: parents, Message: msg}))
	if err != nil {
		t.Fatalf("put change: %v", err)
	}
	return id
}

func sortHex(ids []multihash.Multihash) []multihash.Multihash {
	out := append([]multihash.Multihash{}, ids...)
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i], out[j]) < 0 })
	return out
}

// TestMergeBasesCrissCross builds the classic criss-cross and checks that both
// best common ancestors are found — where the old single-ancestor base would
// have returned just one.
func TestMergeBasesCrissCross(t *testing.T) {
	r := newRepo(t)
	objs := r.Objects

	root := change(t, objs, nil, map[string]string{"f": "A\n", "g": "1\n"}, "")
	o1 := change(t, objs, root, map[string]string{"f": "A\n", "g": "1o\n"}, "") // edits g
	t1 := change(t, objs, root, map[string]string{"f": "At\n", "g": "1\n"}, "") // edits f
	m1 := changeP(t, objs, []multihash.Multihash{o1, t1}, "merge-1", map[string]string{"f": "At\n", "g": "1o\n"})
	m2 := changeP(t, objs, []multihash.Multihash{t1, o1}, "merge-2", map[string]string{"f": "At\n", "g": "1o\n"})

	got := sortHex(MergeBases(objs, m1, m2))
	want := sortHex([]multihash.Multihash{o1, t1})
	if len(got) != 2 {
		t.Fatalf("MergeBases = %d ancestors, want 2 (criss-cross)", len(got))
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("MergeBases = %v, want {o1,t1}", got)
		}
	}
	// root is a common ancestor but dominated by o1/t1, so it must be excluded.
	for _, b := range got {
		if b.Equal(root) {
			t.Fatal("root wrongly reported as a best common ancestor")
		}
	}
}

// TestMergeCrissCrossResolves merges the two symmetric merge commits; with a
// recursive virtual base this completes cleanly and preserves both edits.
func TestMergeCrissCrossResolves(t *testing.T) {
	r := newRepo(t)
	objs := r.Objects

	root := change(t, objs, nil, map[string]string{"f": "A\n", "g": "1\n"}, "")
	o1 := change(t, objs, root, map[string]string{"f": "A\n", "g": "1o\n"}, "")
	t1 := change(t, objs, root, map[string]string{"f": "At\n", "g": "1\n"}, "")
	m1 := changeP(t, objs, []multihash.Multihash{o1, t1}, "merge-1", map[string]string{"f": "At\n", "g": "1o\n"})
	m2 := changeP(t, objs, []multihash.Multihash{t1, o1}, "merge-2", map[string]string{"f": "At\n", "g": "1o\n"})

	res, err := Merge(context.Background(), objs, m1, m2, nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !res.Resolved() {
		t.Fatalf("criss-cross merge unresolved: %+v", res.Conflicts)
	}
	if v, _ := readMerged(t, objs, res.Tree, "f"); v != "At\n" {
		t.Fatalf("f = %q, want At", v)
	}
	if v, _ := readMerged(t, objs, res.Tree, "g"); v != "1o\n" {
		t.Fatalf("g = %q, want 1o", v)
	}
	// Two best common ancestors => synthesized virtual base, so Base is nil.
	if res.Base != nil {
		t.Fatal("expected a virtual base (nil Base) for a criss-cross merge")
	}
}

// TestMergeBasesUnrelated: disjoint histories have no common ancestor.
func TestMergeBasesUnrelated(t *testing.T) {
	r := newRepo(t)
	objs := r.Objects
	a := change(t, objs, nil, map[string]string{"a": "1\n"}, "")
	b := change(t, objs, nil, map[string]string{"b": "1\n"}, "")
	if bs := MergeBases(objs, a, b); len(bs) != 0 {
		t.Fatalf("unrelated MergeBases = %v, want none", bs)
	}
}
