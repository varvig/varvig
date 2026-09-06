package edge

// Writing and reading edge notes.
//
// An edge attaches to its varvig-side endpoint, as a note under the reserved
// varvig/edge namespace. It never touches the object it describes: a note is an
// immutable object of its own, and the (namespace, target) pair maps to a ref,
// so attaching an edge changes no hash (design §2).
//
// The whole surface here takes StoredEdge. There is no overload, no variant, and
// no internal helper that takes a DerivedEdge — that is the property this package
// exists to guarantee, and the compiler enforces it for every caller.

import (
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/graphnode"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/notes"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
)

// ErrNoAnchor reports that neither endpoint is content-addressed, so there is
// nothing in this repository for the edge to attach to. An edge between two
// foreign systems is somebody else's edge.
var ErrNoAnchor = fmt.Errorf("edge: neither endpoint is a varvig object")

// ErrCollectableUnsupported reports an edge touching an ephemeral endpoint.
//
// Notes pin their targets (tickets D4), so storing such an edge as an ordinary
// note would keep alive exactly the speculation state it was supposed to die
// with — retention pressure arriving through a new door (GRAPH.md §11.5). The
// collection machinery that would make this safe is not built, so the write
// fails loudly here rather than succeeding and leaking. A3's preference: a named
// error at the moment of excess beats a store that degrades invisibly.
var ErrCollectableUnsupported = fmt.Errorf(
	"edge: an edge to an ephemeral endpoint cannot be stored yet; its collection with that endpoint is unimplemented")

// Anchor returns the object an edge attaches to: the source when it is
// content-addressed, otherwise the target. An identity node anchors on the
// revision it resolved to, which is what binds the edge to what it meant at the
// time rather than to whatever the identity later became (A4).
func Anchor(e StoredEdge) (multihash.Multihash, error) {
	for _, n := range []graphnode.Node{e.source, e.target} {
		switch v := n.(type) {
		case graphnode.ObjectNode:
			return v.ID(), nil
		case graphnode.IdentityNode:
			return v.Revision(), nil
		}
	}
	return nil, ErrNoAnchor
}

// Put writes an edge as a note on its anchor and returns the note's id.
//
// It accepts StoredEdge and nothing else. A derived edge has no route here: not
// through an overload, not through a conversion, not through a Spec — the type
// is simply not admissible, and a caller that tries fails to compile.
func Put(r *repo.Repo, e StoredEdge, author string, now int64) (multihash.Multihash, error) {
	if e.Retention() == graphnode.Collectable {
		return nil, ErrCollectableUnsupported
	}
	anchor, err := Anchor(e)
	if err != nil {
		return nil, err
	}
	payload, err := Encode(e)
	if err != nil {
		return nil, err
	}
	return notes.New(r).Add(reserved.NoteEdge, anchor, payload, author, now)
}

// Entry is a stored edge together with the id of the note carrying it. The note
// id is the edge's content hash, which is what a promotion attestation binds to
// (§11.3) — so an approval names the exact edge that was read.
type Entry struct {
	Note multihash.Multihash
	Edge StoredEdge
}

// List returns every edge anchored on an object, newest first. A note whose
// payload this binary cannot decode is reported rather than skipped: silently
// dropping an edge would make a coverage gap indistinguishable from an absence
// of edges, which is the failure §5 exists to prevent.
func List(r *repo.Repo, anchor multihash.Multihash) ([]Entry, error) {
	chain, err := notes.New(r).List(reserved.NoteEdge, anchor)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(chain))
	for _, n := range chain {
		e, err := Decode(n.Note.Payload)
		if err != nil {
			return nil, fmt.Errorf("edge: note %s on %s: %w", n.ID.Hex(), anchor.Hex(), err)
		}
		out = append(out, Entry{Note: n.ID, Edge: e})
	}
	return out, nil
}
