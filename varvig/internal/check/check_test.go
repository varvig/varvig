package check

import (
	"os/exec"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
)

// requireCmd skips the test when a POSIX helper the test relies on is absent, so
// the suite stays honest on a stripped-down runner rather than failing spuriously.
func requireCmd(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%q not on PATH; skipping", name)
	}
}

func newRepo(t *testing.T) *repo.Repo {
	t.Helper()
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// treeWith stores a one-file tree and a change over it, returning both ids.
func treeWith(t *testing.T, r *repo.Repo, name, content string) (tree, change multihash.Multihash) {
	t.Helper()
	blob, err := r.Objects.Put(object.NewBlob([]byte(content)))
	if err != nil {
		t.Fatal(err)
	}
	tree, err = r.Objects.Put(object.NewTree([]object.Entry{
		{Name: name, Mode: 0o100644, Kind: object.TypeBlob, ID: blob},
	}))
	if err != nil {
		t.Fatal(err)
	}
	change, err = r.Objects.Put(object.NewChange(object.Change{Tree: tree, Message: "m", Timestamp: 1}))
	if err != nil {
		t.Fatal(err)
	}
	return tree, change
}

// TestEvidenceBindsTreeAndGoesStale: evidence records the tree hash it checked
// and is stale against any other tree (build spec P1.3, §A4).
func TestEvidenceBindsTreeAndGoesStale(t *testing.T) {
	requireCmd(t, "true")
	r := newRepo(t)
	treeA, changeA := treeWith(t, r, "f.txt", "a")
	treeB, _ := treeWith(t, r, "f.txt", "b")

	ev, err := Run(r, treeA, changeA, []string{"true"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Tree != treeA.Hex() {
		t.Fatalf("evidence tree = %s, want the checked tree %s", ev.Tree, treeA.Hex())
	}
	if !ev.Fresh(treeA) {
		t.Error("evidence must be fresh against the tree it checked")
	}
	if ev.Fresh(treeB) {
		t.Error("evidence must be stale against a different tree")
	}
}

// TestFailingCommandIsRecorded: a failing command produces recorded evidence of
// failure, not an absent record.
func TestFailingCommandIsRecorded(t *testing.T) {
	requireCmd(t, "false")
	r := newRepo(t)
	tree, change := treeWith(t, r, "f.txt", "a")

	ev, err := Run(r, tree, change, []string{"false"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Passed {
		t.Error("a failing command must not produce a passing record")
	}
	if len(ev.Results) != 1 || ev.Results[0].Exit == 0 {
		t.Fatalf("the failure must be recorded, not omitted: %+v", ev.Results)
	}
}

// TestTwoChecksAreEquivalent: two checks of the identical tree with the identical
// commands produce equivalent evidence (timestamp aside).
func TestTwoChecksAreEquivalent(t *testing.T) {
	requireCmd(t, "true")
	r := newRepo(t)
	tree, change := treeWith(t, r, "f.txt", "a")

	ev1, err := Run(r, tree, change, []string{"true"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	ev2, err := Run(r, tree, change, []string{"true"}, 999) // different clock
	if err != nil {
		t.Fatal(err)
	}
	if !ev1.Equivalent(ev2) {
		t.Fatalf("two checks of the same tree must be equivalent:\n%+v\n%+v", ev1, ev2)
	}
}

// TestEvidenceIsAReservedReplicatingNote: evidence is stored as a note in a
// reserved namespace, so it is retrievable and always replicates to peers.
func TestEvidenceIsAReservedReplicatingNote(t *testing.T) {
	requireCmd(t, "true")
	if !reserved.IsReservedNoteNamespace(reserved.NoteCheck) {
		t.Fatal("check evidence namespace must be reserved so it always syncs to peers")
	}
	r := newRepo(t)
	tree, change := treeWith(t, r, "f.txt", "a")
	ev, err := Run(r, tree, change, []string{"true"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Attach(r, ev); err != nil {
		t.Fatal(err)
	}
	got, err := List(r, change)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Tree != tree.Hex() {
		t.Fatalf("stored evidence not retrievable: %+v", got)
	}
}

// TestPromotionStaleIsAbsent: promotion classification returns FreshPass for
// evidence matching the current tree and Stale (never a pass) after an edit.
func TestPromotionStaleIsAbsent(t *testing.T) {
	requireCmd(t, "true")
	r := newRepo(t)
	treeA, change := treeWith(t, r, "f.txt", "a")
	treeB, _ := treeWith(t, r, "f.txt", "b")

	ev, err := Run(r, treeA, change, []string{"true"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Attach(r, ev); err != nil {
		t.Fatal(err)
	}

	// Fresh against the tree it checked.
	if st, _, err := Promotion(r, change, treeA); err != nil || st != FreshPass {
		t.Fatalf("Promotion over checked tree = %v (err %v), want FreshPass", st, err)
	}
	// After an edit (a different current tree), the same evidence is stale and
	// names the tree it was produced against — never counted as a pass.
	st, staleTree, err := Promotion(r, change, treeB)
	if err != nil {
		t.Fatal(err)
	}
	if st != Stale {
		t.Fatalf("Promotion over an edited tree = %v, want Stale", st)
	}
	if staleTree != treeA.Hex() {
		t.Errorf("stale tree = %s, want the originally-checked %s", staleTree, treeA.Hex())
	}
}

// TestNoEvidenceIsNotBlocking: an unchecked proposal is NoEvidence, so it is not
// blocked by this guard (evidence is opt-in per proposal).
func TestNoEvidenceIsNotBlocking(t *testing.T) {
	r := newRepo(t)
	tree, change := treeWith(t, r, "f.txt", "a")
	if st, _, err := Promotion(r, change, tree); err != nil || st != NoEvidence {
		t.Fatalf("unchecked proposal = %v (err %v), want NoEvidence", st, err)
	}
}
