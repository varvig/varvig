package ticket

import (
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// This is the backward half of the ticket→commit link (tickets, "The Ticket →
// Commit Link", §3). The forward pointer (Change.Fulfills) is authoritative and
// lives in the signed commit; the reverse direction — a ticket to the commits
// that implement it — is *derived* by scanning, never stored, and rebuildable
// from the object store alone, like the affected-set index (§4.3).

// The implementation states a ticket can be in (§4). They are distinct from the
// governance states (pending/approved/vetoed, derived from attestations):
// governance is about whether the intent is blessed, implementation about
// whether code exists for it.
const (
	ImplOpen        = "open"        // no branch-reachable commit fulfills any revision
	ImplStale       = "stale"       // a commit implements a *superseded* revision
	ImplImplemented = "implemented" // a commit implements the *current* head revision
)

// Revisions returns the ticket's intent revision hashes, head first back to
// genesis. The intent chain is linear (each Revise parents the prior head), so
// this is a straight walk down first parents.
func Revisions(r *repo.Repo, id multihash.Multihash) ([]multihash.Multihash, error) {
	head, err := Head(r, id)
	if err != nil {
		return nil, err
	}
	var out []multihash.Multihash
	seen := map[string]bool{}
	for cur := head; cur != nil && !seen[cur.Hex()]; {
		seen[cur.Hex()] = true
		out = append(out, cur)
		obj, err := r.Objects.Get(cur)
		if err != nil {
			break
		}
		c, err := obj.AsChange()
		if err != nil || len(c.Parents) == 0 {
			break
		}
		cur = c.Parents[0]
	}
	return out, nil
}

// FulfillIndex maps an intent-revision hash to the branch-reachable commits that
// fulfill it (Change.Fulfills). It is the derived reverse index: built by walking
// every change reachable from refs/heads/*, it is a cache of the object store and
// may be rebuilt at will. Reachability from a branch is what separates a promoted
// commit from the speculative attempts that share a Fulfills value.
func FulfillIndex(r *repo.Repo) (map[string][]multihash.Multihash, error) {
	names, err := r.Refs.List()
	if err != nil {
		return nil, err
	}
	idx := map[string][]multihash.Multihash{}
	seen := map[string]bool{}
	var stack []multihash.Multihash
	for _, n := range names {
		if !strings.HasPrefix(n, "refs/heads/") {
			continue
		}
		head, err := r.Refs.Resolve(n)
		if err == nil && head != nil {
			stack = append(stack, head)
		}
	}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == nil || seen[id.Hex()] {
			continue
		}
		seen[id.Hex()] = true
		obj, err := r.Objects.Get(id)
		if err != nil {
			continue // a missing object (partial repo) is not fatal to the scan
		}
		c, err := obj.AsChange()
		if err != nil {
			continue
		}
		if c.Fulfills != nil {
			k := c.Fulfills.Hex()
			idx[k] = append(idx[k], id)
		}
		stack = append(stack, c.Parents...)
	}
	return idx, nil
}

// ImplementationFrom classifies a ticket against a prebuilt FulfillIndex and
// returns the implementing commits (those naming the head revision when
// implemented, or the superseded revisions when stale). Reuse the index across
// many tickets to avoid rescanning.
func ImplementationFrom(r *repo.Repo, id multihash.Multihash, idx map[string][]multihash.Multihash) (string, []multihash.Multihash, error) {
	revs, err := Revisions(r, id)
	if err != nil {
		return "", nil, err
	}
	if len(revs) == 0 {
		return ImplOpen, nil, nil
	}
	if commits := idx[revs[0].Hex()]; len(commits) > 0 {
		return ImplImplemented, commits, nil
	}
	var stale []multihash.Multihash
	for _, rev := range revs[1:] {
		stale = append(stale, idx[rev.Hex()]...)
	}
	if len(stale) > 0 {
		return ImplStale, stale, nil
	}
	return ImplOpen, nil, nil
}

// Implementation classifies a single ticket, building the index on the fly. For
// many tickets, prefer FulfillIndex + ImplementationFrom.
func Implementation(r *repo.Repo, id multihash.Multihash) (string, []multihash.Multihash, error) {
	idx, err := FulfillIndex(r)
	if err != nil {
		return "", nil, err
	}
	return ImplementationFrom(r, id, idx)
}

// fulfillingCommits returns every branch-reachable commit that fulfills any of
// the ticket's revisions (head or superseded), used to gather commit-produced
// artifacts for the ticket.
func fulfillingCommits(r *repo.Repo, id multihash.Multihash) ([]multihash.Multihash, error) {
	idx, err := FulfillIndex(r)
	if err != nil {
		return nil, err
	}
	revs, err := Revisions(r, id)
	if err != nil {
		return nil, err
	}
	var out []multihash.Multihash
	for _, rev := range revs {
		out = append(out, idx[rev.Hex()]...)
	}
	return out, nil
}
