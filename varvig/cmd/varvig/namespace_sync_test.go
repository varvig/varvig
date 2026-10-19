package main

import (
	"net"
	"strings"
	"testing"

	"github.com/varvig/varvig/varvig/internal/multihash"
	"github.com/varvig/varvig/varvig/internal/object"
	"github.com/varvig/varvig/varvig/internal/p2p"
	"github.com/varvig/varvig/varvig/internal/repo"
	"github.com/varvig/varvig/varvig/internal/reserved"
)

// The head and the replicating namespaces are separate ref namespaces with
// separate compare-and-swaps, and sync attempts each independently.
//
// This was not always so, and the failure it caused was serious: a peer whose
// branch had moved on received no notes and no reserved refs either, because the
// head's refused CAS returned before they were reached. A settled lease is the
// only record that money was spent, and it was being withheld from a peer over
// an unrelated disagreement about code — which is exactly the condition a mesh
// of peers is in most of the time.

func dialled(t *testing.T, server *repo.Repo) *p2p.Client {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	go func() { _ = p2p.Serve(server, b) }()
	client, err := p2p.Dial(a)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return client
}

// commit writes a change with the given message and points name at it.
func commit(t *testing.T, r *repo.Repo, name, message string) multihash.Multihash {
	t.Helper()
	blob, err := r.Objects.Put(object.NewBlob([]byte(message)))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := r.Objects.Put(object.NewTree([]object.Entry{
		{Name: "f.txt", Mode: 0o100644, Kind: object.TypeBlob, ID: blob},
	}))
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.Objects.Put(object.NewChange(object.Change{Tree: tree, Message: message, Timestamp: 1}))
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := r.Refs.Resolve(name)
	if err := r.Refs.CompareAndSwap(name, cur, id, "test", message); err != nil {
		t.Fatal(err)
	}
	return id
}

const leaseRef = reserved.LeasesPrefix + "mini-a/deploy"

// TestARefusedHeadPushStillDeliversTheLease is the regression. The peer's branch
// has moved, so the head's force-with-lease is refused — and the spend still has
// to arrive, because the lease is the only record that money left the building.
func TestARefusedHeadPushStillDeliversTheLease(t *testing.T) {
	server, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	commit(t, server, "refs/heads/main", "the peer went its own way")

	local, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	commit(t, local, "refs/heads/main", "and so did we")
	// A tracking ref that matches neither side, so the CAS is certain to be
	// refused — the ordinary state of a cell that last fetched from some other
	// peer in the mesh.
	stale := commit(t, local, "refs/remotes/origin/main", "what a third peer had")
	_ = stale
	lease, err := local.Objects.Put(object.NewBlob([]byte(`{"cell_id":"mini-a","spent":320}`)))
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Refs.CompareAndSwap(leaseRef, nil, lease, "test", "settled spend"); err != nil {
		t.Fatal(err)
	}

	err = pushToPeer(dialled(t, server), local, "main", "peer")
	if err == nil {
		t.Fatal("a refused head push must still be reported")
	}
	if !strings.Contains(err.Error(), "head:") {
		t.Errorf("the failure should be attributed to the head, got: %v", err)
	}

	got, rerr := server.Refs.Resolve(leaseRef)
	if rerr != nil {
		t.Fatalf("the lease did not reach the peer, so the spend went unreported: %v", rerr)
	}
	if !got.Equal(lease) {
		t.Errorf("the peer holds %s, want %s", got.Hex(), lease.Hex())
	}
}

// TestAPeerWithoutOurBranchStillYieldsItsTickets is the fetch direction. A peer
// that has never heard of this branch still holds tickets and authority state,
// and refusing to look at any of it because the head is missing is how a cell
// learns nothing from a peer working elsewhere.
func TestAPeerWithoutOurBranchStillYieldsItsTickets(t *testing.T) {
	server, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The peer has a ticket and a lease, but nothing on refs/heads/feature.
	ticket, err := server.Objects.Put(object.NewBlob([]byte("a real ticket")))
	if err != nil {
		t.Fatal(err)
	}
	ticketRef := reserved.TicketsPrefix + "1e20aa"
	if err := server.Refs.CompareAndSwap(ticketRef, nil, ticket, "test", "seed"); err != nil {
		t.Fatal(err)
	}

	local, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = fetchFromPeer(dialled(t, server), local, "feature", "peer")
	if err == nil {
		t.Fatal("a missing head must still be reported")
	}
	if !strings.Contains(err.Error(), "head:") {
		t.Errorf("the failure should be attributed to the head, got: %v", err)
	}
	if _, rerr := local.Refs.Resolve(ticketRef); rerr != nil {
		t.Errorf("the peer's ticket did not replicate: %v", rerr)
	}
}

// TestASuccessfulPushReportsNoFailure: the decoupling must not turn the ordinary
// path into a noisy one.
func TestASuccessfulPushReportsNoFailure(t *testing.T) {
	server, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	local, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	commit(t, local, "refs/heads/main", "our work")
	lease, err := local.Objects.Put(object.NewBlob([]byte(`{"cell_id":"mini-a"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Refs.CompareAndSwap(leaseRef, nil, lease, "test", "lease"); err != nil {
		t.Fatal(err)
	}

	if err := pushToPeer(dialled(t, server), local, "main", "peer"); err != nil {
		t.Fatalf("a push with nothing wrong must report nothing: %v", err)
	}
	if _, err := server.Refs.Resolve("refs/heads/main"); err != nil {
		t.Errorf("the head did not arrive: %v", err)
	}
	if _, err := server.Refs.Resolve(leaseRef); err != nil {
		t.Errorf("the lease did not arrive: %v", err)
	}
}

// TestNothingToPushIsReportedNotFatal: a repository with no such branch still
// has notes and reserved refs to send, and one is not a reason to skip the
// other.
func TestNothingToPushIsReportedNotFatal(t *testing.T) {
	server, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	local, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := local.Objects.Put(object.NewBlob([]byte(`{"cell_id":"mini-a"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Refs.CompareAndSwap(leaseRef, nil, lease, "test", "lease"); err != nil {
		t.Fatal(err)
	}

	err = pushToPeer(dialled(t, server), local, "nonexistent", "peer")
	if err == nil {
		t.Fatal("a branch we do not have must be reported")
	}
	if _, rerr := server.Refs.Resolve(leaseRef); rerr != nil {
		t.Errorf("the lease did not arrive despite the missing branch: %v", rerr)
	}
}
