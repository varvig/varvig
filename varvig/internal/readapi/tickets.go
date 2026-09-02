package readapi

import (
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/ticket"
)

// Ticket reads (tickets §1.2) go through the one query layer like every other
// read. A ticket is an unmaterialized change — intent with no tree — so these
// views carry the spec and discussion, never file content; reading a ticket
// cannot leak anything outside a task's file scope. Governance (attestations,
// approve/veto) is deliberately absent here: it is a human decision surface, not
// a read view.

// TicketView is a ticket's current state, hash-addressed like every other view.
type TicketView struct {
	ID           string `json:"id"`           // stable ticket id (genesis revision hash)
	Head         string `json:"head"`         // current intent revision
	Spec         string `json:"spec"`         // the head revision's intent text
	Materialized bool   `json:"materialized"` // whether the intent has a tree yet
}

// TicketComment is one entry of a ticket's ungoverned discussion.
type TicketComment struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	Origin    string `json:"origin,omitempty"`
	Timestamp int64  `json:"ts"`
}

// TicketDetail is a ticket plus its discussion, oldest comment first, and its
// derived implementation state.
type TicketDetail struct {
	TicketView
	Comments []TicketComment `json:"comments"`
	// Implementation is "open", "stale", or "implemented" — derived from the
	// commits that fulfill the ticket (the ticket→commit link). Implementers are
	// the ids of the branch-reachable commits behind that state.
	Implementation string   `json:"implementation"`
	Implementers   []string `json:"implementers,omitempty"`
}

func ticketViewOf(info ticket.Info) TicketView {
	return TicketView{
		ID:           info.ID.Hex(),
		Head:         info.Head.Hex(),
		Spec:         info.Spec,
		Materialized: info.Materialized,
	}
}

// Tickets lists every ticket in the repository, ordered by id.
func (q *Query) Tickets() ([]TicketView, error) {
	infos, err := ticket.List(q.r)
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]TicketView, len(infos))
	for i, in := range infos {
		out[i] = ticketViewOf(in)
	}
	return out, nil
}

// ArtifactView is one external artifact a ticket's head revision names — a
// reachability handle for bytes that live outside the object store (a container
// image, an SBOM, a release archive), not the bytes themselves. Hashes are hex
// so the view is a stable, tool-friendly JSON shape.
type ArtifactView struct {
	ContentHash string   `json:"content_hash"`
	MediaType   string   `json:"media_type,omitempty"`
	Size        uint64   `json:"size,omitempty"`
	Locators    []string `json:"locators,omitempty"`
	ProducedBy  string   `json:"produced_by,omitempty"`
}

func artifactViewOf(a object.ArtifactRef) ArtifactView {
	v := ArtifactView{
		ContentHash: a.ContentHash.Hex(),
		MediaType:   a.MediaType,
		Size:        a.Size,
		Locators:    a.Locators,
	}
	if a.ProducedBy != nil {
		v.ProducedBy = a.ProducedBy.Hex()
	}
	return v
}

// TicketArtifacts returns the external artifacts a ticket's head revision names
// (federation §1), so a tool — a bridge connector, say — can surface them
// alongside the tracker row. Like the rest of the ticket read surface it reflects
// the head revision only.
func (q *Query) TicketArtifacts(id multihash.Multihash) ([]ArtifactView, error) {
	arts, err := ticket.Artifacts(q.r, id)
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]ArtifactView, len(arts))
	for i, a := range arts {
		out[i] = artifactViewOf(a)
	}
	return out, nil
}

// Ticket returns one ticket's state and its discussion.
func (q *Query) Ticket(id multihash.Multihash) (TicketDetail, error) {
	info, err := ticket.Get(q.r, id)
	if err != nil {
		return TicketDetail{}, wrap(err)
	}
	comments, err := ticket.Comments(q.r, id)
	if err != nil {
		return TicketDetail{}, wrap(err)
	}
	cs := make([]TicketComment, len(comments))
	for i, c := range comments {
		cs[i] = TicketComment{Author: c.Author, Body: c.Body, Origin: c.Origin, Timestamp: c.Timestamp}
	}
	state, commits, err := ticket.Implementation(q.r, id)
	if err != nil {
		return TicketDetail{}, wrap(err)
	}
	impl := make([]string, len(commits))
	for i, c := range commits {
		impl[i] = c.Hex()
	}
	return TicketDetail{
		TicketView:     ticketViewOf(info),
		Comments:       cs,
		Implementation: state,
		Implementers:   impl,
	}, nil
}
