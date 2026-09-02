package merge

import (
	"context"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
)

// changeFulfilling builds a change like the test helper `change`, but carrying a
// Fulfills link (the intent revision it materializes).
func changeFulfilling(t *testing.T, objs ObjectStore, parent multihash.Multihash, files map[string]string, fulfills multihash.Multihash) multihash.Multihash {
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
	ch := object.Change{Tree: tree, Message: "c", Fulfills: fulfills}
	if parent != nil {
		ch.Parents = []multihash.Multihash{parent}
	}
	id, err := objs.Put(object.NewChange(ch))
	if err != nil {
		t.Fatalf("put change: %v", err)
	}
	return id
}

// TestMergeCarriesFulfills: regeneration re-materializes the incoming change's
// intent, so the merged change must carry that change's ticket→commit link
// forward — dropping it would silently orphan the audit link (C2).
func TestMergeCarriesFulfills(t *testing.T) {
	r := newRepo(t)
	objs := r.Objects
	rev := mustHash(t, "approved-intent-revision")

	base := change(t, objs, nil, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, "")
	ours := change(t, objs, base, map[string]string{"a.txt": "a-ours\n", "b.txt": "b\n"}, "")
	theirs := changeFulfilling(t, objs, base, map[string]string{"a.txt": "a\n", "b.txt": "b-theirs\n"}, rev)

	res, err := Merge(context.Background(), objs, ours, theirs, nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !res.Resolved() || res.Change == nil {
		t.Fatalf("merge did not resolve to a change: %+v", res)
	}
	obj, err := objs.Get(res.Change)
	if err != nil {
		t.Fatalf("get merge change: %v", err)
	}
	c, err := obj.AsChange()
	if err != nil {
		t.Fatalf("AsChange: %v", err)
	}
	if c.Fulfills == nil || !c.Fulfills.Equal(rev) {
		t.Fatalf("merged change Fulfills = %v, want %s", c.Fulfills, rev.Hex())
	}
}

func mustHash(t *testing.T, s string) multihash.Multihash {
	t.Helper()
	h, err := multihash.Sum(multihash.SHA2_256, []byte(s))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return h
}
