package attest

import (
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/notes"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
)

// Status is the derived governance state of a single intent revision. It is
// computed from attestations, never stored (tickets §2.1).
type Status int

const (
	StatusPending  Status = iota // no decisive attestation yet
	StatusApproved               // an approval of sufficient strength, and no veto
	StatusVetoed                 // a veto against this revision
)

func (s Status) String() string {
	switch s {
	case StatusApproved:
		return "approved"
	case StatusVetoed:
		return "vetoed"
	default:
		return "pending"
	}
}

// Derive computes the status of one intent revision from the attestations bound
// to it, given the strength required to approve. A veto against the revision is
// decisive regardless of strength ordering — refusal is not something a higher
// approval outranks. Otherwise an approval satisfying required strength yields
// approved; anything else is pending.
//
// Because attestations bind to a specific hash, this is also the mechanism by
// which approval does not survive a spec edit: a new revision hash simply has
// no attestations, so it derives to pending (tickets §2.2).
func Derive(attestations []object.Attestation, required object.Strength) Status {
	approved := false
	for _, a := range attestations {
		switch a.Decision {
		case object.DecisionVeto:
			return StatusVetoed
		case object.DecisionApprove:
			if a.Strength.Satisfies(required) {
				approved = true
			}
		}
	}
	if approved {
		return StatusApproved
	}
	return StatusPending
}

// Attach stores a signed attestation as a note in the reserved varvig/attest
// namespace, keyed by the intent revision it targets. The note's target is the
// intent hash, so it pins that revision as a GC root (decision D4) and makes
// attestations listable by intent; the encoded attestation travels in the note
// payload and syncs like any other object. obj must be a signed attestation
// (see Sign).
func Attach(r *repo.Repo, obj *object.Object, author string, now int64) (multihash.Multihash, error) {
	if _, _, err := Verify(obj); err != nil {
		return nil, err
	}
	a, err := obj.AsAttestation()
	if err != nil {
		return nil, err
	}
	return notes.New(r).Add(reserved.NoteAttest, a.Target, obj.Encode(), author, now)
}

// Entry is a stored attestation: its verified decoded form plus the signer's
// public key.
type Entry struct {
	Attestation object.Attestation
	SignerKey   []byte
}

// List returns every attestation bound to intent whose signature verifies,
// newest first. An attestation whose signature does not verify is skipped
// rather than trusted — a note payload is not authority; the signature is.
func List(r *repo.Repo, intent multihash.Multihash) ([]Entry, error) {
	chain, err := notes.New(r).List(reserved.NoteAttest, intent)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, n := range chain {
		obj, err := object.Decode(n.Note.Payload)
		if err != nil {
			continue
		}
		pub, a, err := Verify(obj)
		if err != nil {
			continue
		}
		out = append(out, Entry{Attestation: a, SignerKey: pub})
	}
	return out, nil
}

// Attestations returns just the verified decoded attestations bound to intent.
func Attestations(r *repo.Repo, intent multihash.Multihash) ([]object.Attestation, error) {
	entries, err := List(r, intent)
	if err != nil {
		return nil, err
	}
	out := make([]object.Attestation, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Attestation)
	}
	return out, nil
}

// PromotionBlocked reports whether change (or any ancestor revision reachable
// through its parents) carries a veto, which makes every descendant
// unpromotable (tickets §2.3, §3.3). This is the veto half of the promotion
// policy checkpoint; the scoring/policy-module half (M1) layers on top.
//
// The walk is over the change DAG via parent links, so a veto lands on the
// exact revision it targeted and its effect flows forward to work that did not
// exist when the veto was signed.
func PromotionBlocked(r *repo.Repo, change multihash.Multihash) (bool, multihash.Multihash, error) {
	seen := map[string]bool{}
	stack := []multihash.Multihash{change}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		key := id.Hex()
		if seen[key] {
			continue
		}
		seen[key] = true

		atts, err := Attestations(r, id)
		if err != nil {
			return false, nil, err
		}
		for _, a := range atts {
			if a.Decision == object.DecisionVeto {
				return true, id, nil
			}
		}

		obj, err := r.Objects.Get(id)
		if err != nil {
			// A parent not present locally cannot be inspected; treat its
			// absence as non-blocking here — the closure check on promotion is
			// where an incomplete history fails loudly.
			continue
		}
		if obj.Type() != object.TypeChange {
			continue
		}
		c, err := obj.AsChange()
		if err != nil {
			return false, nil, err
		}
		stack = append(stack, c.Parents...)
	}
	return false, nil, nil
}
