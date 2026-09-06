// Package ticket gives a ticket a stable identity as a ref and its state as an
// append-only chain of intent revisions (tickets §1.2) — the same construction
// as a branch over commits, pointed at a new namespace. There is no new
// machinery here: identity is refs/varvig/tickets/<id> moved by compare-and-swap,
// mutation appends an immutable revision, undo is the reflog, and replication is
// ordinary ref sync.
//
// The construction itself lives in package idchain, which the context graph's
// identity nodes share (GRAPH.md §3.1). What stays here is what is specific to a
// ticket: the namespace, and reading a revision's state as a spec.
//
// A ticket revision is an *unmaterialized* change (D1): intent with no tree,
// carrying the spec, provenance, authorship, and a signature. The ticket id is
// the genesis revision's hash — stable forever, because the genesis revision is
// immutable — while the ref value tracks the current head.
package ticket

import (
	"crypto/ed25519"
	"fmt"
	"sort"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/idchain"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
)

// Ref returns the ticket ref name for a ticket id.
func Ref(id multihash.Multihash) string { return reserved.TicketsPrefix + id.Hex() }

// Info is a ticket's current state: its stable id, the head intent revision,
// and that revision's spec and materialization.
type Info struct {
	ID           multihash.Multihash // stable ticket id = genesis revision hash
	Head         multihash.Multihash // current intent revision
	Spec         string
	Materialized bool
}

// New mints a ticket: a signed, unmaterialized genesis revision plus a ref
// refs/varvig/tickets/<id> pointing at it. The returned id is the ticket's
// stable identity for its whole life.
func New(r *repo.Repo, spec string, priv ed25519.PrivateKey, author string, now int64) (multihash.Multihash, error) {
	if spec == "" {
		return nil, fmt.Errorf("ticket: a spec is required")
	}
	return idchain.New(r, reserved.TicketsPrefix, spec, priv, author, now)
}

// Revise appends a new intent revision (parent = the current head) and moves the
// ticket ref by compare-and-swap. The reflog records the move, so a bad edit —
// a director's Friday reprioritization — is recoverable by construction (§1.2).
// Approvals do not follow: they bind to the old revision's hash (§2.2).
func Revise(r *repo.Repo, id multihash.Multihash, spec string, priv ed25519.PrivateKey, author string, now int64) (multihash.Multihash, error) {
	if spec == "" {
		return nil, fmt.Errorf("ticket: a spec is required")
	}
	return idchain.Revise(r, reserved.TicketsPrefix, id, spec, priv, author, now)
}

// Head returns the current intent revision of a ticket.
func Head(r *repo.Repo, id multihash.Multihash) (multihash.Multihash, error) {
	return r.Refs.Resolve(Ref(id))
}

// Get returns a ticket's current state.
func Get(r *repo.Repo, id multihash.Multihash) (Info, error) {
	head, err := Head(r, id)
	if err != nil {
		return Info{}, err
	}
	return infoFor(r, id, head)
}

// List returns every ticket, ordered by id for determinism.
func List(r *repo.Repo) ([]Info, error) {
	names, err := r.Refs.List()
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, n := range names {
		if !strings.HasPrefix(n, reserved.TicketsPrefix) {
			continue
		}
		suffix := strings.TrimPrefix(n, reserved.TicketsPrefix)
		// A ticket ref is refs/varvig/tickets/<id>; skip any deeper path (e.g.
		// a future .../<id>/spec) so each ticket is listed once.
		if strings.Contains(suffix, "/") {
			continue
		}
		id, err := multihash.ParseHex(suffix)
		if err != nil {
			continue
		}
		head, err := r.Refs.Resolve(n)
		if err != nil {
			continue
		}
		info, err := infoFor(r, id, head)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.Hex() < out[j].ID.Hex() })
	return out, nil
}

func infoFor(r *repo.Repo, id, head multihash.Multihash) (Info, error) {
	obj, err := r.Objects.Get(head)
	if err != nil {
		return Info{}, err
	}
	c, err := obj.AsChange()
	if err != nil {
		return Info{}, fmt.Errorf("ticket: head %s is not a change: %w", head.Hex(), err)
	}
	return Info{ID: id, Head: head, Spec: c.Message, Materialized: c.Materialized()}, nil
}
