// Package idchain is the construction varvig uses for anything with a life:
// identity is a ref moved by compare-and-swap, and state is an append-only chain
// of immutable revisions (tickets §1.2). The stable id is the genesis revision's
// hash — stable forever, because the genesis revision is immutable — while the
// ref value tracks the current head.
//
// There is no new machinery here. It is the same construction as a branch over
// commits, and it was already implemented once, for tickets. It lives in its own
// package now because the context graph needs it for identity nodes too — a
// symbol or a file has a life across renames in exactly the way a ticket has a
// life across revisions (GRAPH.md §3.1) — and two implementations of one
// construction is how they come to disagree.
//
// What this package deliberately does not know: what a revision *means*. State
// is opaque text. A ticket reads it as a spec; a file identity reads it as a
// path; a symbol identity reads it as a name. The core stores and chains it and
// never interprets it, which is the same discipline that keeps edge types out of
// the core (GRAPH.md §11.2).
package idchain

import (
	"crypto/ed25519"
	"fmt"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// Ref returns the identity ref for an id under a namespace prefix. The prefix is
// the caller's reserved namespace (refs/varvig/tickets/, refs/varvig/nodes/, …);
// this package neither owns nor enumerates them.
func Ref(prefix string, id multihash.Multihash) string { return prefix + id.Hex() }

// New mints an identity: a signed, unmaterialized genesis revision carrying
// state, plus a ref pointing at it. The returned id is the identity's stable
// name for its whole life, and never moves again.
func New(r *repo.Repo, prefix, state string, priv ed25519.PrivateKey, author string, now int64) (multihash.Multihash, error) {
	if err := validPrefix(prefix); err != nil {
		return nil, err
	}
	if state == "" {
		return nil, fmt.Errorf("idchain: a genesis revision needs state")
	}
	rev, err := BuildRevision(r, state, nil, priv, author, now)
	if err != nil {
		return nil, err
	}
	if err := r.Refs.Create(Ref(prefix, rev), rev, author, "idchain new"); err != nil {
		return nil, err
	}
	return rev, nil
}

// Revise appends a revision (parent = the current head) and moves the ref by
// compare-and-swap. The reflog records the move, so a bad edit is recoverable by
// construction.
//
// Nothing bound to the old revision follows. That is the property the whole
// construction exists for: an approval stays attached to the spec that was
// signed (tickets §2.2), and an edge stays attached to the name a symbol had
// when the edge was observed (GRAPH.md §3.1). A rename appends; it never
// retargets.
func Revise(r *repo.Repo, prefix string, id multihash.Multihash, state string, priv ed25519.PrivateKey, author string, now int64) (multihash.Multihash, error) {
	if err := validPrefix(prefix); err != nil {
		return nil, err
	}
	if state == "" {
		return nil, fmt.Errorf("idchain: a revision needs state")
	}
	name := Ref(prefix, id)
	head, err := r.Refs.Resolve(name)
	if err != nil {
		return nil, fmt.Errorf("idchain: %s: %w", id.Hex(), err)
	}
	rev, err := BuildRevision(r, state, []multihash.Multihash{head}, priv, author, now)
	if err != nil {
		return nil, err
	}
	if err := r.Refs.CompareAndSwap(name, head, rev, author, "idchain revise"); err != nil {
		return nil, err
	}
	return rev, nil
}

// Head returns the current revision of an identity.
func Head(r *repo.Repo, prefix string, id multihash.Multihash) (multihash.Multihash, error) {
	return r.Refs.Resolve(Ref(prefix, id))
}

// StateAt returns the state a specific revision carries. It takes a revision
// hash, not an identity, because that is the whole point: asking what an
// identity means *now* and asking what it meant when something was bound to it
// are different questions, and only the second one is stable.
func StateAt(r *repo.Repo, revision multihash.Multihash) (string, error) {
	obj, err := r.Objects.Get(revision)
	if err != nil {
		return "", err
	}
	c, err := obj.AsChange()
	if err != nil {
		return "", fmt.Errorf("idchain: revision %s is not a revision: %w", revision.Hex(), err)
	}
	return c.Message, nil
}

// History walks an identity's revision chain from head to genesis, newest first.
// The last entry is the genesis revision, whose hash is the identity's id.
func History(r *repo.Repo, prefix string, id multihash.Multihash) ([]multihash.Multihash, error) {
	head, err := Head(r, prefix, id)
	if err != nil {
		return nil, err
	}
	var out []multihash.Multihash
	cur := head
	for cur != nil {
		out = append(out, cur)
		obj, err := r.Objects.Get(cur)
		if err != nil {
			return nil, err
		}
		c, err := obj.AsChange()
		if err != nil {
			return nil, fmt.Errorf("idchain: revision %s is not a revision: %w", cur.Hex(), err)
		}
		if len(c.Parents) == 0 {
			break
		}
		cur = c.Parents[0]
	}
	return out, nil
}

// List returns every identity id under a prefix, sorted by id. A ref nested
// deeper than one segment below the prefix is skipped, so an identity that later
// grows sub-refs is still listed once.
func List(r *repo.Repo, prefix string) ([]multihash.Multihash, error) {
	if err := validPrefix(prefix); err != nil {
		return nil, err
	}
	names, err := r.Refs.List()
	if err != nil {
		return nil, err
	}
	var out []multihash.Multihash
	for _, n := range names {
		suffix, ok := strings.CutPrefix(n, prefix)
		if !ok || strings.Contains(suffix, "/") {
			continue
		}
		id, err := multihash.ParseHex(suffix)
		if err != nil {
			continue
		}
		out = append(out, id)
	}
	sortIDs(out)
	return out, nil
}

// BuildRevision creates a signed, unmaterialized revision carrying state and
// provenance, and stores it. No tree: a revision is intent without a
// materialization (tickets D1).
func BuildRevision(r *repo.Repo, state string, parents []multihash.Multihash, priv ed25519.PrivateKey, author string, now int64) (multihash.Multihash, error) {
	provID, err := r.Objects.Put(object.NewProvenance(provenance.Build(author)))
	if err != nil {
		return nil, err
	}
	rev := object.NewChange(object.Change{
		Parents:    parents,
		Message:    state,
		Timestamp:  now,
		Author:     author,
		Provenance: provID,
	})
	if err := provenance.Sign(rev, priv); err != nil {
		return nil, err
	}
	return r.Objects.Put(rev)
}

func validPrefix(prefix string) error {
	if !strings.HasPrefix(prefix, "refs/") || !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("idchain: prefix %q must be a ref namespace ending in /", prefix)
	}
	return nil
}

func sortIDs(ids []multihash.Multihash) {
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j].Hex() < ids[j-1].Hex(); j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
}
