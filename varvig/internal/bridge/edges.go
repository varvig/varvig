package bridge

// Imported edges (GRAPH.md §2, builder G3): the connector-facing half of the
// context graph.
//
// A connector knows things varvig cannot derive — which foreign row tracks this
// ticket, which CI run tested this commit, which deploy shipped this tree — and
// those are edges, not derived facts. They are stored as notes, they replicate,
// and they carry the connector as their principal.
//
// Everything here caps the connector rather than trusting it. The class and the
// strength are not parameters: an imported edge is always `imported` and always
// `weak`, because a connector signs on behalf of a principal who holds no key
// (tickets §2.4). There is no argument a caller can pass to raise either, which
// is what makes the rule hold for a compromised connector and not only for a
// well-behaved one.
//
// The core still learns no vendor's name. `system` is an opaque tag the peer
// chooses, exactly as it is on an external link.

import (
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/edge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/graphnode"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/ticket"
)

// ForeignRow identifies a row in an external system: an opaque system tag the
// connector chooses, and the row's id over there.
type ForeignRow struct {
	System    string
	ForeignID string
}

// ImportTicketEdge records an edge between a ticket and a foreign row.
//
// The edge binds to the ticket's *current intent revision*, not to the ticket
// id and not to its ref (A4). A ticket's ref moves every time the spec is
// revised; an edge bound to it would silently come to describe intent nobody
// imported it against. Bound to the revision, the edge keeps meaning what it
// meant, and the ref it also carries still leads back to the ticket's life.
func ImportTicketEdge(r *repo.Repo, ticketID multihash.Multihash, row ForeignRow, edgeType, connector string, now int64) (multihash.Multihash, bool, error) {
	head, err := ticket.Head(r, ticketID)
	if err != nil {
		return nil, false, fmt.Errorf("bridge: ticket %s: %w", ticketID.Hex(), err)
	}
	node, err := graphnode.Identity(ticket.Ref(ticketID), head)
	if err != nil {
		return nil, false, err
	}
	return importEdge(r, node, head, row, edgeType, connector, now)
}

// ImportObjectEdge records an edge between a content-addressed varvig object — a
// commit, a tree — and a foreign row: a CI run against a commit, a deploy of a
// tree.
func ImportObjectEdge(r *repo.Repo, id multihash.Multihash, row ForeignRow, edgeType, connector string, now int64) (multihash.Multihash, bool, error) {
	node, err := graphnode.Object(id)
	if err != nil {
		return nil, false, err
	}
	return importEdge(r, node, id, row, edgeType, connector, now)
}

// importEdge is the one place an imported edge is built, so the class and
// strength caps cannot be bypassed by reaching for a different entry point.
//
// observedUnder is the varvig endpoint's own hash. An imported edge is a
// statement about that specific object — this row tracks this revision, this run
// tested this commit — and there is no tree it was observed under, so recording
// one would be inventing a fact. The endpoint is the honest answer.
func importEdge(r *repo.Repo, varvigEnd graphnode.Node, observedUnder multihash.Multihash, row ForeignRow, edgeType, connector string, now int64) (multihash.Multihash, bool, error) {
	if connector == "" {
		return nil, false, fmt.Errorf("bridge: an imported edge must name the connector that produced it")
	}
	foreign, err := graphnode.External(row.System, row.ForeignID)
	if err != nil {
		return nil, false, err
	}
	e, err := edge.New(edge.Spec{
		Source:        varvigEnd,
		Target:        foreign,
		Type:          edgeType,
		ObservedUnder: observedUnder,
		Provenance: edge.Provenance{
			// Not parameters. A connector cannot mint above weak under any input.
			Class:     edge.Imported,
			Strength:  object.StrengthWeak,
			Principal: connector,
		},
	})
	if err != nil {
		return nil, false, err
	}
	// PutOnce, not Put: re-importing unchanged foreign state must leave no trace
	// (§5.4's echo suppression, applied to edges).
	return edge.PutOnce(r, e, connector, now)
}

// ImportedEdges returns the edges recorded against a ticket's current intent
// revision. An edge imported against an earlier revision stays attached to that
// revision and is not returned here — which is the point of binding to it.
func ImportedEdges(r *repo.Repo, ticketID multihash.Multihash) ([]edge.Entry, error) {
	head, err := ticket.Head(r, ticketID)
	if err != nil {
		return nil, fmt.Errorf("bridge: ticket %s: %w", ticketID.Hex(), err)
	}
	return edge.List(r, head)
}
