package main

import (
	"strings"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/refs"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// TestAPushToOnePeerDoesNotLeaseAgainstAnother is the bug this file exists for.
//
// With one refs/remotes/origin/<branch>, the lease a push carried was whichever
// peer was fetched from last. Fetch B, push A, and A is told "advance only if
// you are where B was" — which A never is. In a mesh at most one peer could
// accept a head push, and which one depended on fetch order.
func TestAPushToOnePeerDoesNotLeaseAgainstAnother(t *testing.T) {
	peerA, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shared := commit(t, peerA, "refs/heads/main", "what both peers start from")

	peerB, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// B has moved on; A has not.
	if err := peerB.Refs.CompareAndSwap("refs/heads/main", nil, shared, "test", "seed"); err != nil {
		t.Fatal(err)
	}
	bTip := commit(t, peerB, "refs/heads/main", "B went its own way")

	local, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Refs.CompareAndSwap("refs/heads/main", nil, shared, "test", "seed"); err != nil {
		t.Fatal(err)
	}

	// A pass fetches every peer in the set, then pushes. Fetch A first and B
	// second, so a single tracking ref would end the fetch holding B's tip —
	// which is exactly the state that made a push to A impossible.
	if err := fetchFromPeer(dialled(t, peerA), local, "main", "peer-a"); err != nil {
		t.Fatalf("fetch from A: %v", err)
	}
	if err := fetchFromPeer(dialled(t, peerB), local, "main", "peer-b"); err != nil {
		t.Fatalf("fetch from B: %v", err)
	}
	if got := trackedTip(local, "peer-b", "main"); got == nil || !got.Equal(bTip) {
		t.Fatalf("B's record is %v, want %s", got, bTip.Hex())
	}
	// A's record is untouched by the later fetch from B: it is a different peer.
	if got := trackedTip(local, "peer-a", "main"); got == nil || !got.Equal(shared) {
		t.Fatalf("A's record is %v, want %s — fetching from one peer must not rewrite what we know of another",
			got, shared.Hex())
	}

	// Now advance locally and push to A. Under one tracking ref this was
	// refused, because the lease described B.
	ours := commit(t, local, "refs/heads/main", "our work")
	if err := pushToPeer(dialled(t, peerA), local, "main", "peer-a"); err != nil {
		t.Fatalf("push to A carried a lease about A and must be accepted: %v", err)
	}
	got, err := peerA.Refs.Resolve("refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(ours) {
		t.Errorf("A is at %s, want %s", got.Hex(), ours.Hex())
	}
}

// TestALeaseStillGuardsThatPeersOwnMovement: per-peer records must not weaken
// force-with-lease into a force. A peer that moved since we looked still refuses.
func TestALeaseStillGuardsThatPeersOwnMovement(t *testing.T) {
	peer, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shared := commit(t, peer, "refs/heads/main", "where we last saw it")

	local, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Refs.CompareAndSwap("refs/heads/main", nil, shared, "test", "seed"); err != nil {
		t.Fatal(err)
	}
	if err := fetchFromPeer(dialled(t, peer), local, "main", "peer"); err != nil {
		t.Fatal(err)
	}

	// The peer moves without us seeing it.
	moved := commit(t, peer, "refs/heads/main", "someone else pushed")
	commit(t, local, "refs/heads/main", "our work")

	err = pushToPeer(dialled(t, peer), local, "main", "peer")
	if err == nil {
		t.Fatal("a peer that moved since we looked must refuse the push, not be overwritten")
	}
	if !strings.Contains(err.Error(), "head:") {
		t.Errorf("the refusal should be attributed to the head, got: %v", err)
	}
	got, rerr := peer.Refs.Resolve("refs/heads/main")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !got.Equal(moved) {
		t.Errorf("the peer's work was overwritten: %s became %s", moved.Hex(), got.Hex())
	}
}

// TestALegacyCloneStillPushes covers the migration. A repository cloned before
// tracking refs were per peer has only refs/remotes/origin/<branch>, and its
// first push after the upgrade must not be refused for want of a record.
func TestALegacyCloneStillPushes(t *testing.T) {
	peer, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shared := commit(t, peer, "refs/heads/main", "the shared tip")

	local, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Refs.CompareAndSwap("refs/heads/main", nil, shared, "test", "seed"); err != nil {
		t.Fatal(err)
	}
	// Exactly what an old clone left behind: the legacy ref and nothing else.
	if err := local.Refs.CompareAndSwap(legacyTrackingPrefix+"main", nil, shared, "clone", "legacy"); err != nil {
		t.Fatal(err)
	}
	ours := commit(t, local, "refs/heads/main", "our work")

	if err := pushToPeer(dialled(t, peer), local, "main", "peer"); err != nil {
		t.Fatalf("a repository cloned before this change must still push: %v", err)
	}
	got, err := peer.Refs.Resolve("refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(ours) {
		t.Errorf("peer is at %s, want %s", got.Hex(), ours.Hex())
	}
	// And from now on the per-peer record is what is consulted.
	if got := trackedTip(local, "peer", "main"); got == nil || !got.Equal(ours) {
		t.Errorf("the per-peer record was not written after the push: %v", got)
	}
}

// TestPeerSegmentIsOneSafeSegment: these names become file paths, on every
// platform this binary is built for.
func TestPeerSegmentIsOneSafeSegment(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:9418", "[::1]:9418", "peer.example.com:9418",
		"/var/run/varvig.sock", "..", ".", "", "weird\\name", "a/b/c",
	} {
		seg := peerSegment(addr)
		if strings.Contains(seg, "/") {
			t.Errorf("%q produced %q, which is more than one segment", addr, seg)
		}
		if err := refs.ValidName("refs/remotes/" + seg + "/main"); err != nil {
			t.Errorf("%q produced %q, which is not a usable ref name: %v", addr, seg, err)
		}
		// ':' is legal in a ref name but not in a filename on Windows, which
		// this binary cross-compiles for.
		if strings.ContainsAny(seg, `:\`) {
			t.Errorf("%q produced %q, which will not be a filename on every platform", addr, seg)
		}
	}
}

// TestPeerSegmentIsInjective: two peers must never share one record, or a lease
// for one would be sent to the other — the bug this change exists to fix,
// reintroduced through the encoding.
func TestPeerSegmentIsInjective(t *testing.T) {
	seen := map[string]string{}
	for _, addr := range []string{
		"127.0.0.1:9418", "127.0.0.1:9419", "127.0.0.19:418",
		"a:b", "a%3Ab", "a/b", "a%2Fb", "", ".", "..", "%", "%%",
	} {
		seg := peerSegment(addr)
		if prev, ok := seen[seg]; ok {
			t.Errorf("%q and %q both map to %q; a lease for one would be sent to the other", prev, addr, seg)
		}
		seen[seg] = addr
	}
}
