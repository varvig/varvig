package bridge

import (
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/edge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/graphnode"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/ticket"
)

const (
	testConnector = "some-connector"
	testEdgeType  = "somesystem:tracks"
)

func newTicket(t *testing.T, r *repo.Repo) (id []byte) {
	t.Helper()
	tid, err := ticket.New(r, "make login work", newKey(t), "jan", 100)
	if err != nil {
		t.Fatal(err)
	}
	return tid
}

// TestImportedEdgeIsAlwaysWeak is G3's acceptance: a bridge cannot write an edge
// at strength above weak under any input. There is no argument to pass — the
// class and strength are fixed inside importEdge — so the property holds for a
// compromised connector, not only a well-behaved one.
func TestImportedEdgeIsAlwaysWeak(t *testing.T) {
	r := newRepo(t)
	tid := newTicket(t, r)

	if _, _, err := ImportTicketEdge(r, tid,
		ForeignRow{System: "somesystem", ForeignID: "PROJ-1"},
		testEdgeType, testConnector, 200); err != nil {
		t.Fatal(err)
	}
	got, err := ImportedEdges(r, tid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d edges, want 1", len(got))
	}
	p := got[0].Edge.Provenance()
	if p.Strength != object.StrengthWeak {
		t.Errorf("imported edge strength = %s, want weak", p.Strength)
	}
	if p.Class != edge.Imported {
		t.Errorf("imported edge class = %s, want imported", p.Class)
	}
	if p.Principal != testConnector {
		t.Errorf("principal = %q, want the connector", p.Principal)
	}
}

// TestReImportOfUnchangedStateWritesNothing is G3's echo-suppression acceptance:
// a connector that polls a tracker and sees no change must leave no trace. A
// note chain growing one entry per poll would be unbounded and would make a
// re-observed edge indistinguishable from a re-read one.
func TestReImportOfUnchangedStateWritesNothing(t *testing.T) {
	r := newRepo(t)
	tid := newTicket(t, r)
	row := ForeignRow{System: "somesystem", ForeignID: "PROJ-1"}

	first, written, err := ImportTicketEdge(r, tid, row, testEdgeType, testConnector, 200)
	if err != nil {
		t.Fatal(err)
	}
	if !written {
		t.Fatal("the first import must write")
	}

	// The connector polls again and sees exactly the same foreign state. Note the
	// different timestamp: a clock tick is not a change.
	second, written, err := ImportTicketEdge(r, tid, row, testEdgeType, testConnector, 999)
	if err != nil {
		t.Fatal(err)
	}
	if written {
		t.Error("re-importing unchanged foreign state wrote a second edge")
	}
	if !second.Equal(first) {
		t.Error("the suppressed import did not report the existing edge")
	}
	got, err := ImportedEdges(r, tid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("the note chain grew to %d entries on a no-op poll", len(got))
	}

	// A genuine change does write.
	changed := ForeignRow{System: "somesystem", ForeignID: "PROJ-2"}
	if _, written, err = ImportTicketEdge(r, tid, changed, testEdgeType, testConnector, 300); err != nil {
		t.Fatal(err)
	}
	if !written {
		t.Error("a changed foreign row must produce a new edge")
	}
}

// TestTicketEdgeBindsToTheRevisionNotTheRef is A4 applied here: a ticket's ref
// moves on every revision, so an edge bound to it would silently come to
// describe intent nobody imported it against.
func TestTicketEdgeBindsToTheRevisionNotTheRef(t *testing.T) {
	r := newRepo(t)
	key := newKey(t)
	tid, err := ticket.New(r, "first intent", key, "jan", 100)
	if err != nil {
		t.Fatal(err)
	}
	// Revise once *before* importing, so the revision the edge binds to is
	// neither the genesis nor the eventual head. A test where they coincide
	// cannot tell the three bindings apart.
	importedAgainst, err := ticket.Revise(r, tid, "second intent", key, "jan", 150)
	if err != nil {
		t.Fatal(err)
	}
	if importedAgainst.Equal(tid) {
		t.Fatal("a revision must differ from the genesis for this test to mean anything")
	}
	row := ForeignRow{System: "somesystem", ForeignID: "PROJ-1"}
	if _, _, err := ImportTicketEdge(r, tid, row, testEdgeType, testConnector, 200); err != nil {
		t.Fatal(err)
	}

	// The ticket is revised. The edge was imported against the old intent.
	rev3, err := ticket.Revise(r, tid, "third intent", key, "jan", 300)
	if err != nil {
		t.Fatal(err)
	}

	// The edge did not follow. Asking about the current revision finds nothing.
	current, err := ImportedEdges(r, tid)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 0 {
		t.Errorf("an edge imported against the old intent followed the ticket forward: %d found", len(current))
	}

	// It is still there, attached to the revision it was imported against —
	// which is neither the genesis nor the current head.
	old, err := edge.List(r, importedAgainst)
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 1 {
		t.Fatalf("the edge on the original revision is gone: %d found", len(old))
	}
	// And it still carries the ref, so the thread back to the ticket survives.
	src, ok := old[0].Edge.Source().(graphnode.IdentityNode)
	if !ok {
		t.Fatalf("ticket endpoint is %T, want an identity node", old[0].Edge.Source())
	}
	if src.Ref() != ticket.Ref(tid) {
		t.Errorf("edge ref = %q, want the ticket ref", src.Ref())
	}
	if !src.Revision().Equal(importedAgainst) {
		t.Error("edge revision is not the one it was imported against")
	}
	if src.Revision().Equal(rev3) {
		t.Error("the edge retargeted to the new head")
	}
	if src.Revision().Equal(tid) {
		t.Error("the edge bound to the ticket genesis rather than the revision it saw")
	}
	// Nothing landed on the genesis or the new head.
	for _, other := range []struct {
		name string
		at   []byte
	}{{"genesis", tid}, {"new head", rev3}} {
		es, err := edge.List(r, other.at)
		if err != nil {
			t.Fatal(err)
		}
		if len(es) != 0 {
			t.Errorf("%d edges landed on the %s", len(es), other.name)
		}
	}
}

// TestImportObjectEdge covers the other shape the design names: a foreign run or
// deploy bound to a content-addressed object rather than to a ticket.
func TestImportObjectEdge(t *testing.T) {
	r := newRepo(t)
	blob, err := r.Objects.Put(object.NewBlob([]byte("tree contents")))
	if err != nil {
		t.Fatal(err)
	}
	if _, written, err := ImportObjectEdge(r, blob,
		ForeignRow{System: "ci", ForeignID: "run/8891"},
		"ci:tested", testConnector, 200); err != nil || !written {
		t.Fatalf("ImportObjectEdge: written=%v err=%v", written, err)
	}
	got, err := edge.List(r, blob)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d edges on the object, want 1", len(got))
	}
	tgt, ok := got[0].Edge.Target().(graphnode.ExternalNode)
	if !ok {
		t.Fatalf("foreign endpoint is %T, want an external node", got[0].Edge.Target())
	}
	if tgt.System() != "ci" || tgt.ForeignID() != "run/8891" {
		t.Errorf("foreign endpoint = %s:%s, want ci:run/8891", tgt.System(), tgt.ForeignID())
	}
}

// TestImportRefusesAnAnonymousConnector: an edge whose producer is unnamed has
// no provenance to audit, which is the whole warrant for a non-derived edge.
func TestImportRefusesAnAnonymousConnector(t *testing.T) {
	r := newRepo(t)
	tid := newTicket(t, r)
	if _, _, err := ImportTicketEdge(r, tid,
		ForeignRow{System: "somesystem", ForeignID: "PROJ-1"},
		testEdgeType, "", 200); err == nil {
		t.Error("an imported edge with no named connector must be refused")
	}
}
