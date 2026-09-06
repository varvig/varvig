package p2p

import (
	"bytes"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/edge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/graphnode"
	"github.com/dividebyzero/claude-experiments/varvig/internal/notes"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
)

// TestUnknownEdgeTypeSurvivesSync is the second half of GRAPH.md §11.2's
// acceptance: an edge type no binary has ever seen must survive write, sync and
// read byte-identically. The first half (write and read) is pinned in package
// edge; this is the wire.
//
// It matters because the core never looks at an edge type, so a peer running an
// older binary is not merely tolerant of an unseen type — it cannot tell there
// is anything to tolerate. Sync moves the notes; nothing inspects them.
func TestUnknownEdgeTypeSurvivesSync(t *testing.T) {
	server, _, tip := seedServer(t)

	src, err := graphnode.Object(tip)
	if err != nil {
		t.Fatal(err)
	}
	dstNode, err := graphnode.External("somesystem", "row/42:part:7")
	if err != nil {
		t.Fatal(err)
	}
	e, err := edge.New(edge.Spec{
		Source:        src,
		Target:        dstNode,
		Type:          "producerfromthefuture:a-verb-nobody-has-seen",
		ObservedUnder: tip,
		Provenance: edge.Provenance{
			Class: edge.Imported, Principal: "some-connector", Strength: object.StrengthWeak,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := edge.Put(server, e, "a", 1); err != nil {
		t.Fatal(err)
	}
	anchor, err := edge.Anchor(e)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := edge.Encode(e)
	if err != nil {
		t.Fatal(err)
	}

	client := dialServe(t, server)
	dst, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := ReplicateNotesFetch(client, dst, nil); err != nil {
		t.Fatalf("ReplicateNotesFetch: %v", err)
	}

	got, err := edge.List(dst, anchor)
	if err != nil {
		t.Fatalf("the peer could not read the replicated edge: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("peer has %d edges on the anchor, want 1", len(got))
	}
	if got[0].Edge.Type() != e.Type() {
		t.Errorf("edge type changed in transit: %q -> %q", e.Type(), got[0].Edge.Type())
	}
	received, err := edge.Encode(got[0].Edge)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sent, received) {
		t.Errorf("edge record changed in transit:\n  sent:     %s\n  received: %s", sent, received)
	}
}

// TestEdgeNamespaceCannotBeOptedOutOfSync: a peer may decline any namespace of
// its own, but not this one. An imported edge a peer cannot see is an edge that
// does not exist, and the failure would be silent — the same argument that makes
// evidence non-optional.
func TestEdgeNamespaceCannotBeOptedOutOfSync(t *testing.T) {
	server, _, tip := seedServer(t)
	if _, err := notes.New(server).Add(reserved.NoteEdge, tip, []byte(`{}`), "a", 1); err != nil {
		t.Fatal(err)
	}
	edgeRef := "refs/notes/" + reserved.NoteEdge + "/" + tip.Hex()

	client := dialServe(t, server)
	dst, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A peer that tries to opt out of everything still gets the edge namespace.
	optOutAll := func(string) bool { return true }
	if err := ReplicateNotesFetch(client, dst, optOutAll); err != nil {
		t.Fatalf("ReplicateNotesFetch: %v", err)
	}
	if _, err := dst.Refs.Resolve(edgeRef); err != nil {
		t.Errorf("the edge namespace was opted out of sync: %v", err)
	}
}
