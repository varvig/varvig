package txn

import (
	"context"
	"errors"
	"testing"

	"github.com/varvig/varvig/varvig/internal/multihash"
	"github.com/varvig/varvig/varvig/internal/object"
	"github.com/varvig/varvig/varvig/internal/repo"
)

// tip resolves the current main tip, or nil if the ref does not exist.
func tip(t *testing.T, r *repo.Repo) multihash.Multihash {
	t.Helper()
	id, err := r.Refs.Resolve(mainRef)
	if err != nil {
		return nil
	}
	return id
}

// proposalFrom builds a proposal change against base (a change id, or nil for a
// root proposal): it flattens base's tree, applies writes and removes, and
// records the result as a change whose single parent is base. This is what an
// executor emits — a change generated against the base it saw — stored but not
// landed.
func proposalFrom(t *testing.T, r *repo.Repo, base multihash.Multihash, writes map[string]string, removes ...string) multihash.Multihash {
	t.Helper()
	baseTree, err := changeTree(r.Objects, base)
	if err != nil {
		t.Fatalf("changeTree: %v", err)
	}
	files, err := flatten(r.Objects, baseTree)
	if err != nil {
		t.Fatalf("flatten base: %v", err)
	}
	nf := map[string]fileEnt{}
	for k, v := range files {
		nf[k] = v
	}
	for p, c := range writes {
		id, err := r.Objects.Put(object.NewBlob([]byte(c)))
		if err != nil {
			t.Fatalf("put blob: %v", err)
		}
		nf[p] = fileEnt{ID: id, Mode: modeFile}
	}
	for _, p := range removes {
		delete(nf, p)
	}
	tree, err := buildTree(r.Objects, nf)
	if err != nil {
		t.Fatalf("buildTree: %v", err)
	}
	var parents []multihash.Multihash
	if base != nil {
		parents = []multihash.Multihash{base}
	}
	ch := object.NewChange(object.Change{Tree: tree, Parents: parents, Message: "proposal", Author: "test"})
	id, err := r.Objects.Put(ch)
	if err != nil {
		t.Fatalf("put change: %v", err)
	}
	return id
}

// TestLandFastForward is the base case: a proposal generated against the current
// tip, changing one path, lands and advances the ref — and touches nothing else.
func TestLandFastForward(t *testing.T) {
	r := newRepo(t)
	seed(t, r, "a", "a1")
	seed(t, r, "b", "b1")
	before := tip(t, r)

	p := proposalFrom(t, r, before, map[string]string{"a": "a2"})
	res, err := LandProposal(context.Background(), r, mainRef, p)
	if err != nil {
		t.Fatalf("LandProposal: %v", err)
	}
	if res.NoOp {
		t.Fatal("a real change landed as a no-op")
	}
	if got, ok := readFile(t, r, "a"); !ok || got != "a2" {
		t.Fatalf("a = %q,%v; want a2", got, ok)
	}
	if got, ok := readFile(t, r, "b"); !ok || got != "b1" {
		t.Fatalf("b = %q,%v; want b1 untouched", got, ok)
	}
	if tip(t, r).Equal(before) {
		t.Fatal("ref did not advance")
	}
}

// TestLandDisjointProposalsBothLand lands two proposals generated against the
// *same* base but touching disjoint paths. The second was generated before the
// first landed, so its base is stale — yet it must land, because its delta is
// disjoint from what moved. A whole-tree apply would make the second revert the
// first; a delta apply does not.
func TestLandDisjointProposalsBothLand(t *testing.T) {
	r := newRepo(t)
	seed(t, r, "a", "a1")
	seed(t, r, "b", "b1")
	base := tip(t, r)

	p1 := proposalFrom(t, r, base, map[string]string{"a": "a2"})
	p2 := proposalFrom(t, r, base, map[string]string{"b": "b2"}) // same stale base

	if _, err := LandProposal(context.Background(), r, mainRef, p1); err != nil {
		t.Fatalf("land p1: %v", err)
	}
	if _, err := LandProposal(context.Background(), r, mainRef, p2); err != nil {
		t.Fatalf("land p2 (stale base, disjoint path): %v", err)
	}
	if got, _ := readFile(t, r, "a"); got != "a2" {
		t.Fatalf("a = %q; p2 reverted p1's change", got)
	}
	if got, _ := readFile(t, r, "b"); got != "b2" {
		t.Fatalf("b = %q; want b2", got)
	}
}

// TestLandConvergentIsNoOp lands a proposal twice. The second time the tip
// already holds what the proposal wants, so it is a no-op, not a conflict and
// not a redundant commit.
func TestLandConvergentIsNoOp(t *testing.T) {
	r := newRepo(t)
	seed(t, r, "a", "a1")
	base := tip(t, r)
	p := proposalFrom(t, r, base, map[string]string{"a": "a2"})

	if _, err := LandProposal(context.Background(), r, mainRef, p); err != nil {
		t.Fatalf("first land: %v", err)
	}
	afterFirst := historyLen(t, r)

	res, err := LandProposal(context.Background(), r, mainRef, p)
	if err != nil {
		t.Fatalf("second land: %v", err)
	}
	if !res.NoOp {
		t.Fatalf("re-landing an applied proposal was not a no-op: %+v", res)
	}
	if historyLen(t, r) != afterFirst {
		t.Fatal("a convergent land added a commit")
	}
}

// TestLandRefusesGenuineConflict is the refuse-first floor: the tip changed a
// path the proposal also changed, to a third value. Landing must refuse with
// ErrLandConflict and leave the ref untouched — never silently drop either the
// proposal's work or the concurrent change's.
func TestLandRefusesGenuineConflict(t *testing.T) {
	r := newRepo(t)
	seed(t, r, "a", "a1")
	base := tip(t, r)
	p := proposalFrom(t, r, base, map[string]string{"a": "a2"}) // base a1 -> a2

	seed(t, r, "a", "a3") // a concurrent change moves a to a third value
	tipBefore := tip(t, r)

	res, err := LandProposal(context.Background(), r, mainRef, p)
	if !errors.Is(err, ErrLandConflict) {
		t.Fatalf("err = %v; want ErrLandConflict", err)
	}
	if !errors.Is(res.Err, ErrLandConflict) {
		t.Fatalf("res.Err = %v; want ErrLandConflict", res.Err)
	}
	if !tip(t, r).Equal(tipBefore) {
		t.Fatal("ref advanced despite a refused land")
	}
	if got, _ := readFile(t, r, "a"); got != "a3" {
		t.Fatalf("a = %q; the concurrent change was clobbered", got)
	}
}

// TestLandRemoval lands a proposal that removes a path.
func TestLandRemoval(t *testing.T) {
	r := newRepo(t)
	seed(t, r, "a", "a1")
	seed(t, r, "b", "b1")
	base := tip(t, r)

	p := proposalFrom(t, r, base, nil, "a")
	if _, err := LandProposal(context.Background(), r, mainRef, p); err != nil {
		t.Fatalf("land removal: %v", err)
	}
	if _, ok := readFile(t, r, "a"); ok {
		t.Fatal("a was not removed")
	}
	if got, ok := readFile(t, r, "b"); !ok || got != "b1" {
		t.Fatalf("b = %q,%v; want b1 untouched", got, ok)
	}
}

// TestLandRootProposalMergesAdditively lands a parentless proposal onto a
// non-empty tip. Its base is empty, so every path it carries is an addition; a
// disjoint path already on the tip survives.
func TestLandRootProposalMergesAdditively(t *testing.T) {
	r := newRepo(t)
	seed(t, r, "b", "b1")

	p := proposalFrom(t, r, nil, map[string]string{"a": "a1"})
	if _, err := LandProposal(context.Background(), r, mainRef, p); err != nil {
		t.Fatalf("land root proposal: %v", err)
	}
	if got, ok := readFile(t, r, "a"); !ok || got != "a1" {
		t.Fatalf("a = %q,%v; want a1 added", got, ok)
	}
	if got, ok := readFile(t, r, "b"); !ok || got != "b1" {
		t.Fatalf("b = %q,%v; want b1 to survive", got, ok)
	}
}

// TestLandRejectsNonChange rejects an id that is not a change: landing's input
// is a proposal, and a blob is a caller mistake worth naming.
func TestLandRejectsNonChange(t *testing.T) {
	r := newRepo(t)
	id, err := r.Objects.Put(object.NewBlob([]byte("not a change")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LandProposal(context.Background(), r, mainRef, id); !errors.Is(err, ErrNotAProposal) {
		t.Fatalf("err = %v; want ErrNotAProposal", err)
	}
}
