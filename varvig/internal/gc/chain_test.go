package gc

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/core"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/spec"
)

// The F4 retention acceptance: "a proposal carrying its chain forward retains it
// under GC; one that does not, does not." The task-local commit chain is §1.6's
// bottom mip level — raw operations beneath the summarized proposal. Whether it
// survives is decided by what the proposal points its parent at: the chain tip
// (carry forward) or the base (summarized only). GC follows change parents, so
// the retention difference falls out of that one choice.

func key(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

// deleted reports whether GC's dry-run report would reclaim target.
func deleted(ids []multihash.Multihash, target multihash.Multihash) bool {
	for _, id := range ids {
		if id.Equal(target) {
			return true
		}
	}
	return false
}

func TestCarryChainForwardRetainsChain(t *testing.T) {
	r := newRepo(t)
	// A task-local commit chain base -> c1 -> c2, on no ref (it lives only in the
	// checkout's history; here, only whatever references it keeps it alive).
	base, _, _ := mkChange(t, r, "base", nil)
	c1, _, _ := mkChange(t, r, "c1", base)
	c2, c2tree, _ := mkChange(t, r, "c2", c1)

	// A proposal that carries the chain forward: its parent is the chain tip, so
	// base, c1, c2 are all in the proposal's closure.
	res, err := core.Propose(r, core.CLICapabilities(), core.ProposeParams{
		Base: base, ChainTip: c2, Tree: c2tree, Message: "carry", Author: "a",
		Signer: key(t), SpecTask: "task", Now: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Parents[0].Equal(c2) {
		t.Fatalf("carry-forward proposal parent = %s, want chain tip %s", res.Parents[0].Hex(), c2.Hex())
	}

	rep, err := Collect(r, spec.Open(r.GitDir()), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []multihash.Multihash{base, c1, c2} {
		if deleted(rep.DeletedIDs, id) {
			t.Fatalf("carried-forward chain member %s marked for collection", id.Hex()[4:16])
		}
	}
}

func TestNoCarryDropsChain(t *testing.T) {
	r := newRepo(t)
	base, _, _ := mkChange(t, r, "base", nil)
	c1, _, _ := mkChange(t, r, "c1", base)
	c2, c2tree, _ := mkChange(t, r, "c2", c1)

	// A summarized proposal that does not carry the chain: its parent is the base,
	// so c1 and c2 are referenced by nothing and become collectible.
	res, err := core.Propose(r, core.CLICapabilities(), core.ProposeParams{
		Base: base, Tree: c2tree, Message: "summary", Author: "a",
		Signer: key(t), SpecTask: "task", Now: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Parents[0].Equal(base) {
		t.Fatalf("summarized proposal parent = %s, want base %s", res.Parents[0].Hex(), base.Hex())
	}

	rep, err := Collect(r, spec.Open(r.GitDir()), true)
	if err != nil {
		t.Fatal(err)
	}
	// The base stays (the proposal points at it); the intermediate chain does not.
	if deleted(rep.DeletedIDs, base) {
		t.Fatal("base marked for collection though the proposal points at it")
	}
	for _, id := range []multihash.Multihash{c1, c2} {
		if !deleted(rep.DeletedIDs, id) {
			t.Fatalf("uncarried chain member %s survived though nothing references it", id.Hex()[4:16])
		}
	}
}
