package p2p

import (
	"net"
	"strings"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
	"github.com/dividebyzero/claude-experiments/varvig/internal/wire"
)

// putBlobRef writes a blob and points name at it, returning the id. It stands in
// for how the ticket and Factory layers write: a canonical object, and a ref
// moved by compare-and-swap.
func putBlobRef(t *testing.T, r *repo.Repo, name, body string) multihash.Multihash {
	t.Helper()
	id, err := r.Objects.Put(object.NewBlob([]byte(body)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	cur := resolveOrNil(r, name)
	if err := r.Refs.CompareAndSwap(name, cur, id, "test", "seed "+name); err != nil {
		t.Fatalf("CAS %s: %v", name, err)
	}
	return id
}

const (
	ticketRef  = reserved.TicketsPrefix + "1e20aa"
	leaseRef   = reserved.LeasesPrefix + "mini-a/deploy"
	claimRef   = reserved.ClaimsPrefix + "mini-a/task-1"
	privateRef = "refs/myteam/scratch"
)

// TestReservedRefsReplicateOnFetch is the regression this file exists for: a
// peer's ticket refs and Factory refs were advertised on LISTREFS and never
// asked for, so a fetch delivered the branch and nothing else. Every layer built
// on a reserved namespace was invisible across the wire.
func TestReservedRefsReplicateOnFetch(t *testing.T) {
	server, _, _ := seedServer(t)
	putBlobRef(t, server, ticketRef, "a ticket")
	putBlobRef(t, server, leaseRef, `{"cell_id":"mini-a"}`)
	client := dialServe(t, server)

	dst, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rep, err := ReplicateRefsFetch(client, dst)
	if err != nil {
		t.Fatalf("ReplicateRefsFetch: %v", err)
	}
	for _, name := range []string{ticketRef, leaseRef} {
		if _, err := dst.Refs.Resolve(name); err != nil {
			t.Errorf("%s did not replicate: %v", name, err)
		}
	}
	if len(rep.Updated) != 2 {
		t.Errorf("report should name both refs, got %v", rep.Updated)
	}
	if len(rep.Diverged) != 0 {
		t.Errorf("nothing diverged, got %v", rep.Diverged)
	}
}

// TestReplicationIsConfinedToTheCatalogue asserts the mechanism moves exactly
// what internal/reserved says replicates — no more. The exclusions there are
// each a deliberate decision, and a replicator that quietly widened them would
// import a peer's org chart or their retention obligations.
func TestReplicationIsConfinedToTheCatalogue(t *testing.T) {
	server, _, _ := seedServer(t)
	putBlobRef(t, server, privateRef, "mine")
	putBlobRef(t, server, reserved.PrincipalsRef, "their org chart")
	putBlobRef(t, server, reserved.PolicyRef, "their policy module")
	putBlobRef(t, server, reserved.PinsPrefix+"aabb/0000000000000001/deadbeef", "their pin")
	client := dialServe(t, server)

	dst, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReplicateRefsFetch(client, dst); err != nil {
		t.Fatalf("ReplicateRefsFetch: %v", err)
	}
	for _, name := range []string{
		privateRef, reserved.PrincipalsRef, reserved.PolicyRef,
		reserved.PinsPrefix + "aabb/0000000000000001/deadbeef",
		"refs/heads/main",
	} {
		if _, err := dst.Refs.Resolve(name); err == nil {
			t.Errorf("%s is outside the replication catalogue and must not have replicated", name)
		}
	}
}

// TestDivergedRefIsReportedAndUntouched covers the case the core cannot decide:
// two peers wrote unrelated objects under one name. A lease is a blob with no
// links, so there is no ancestry to prefer either by — and picking one would be
// the core inventing a meaning for a ref it does not own.
func TestDivergedRefIsReportedAndUntouched(t *testing.T) {
	server, _, _ := seedServer(t)
	putBlobRef(t, server, leaseRef, `{"cell_id":"mini-a","units":10}`)
	client := dialServe(t, server)

	dst, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mine := putBlobRef(t, dst, leaseRef, `{"cell_id":"mini-a","units":99}`)

	rep, err := ReplicateRefsFetch(client, dst)
	if err != nil {
		t.Fatalf("divergence must be a report, not an error: %v", err)
	}
	if len(rep.Diverged) != 1 || rep.Diverged[0] != leaseRef {
		t.Fatalf("expected %s reported as diverged, got %v", leaseRef, rep.Diverged)
	}
	if len(rep.Updated) != 0 {
		t.Errorf("nothing should have moved, got %v", rep.Updated)
	}
	got, err := dst.Refs.Resolve(leaseRef)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(mine) {
		t.Errorf("our value was clobbered: %s became %s", mine.Hex(), got.Hex())
	}
}

// TestOneDivergenceDoesNotStopTheRest is why divergence is a report rather than
// an abort. Two cells racing a claim is the claim mechanism working; if that
// stopped every lease and ticket from replicating, contention would starve the
// authority state a cell needs to act at all.
func TestOneDivergenceDoesNotStopTheRest(t *testing.T) {
	server, _, _ := seedServer(t)
	putBlobRef(t, server, claimRef, `{"cell":"mini-b"}`)
	putBlobRef(t, server, leaseRef, `{"cell_id":"mini-a"}`)
	putBlobRef(t, server, ticketRef, "a ticket")
	client := dialServe(t, server)

	dst, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	putBlobRef(t, dst, claimRef, `{"cell":"mini-a"}`) // we claimed it too

	rep, err := ReplicateRefsFetch(client, dst)
	if err != nil {
		t.Fatalf("ReplicateRefsFetch: %v", err)
	}
	if len(rep.Diverged) != 1 || rep.Diverged[0] != claimRef {
		t.Fatalf("expected only the claim to diverge, got %v", rep.Diverged)
	}
	for _, name := range []string{leaseRef, ticketRef} {
		if _, err := dst.Refs.Resolve(name); err != nil {
			t.Errorf("%s should have replicated despite the contested claim: %v", name, err)
		}
	}
}

// TestRefWithAncestryFastForwards covers the one case the core *can* decide
// without knowing what a ref means: the peer's object reaches ours through the
// links every object already exposes, so it strictly extends what we hold. A
// ticket's intent chain has that shape; a lease blob does not, which is why the
// weak rule and the strong one live in the same function.
func TestRefWithAncestryFastForwards(t *testing.T) {
	server, c1, c2 := seedServer(t)
	if err := server.Refs.Create(ticketRef, c2, "test", "chained"); err != nil {
		t.Fatal(err)
	}
	client := dialServe(t, server)

	dst, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Give the destination the older link in the same chain.
	if err := client.Fetch(dst.Objects, []multihash.Multihash{c1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := dst.Refs.Create(ticketRef, c1, "test", "older"); err != nil {
		t.Fatal(err)
	}

	rep, err := ReplicateRefsFetch(client, dst)
	if err != nil {
		t.Fatalf("ReplicateRefsFetch: %v", err)
	}
	if len(rep.Diverged) != 0 {
		t.Fatalf("a ref whose remote value reaches ours is not divergent: %v", rep.Diverged)
	}
	got, err := dst.Refs.Resolve(ticketRef)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(c2) {
		t.Errorf("expected fast-forward to %s, got %s", c2.Hex(), got.Hex())
	}
}

// TestReservedRefsReplicateOnPush is the fetch test's mirror. Push matters more
// than fetch for a cell: the state an overseer needs to see — attempts,
// reservations, reported spend — is written locally and has to leave.
func TestReservedRefsReplicateOnPush(t *testing.T) {
	server, _, _ := seedServer(t)
	client := dialServe(t, server)

	src, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	putBlobRef(t, src, reserved.ReservationsPrefix+"mini-a/key1", `{"state":"pending"}`)
	putBlobRef(t, src, privateRef, "mine alone")

	rep, err := ReplicateRefsPush(client, src)
	if err != nil {
		t.Fatalf("ReplicateRefsPush: %v", err)
	}
	if len(rep.Updated) != 1 {
		t.Fatalf("expected one ref pushed, got %v", rep.Updated)
	}
	if _, err := server.Refs.Resolve(reserved.ReservationsPrefix + "mini-a/key1"); err != nil {
		t.Errorf("reservation did not reach the peer: %v", err)
	}
	if _, err := server.Refs.Resolve(privateRef); err == nil {
		t.Error("a ref outside the catalogue must not have been pushed")
	}
}

// TestPushDoesNotClobberADivergentPeer is the property that makes replication
// safe to run by default in both directions: it can add state to a peer, never
// silently replace state the peer wrote.
func TestPushDoesNotClobberADivergentPeer(t *testing.T) {
	server, _, _ := seedServer(t)
	theirs := putBlobRef(t, server, claimRef, `{"cell":"mini-b"}`)
	client := dialServe(t, server)

	src, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	putBlobRef(t, src, claimRef, `{"cell":"mini-a"}`)

	rep, err := ReplicateRefsPush(client, src)
	if err != nil {
		t.Fatalf("ReplicateRefsPush: %v", err)
	}
	if len(rep.Diverged) != 1 || rep.Diverged[0] != claimRef {
		t.Fatalf("expected the claim reported as diverged, got %v", rep.Diverged)
	}
	got, err := server.Refs.Resolve(claimRef)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(theirs) {
		t.Errorf("the peer's value was clobbered: %s became %s", theirs.Hex(), got.Hex())
	}
}

// TestDeletionNeverPropagates: a ref one side lacks is left alone, because the
// protocol cannot tell "deleted there" from "never existed there". Guessing
// deletion would drop a reservation record, and a missing reservation reads as
// "nothing was ordered" — the one wrong answer for an irreversible action.
func TestDeletionNeverPropagates(t *testing.T) {
	server, _, _ := seedServer(t)
	client := dialServe(t, server)

	dst, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mine := putBlobRef(t, dst, reserved.ReservationsPrefix+"mini-a/key1", `{"state":"pending"}`)

	if _, err := ReplicateRefsFetch(client, dst); err != nil {
		t.Fatalf("ReplicateRefsFetch: %v", err)
	}
	got, err := dst.Refs.Resolve(reserved.ReservationsPrefix + "mini-a/key1")
	if err != nil {
		t.Fatalf("a ref the peer never had must survive a fetch: %v", err)
	}
	if !got.Equal(mine) {
		t.Errorf("value changed: %s became %s", mine.Hex(), got.Hex())
	}
}

// TestTransferFailureIsLoud holds the notes discipline (federation §4): between
// two peers, a ref that cannot be fully transferred is an error, never a silent
// omission. Divergence is a report; a dropped object is not.
func TestTransferFailureIsLoud(t *testing.T) {
	server, _, _ := seedServer(t)
	id := putBlobRef(t, server, ticketRef, "a ticket")
	if err := server.Objects.Delete(id); err != nil {
		t.Fatal(err)
	}
	client := dialServe(t, server)

	dst, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = ReplicateRefsFetch(client, dst)
	if err == nil {
		t.Fatal("a ref that failed to transfer must be a loud error, not a silent skip")
	}
	if !strings.Contains(err.Error(), ticketRef) {
		t.Errorf("the error should name the ref in flight, got: %v", err)
	}
}

// TestNoCapabilityIsRequired is a deployability property, not a nicety: LISTREFS
// already advertises these refs and servePush already accepts them, so the fix
// works against a peer built before it existed. A negotiated bit would have
// turned a client-side omission into a federation-wide upgrade.
func TestNoCapabilityIsRequired(t *testing.T) {
	server, _, _ := seedServer(t)
	putBlobRef(t, server, ticketRef, "a ticket")
	client := dialServe(t, server)
	// Strip the federation bits, keeping only the payload codec the two ends
	// already negotiated: this is a peer that predates notes-sync, pin and
	// artifact-ref entirely.
	client.caps = map[string]bool{wire.CapDeflate: client.caps[wire.CapDeflate]}

	dst, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReplicateRefsFetch(client, dst); err != nil {
		t.Fatalf("ref replication must not depend on a negotiated capability: %v", err)
	}
	if _, err := dst.Refs.Resolve(ticketRef); err != nil {
		t.Errorf("ticket did not replicate from a capability-less peer: %v", err)
	}
}

// servePeerAdvertising is a peer that is not a varvig server: it completes the
// handshake, answers LISTREFS with whatever list the test hands it, and serves
// objects from r for everything else.
//
// It exists because a real varvig server cannot produce the case under test.
// serveListRefs resolves each name before advertising it, and Resolve validates,
// so a malformed name is filtered at the source. That makes the client-side
// check defence against a peer that is not a varvig server — which is exactly
// what a network boundary has to assume — so the test dials one.
func dialPeerAdvertising(t *testing.T, r *repo.Repo, advertise []wire.Ref) *Client {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	go func() {
		conn := wire.NewConn(b)
		caps, err := handshake(conn, localHello())
		if err != nil {
			return
		}
		for {
			mt, payload, err := conn.ReadFrame()
			if err != nil {
				return
			}
			switch mt {
			case wire.MsgListRefs:
				_ = conn.WriteRefs(advertise)
				_ = conn.Flush()
			case wire.MsgGetObjects:
				_ = serveGetObjects(r.Objects, conn, caps, payload)
			default:
				return
			}
		}
	}()
	client, err := Dial(a)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return client
}

// TestAMalformedAdvertisedNameIsRefusedNotWritten covers the boundary: these
// names arrive over the network and become paths in our ref store. The refusal
// is reported, and it must not stop the refs alongside it from replicating —
// otherwise one bad advertisement is a denial of service on a cell's authority
// state.
func TestAMalformedAdvertisedNameIsRefusedNotWritten(t *testing.T) {
	server, _, _ := seedServer(t)
	good := putBlobRef(t, server, leaseRef, `{"cell_id":"mini-a"}`)
	client := dialPeerAdvertising(t, server, []wire.Ref{
		{Name: leaseRef, ID: good},
		{Name: reserved.FactoryPrefix + "leases/../../escaped", ID: good},
		{Name: reserved.TicketsPrefix + "with\x00nul", ID: good},
		{Name: reserved.TicketsPrefix + "badid", ID: []byte{0xff, 0xff}},
	})

	dst, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rep, err := ReplicateRefsFetch(client, dst)
	if err != nil {
		t.Fatalf("malformed names must not fail the pass: %v", err)
	}
	if len(rep.Refused) != 3 {
		t.Fatalf("expected both malformed names and the undecodable id refused, got %v", rep.Refused)
	}
	if _, err := dst.Refs.Resolve(leaseRef); err != nil {
		t.Errorf("the valid ref alongside them must still replicate: %v", err)
	}
	names, err := dst.Refs.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if strings.Contains(name, "escaped") || strings.Contains(name, "nul") || strings.Contains(name, "badid") {
			t.Errorf("a malformed name was written into our ref store as %q", name)
		}
	}
}
