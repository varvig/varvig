// Package deps derives the blocking dependencies between tickets from their
// declared read/write sets (tickets §3.2). There are no hand-declared links:
// two tickets block each other exactly when their scopes conflict, computed
// with the same predicate the transaction scheduler serializes on (txn.Conflict),
// so "what blocks what" is a query over declared scope, not a graph a human
// maintains by dragging arrows.
//
// A ticket's scope (tickets §3.1) is what makes it schedulable, and it doubles
// as the checkout scope and capability boundary. It is stored as a note in the
// reserved varvig/scope namespace, keyed by the ticket's intent hash, so it
// accretes onto the immutable ticket without touching its identity and syncs
// like any other note.
package deps

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/notes"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
	"github.com/dividebyzero/claude-experiments/varvig/internal/txn"
)

// Scope is a ticket's declared read set and write set (tickets §3.1): the path
// prefixes it reads and the ones it writes. A ticket without a scope is
// unschedulable and participates in no derived dependency.
type Scope struct {
	Reads  []string `json:"reads,omitempty"`
	Writes []string `json:"writes,omitempty"`
}

// Ticket pairs a ticket's intent hash with its declared scope.
type Ticket struct {
	ID    multihash.Multihash
	Scope Scope
}

// Blocks reports whether tickets a and b cannot run concurrently — their scopes
// conflict — so one must be scheduled before the other. Blocking is symmetric;
// which goes first is the scheduler's ordering decision (M2), not this graph's.
func Blocks(a, b Scope) bool {
	return txn.Conflict(a.Reads, a.Writes, b.Reads, b.Writes)
}

// SetScope records a ticket's declared scope as a note in the reserved
// varvig/scope namespace. Re-recording appends a new note (the newest wins),
// so a rescope is an ordinary, auditable note accretion.
func SetScope(r *repo.Repo, ticket multihash.Multihash, s Scope, author string, now int64) (multihash.Multihash, error) {
	payload, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return notes.New(r).Add(reserved.NoteScope, ticket, payload, author, now)
}

// GetScope returns a ticket's current declared scope and whether one is set.
func GetScope(r *repo.Repo, ticket multihash.Multihash) (Scope, bool, error) {
	chain, err := notes.New(r).List(reserved.NoteScope, ticket)
	if err != nil {
		return Scope{}, false, err
	}
	if len(chain) == 0 {
		return Scope{}, false, nil
	}
	var s Scope
	if err := json.Unmarshal(chain[0].Note.Payload, &s); err != nil {
		return Scope{}, false, err
	}
	return s, true, nil
}

// ScopedTickets returns every ticket that has a declared scope, discovered from
// the varvig/scope note refs. Order is by hash for determinism.
func ScopedTickets(r *repo.Repo) ([]Ticket, error) {
	names, err := r.Refs.List()
	if err != nil {
		return nil, err
	}
	prefix := "refs/notes/" + reserved.NoteScope + "/"
	var out []Ticket
	for _, n := range names {
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		id, err := multihash.ParseHex(strings.TrimPrefix(n, prefix))
		if err != nil {
			continue
		}
		s, ok, err := GetScope(r, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, Ticket{ID: id, Scope: s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.Hex() < out[j].ID.Hex() })
	return out, nil
}

// Blockers returns the ids of tickets that block ticket, derived purely from
// scope conflicts against the given set. A ticket never blocks itself.
func Blockers(ticket Ticket, others []Ticket) []multihash.Multihash {
	var out []multihash.Multihash
	for _, o := range others {
		if o.ID.Equal(ticket.ID) {
			continue
		}
		if Blocks(ticket.Scope, o.Scope) {
			out = append(out, o.ID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hex() < out[j].Hex() })
	return out
}

// Graph is the derived blocking graph: for each ticket id, the ids it blocks
// with (mutually). It is a pure function of the tickets' scopes — there is no
// API to add an edge by hand (tickets §3.2, §7.4).
func Graph(tickets []Ticket) map[string][]multihash.Multihash {
	g := make(map[string][]multihash.Multihash, len(tickets))
	for _, t := range tickets {
		g[t.ID.Hex()] = Blockers(t, tickets)
	}
	return g
}
