package merge

import (
	"context"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

func newRepo(t *testing.T) *repo.Repo {
	t.Helper()
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return r
}

// change builds a change with the given files and parent, optionally carrying
// an intent (provenance with a TaskSpec) so regeneration can be exercised.
func change(t *testing.T, objs ObjectStore, parent multihash.Multihash, files map[string]string, intent string) multihash.Multihash {
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
	ch := object.Change{Tree: tree, Message: "c"}
	if parent != nil {
		ch.Parents = []multihash.Multihash{parent}
	}
	if intent != "" {
		provID, err := objs.Put(object.NewProvenance(object.Provenance{TaskSpec: intent, Model: "test"}))
		if err != nil {
			t.Fatalf("put prov: %v", err)
		}
		ch.Provenance = provID
	}
	id, err := objs.Put(object.NewChange(ch))
	if err != nil {
		t.Fatalf("put change: %v", err)
	}
	return id
}

func readMerged(t *testing.T, objs ObjectStore, tree multihash.Multihash, path string) (string, bool) {
	t.Helper()
	flat, err := flatten(objs, tree)
	if err != nil {
		t.Fatalf("flatten: %v", err)
	}
	fe, ok := flat[path]
	if !ok {
		return "", false
	}
	c, err := content(objs, fe, true)
	if err != nil {
		t.Fatalf("content: %v", err)
	}
	return string(c), true
}

func TestMergeDisjointFiles(t *testing.T) {
	r := newRepo(t)
	objs := r.Objects
	base := change(t, objs, nil, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, "")
	ours := change(t, objs, base, map[string]string{"a.txt": "a-ours\n", "b.txt": "b\n"}, "")
	theirs := change(t, objs, base, map[string]string{"a.txt": "a\n", "b.txt": "b-theirs\n"}, "")

	res, err := Merge(context.Background(), objs, ours, theirs, nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !res.Resolved() {
		t.Fatalf("unexpected conflicts: %+v", res.Conflicts)
	}
	if v, _ := readMerged(t, objs, res.Tree, "a.txt"); v != "a-ours\n" {
		t.Fatalf("a.txt = %q", v)
	}
	if v, _ := readMerged(t, objs, res.Tree, "b.txt"); v != "b-theirs\n" {
		t.Fatalf("b.txt = %q", v)
	}
	if res.Change == nil {
		t.Fatal("resolved merge produced no change")
	}
}

func TestMergeTextualSameFile(t *testing.T) {
	r := newRepo(t)
	objs := r.Objects
	base := change(t, objs, nil, map[string]string{"f": "l1\nl2\nl3\n"}, "")
	ours := change(t, objs, base, map[string]string{"f": "L1\nl2\nl3\n"}, "")   // first line
	theirs := change(t, objs, base, map[string]string{"f": "l1\nl2\nL3\n"}, "") // last line

	res, err := Merge(context.Background(), objs, ours, theirs, nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !res.Resolved() {
		t.Fatalf("unexpected conflict: %+v", res.Conflicts)
	}
	if v, _ := readMerged(t, objs, res.Tree, "f"); v != "L1\nl2\nL3\n" {
		t.Fatalf("f = %q", v)
	}
	if len(res.TextMerged) != 1 {
		t.Fatalf("TextMerged = %v", res.TextMerged)
	}
}

func TestMergeConflictNoRegenerator(t *testing.T) {
	r := newRepo(t)
	objs := r.Objects
	base := change(t, objs, nil, map[string]string{"f": "x\n"}, "")
	ours := change(t, objs, base, map[string]string{"f": "ours\n"}, "")
	theirs := change(t, objs, base, map[string]string{"f": "theirs\n"}, "")

	res, err := Merge(context.Background(), objs, ours, theirs, nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Resolved() {
		t.Fatal("expected an unresolved conflict")
	}
	if res.Change != nil {
		t.Fatal("conflicted merge must not produce a change")
	}
	v, _ := readMerged(t, objs, res.Tree, "f")
	if !containsAll(v, "ours\n", "theirs\n", markerOurs) {
		t.Fatalf("conflict markers missing: %q", v)
	}
}

// fakeRegen resolves any conflict deterministically from the incoming intent,
// standing in for an out-of-process model (design §3.3).
type fakeRegen struct{ called int }

func (f *fakeRegen) Regenerate(_ context.Context, req RegenRequest) ([]byte, error) {
	f.called++
	return []byte("REGEN:" + req.Intent.TaskSpec + "\n"), nil
}

func TestMergeConflictRegenerated(t *testing.T) {
	r := newRepo(t)
	objs := r.Objects
	base := change(t, objs, nil, map[string]string{"f": "x\n"}, "")
	ours := change(t, objs, base, map[string]string{"f": "ours\n"}, "")
	// theirs carries an intent, so regeneration can re-run it against ours.
	theirs := change(t, objs, base, map[string]string{"f": "theirs\n"}, "make f great")

	reg := &fakeRegen{}
	res, err := Merge(context.Background(), objs, ours, theirs, reg)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !res.Resolved() {
		t.Fatalf("regeneration should have resolved the conflict: %+v", res.Conflicts)
	}
	if reg.called == 0 {
		t.Fatal("regenerator was not invoked")
	}
	if len(res.Regenerated) != 1 {
		t.Fatalf("Regenerated = %v", res.Regenerated)
	}
	if v, _ := readMerged(t, objs, res.Tree, "f"); v != "REGEN:make f great\n" {
		t.Fatalf("regenerated content = %q", v)
	}
}

func TestMergeDeleteVsUnchanged(t *testing.T) {
	r := newRepo(t)
	objs := r.Objects
	base := change(t, objs, nil, map[string]string{"keep": "k\n", "drop": "d\n"}, "")
	ours := change(t, objs, base, map[string]string{"keep": "k\n"}, "")                  // deleted "drop"
	theirs := change(t, objs, base, map[string]string{"keep": "k\n", "drop": "d\n"}, "") // unchanged

	res, err := Merge(context.Background(), objs, ours, theirs, nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !res.Resolved() {
		t.Fatalf("unexpected conflict: %+v", res.Conflicts)
	}
	if _, ok := readMerged(t, objs, res.Tree, "drop"); ok {
		t.Fatal("deleted file reappeared after merge")
	}
}

func TestMergeBase(t *testing.T) {
	r := newRepo(t)
	objs := r.Objects
	base := change(t, objs, nil, map[string]string{"f": "0\n"}, "")
	ours := change(t, objs, base, map[string]string{"f": "1\n"}, "")
	theirs := change(t, objs, base, map[string]string{"f": "2\n"}, "")
	got := MergeBase(objs, ours, theirs)
	if got == nil || !got.Equal(base) {
		t.Fatalf("merge base = %v, want %v", got, base)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
