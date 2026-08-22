package p2p

import (
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// attempt builds a distinct change on top of base in r and returns its id.
func attempt(t *testing.T, r *repo.Repo, base multihash.Multihash, content string) multihash.Multihash {
	t.Helper()
	blob, _ := r.Objects.Put(object.NewBlob([]byte(content)))
	tree, _ := r.Objects.Put(object.NewTree([]object.Entry{{Name: "out", Mode: 0o100644, Kind: object.TypeBlob, ID: blob}}))
	ch, err := r.Objects.Put(object.NewChange(object.Change{
		Tree: tree, Parents: []multihash.Multihash{base}, Message: content, Timestamp: 9,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

// TestPartitionBothAttemptsSurvive covers §7.8: two peers claim the same task
// while partitioned, each CASes its own attempt ref locally (both succeed), and
// on reconnect both attempts exist and neither is lost. Attempts are namespaced
// by convention (refs/attempts/<task>/<peer>), so concurrent claims converge
// rather than clobber. This is correct behaviour; the test exists to keep it so.
func TestPartitionBothAttemptsSurvive(t *testing.T) {
	// Peer A is the server, seeded with a shared base c1.
	a, base, _ := seedServer(t)

	// --- partitioned: each peer independently CASes its own attempt ---
	chA := attempt(t, a, base, "attempt-from-A")
	if err := a.Refs.CompareAndSwap("refs/attempts/T/peerA", nil, chA, "peerA", "claim"); err != nil {
		t.Fatalf("A local CAS: %v", err)
	}

	b, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := dialServe(t, a)
	if err := client.Fetch(b.Objects, []multihash.Multihash{base}, nil); err != nil {
		t.Fatalf("B fetch base: %v", err)
	}
	chB := attempt(t, b, base, "attempt-from-B")
	if err := b.Refs.CompareAndSwap("refs/attempts/T/peerB", nil, chB, "peerB", "claim"); err != nil {
		t.Fatalf("B local CAS: %v", err)
	}

	// --- reconnect: exchange attempts ---
	// B pushes its attempt to A (distinct ref name → no conflict).
	if err := client.Push(b.Objects, "refs/attempts/T/peerB", nil, chB); err != nil {
		t.Fatalf("B push attempt: %v", err)
	}
	// B pulls A's attempt and records it locally.
	if err := client.Fetch(b.Objects, []multihash.Multihash{chA}, []multihash.Multihash{base}); err != nil {
		t.Fatalf("B fetch A's attempt: %v", err)
	}
	if err := b.Refs.CompareAndSwap("refs/attempts/T/peerA", nil, chA, "peerB", "mirror"); err != nil {
		t.Fatalf("B mirror A's attempt: %v", err)
	}

	// Both peers now hold both attempts; neither was lost.
	for _, r := range []*repo.Repo{a, b} {
		if !r.Objects.Has(chA) || !r.Objects.Has(chB) {
			t.Fatal("an attempt object was lost across the partition")
		}
		gotA, errA := r.Refs.Resolve("refs/attempts/T/peerA")
		gotB, errB := r.Refs.Resolve("refs/attempts/T/peerB")
		if errA != nil || errB != nil || !gotA.Equal(chA) || !gotB.Equal(chB) {
			t.Fatalf("both attempt refs must survive on every peer (A=%v B=%v)", errA, errB)
		}
	}
}
