// Package assertion is how the execution layer contributes what it believes
// (GRAPH.md §7, builder G7): an agent's claim that two modules are coupled, that
// a ticket duplicates another, that a helper is dead.
//
// The line between this and a derived edge is reproducibility, not intelligence.
// Core derives edges by re-running deterministic analyzers, and a peer that
// disagrees re-runs them and finds out who is right. An execution layer that
// discovers a relationship cannot offer that, so what it contributes is an
// *assertion*, carrying who said it and how — never a derived edge (§7).
//
// Three rules hold here, and each prevents a specific way this becomes a second
// source of truth:
//
//   - **An assertion never gates.** Conflict detection, scheduling and promotion
//     consume derived edges only. A wrong assertion costs wasted work, which is
//     acceptable; a bad merge is not (§11.3, §11.6).
//   - **Provenance, not confidence.** No scores. A float is unfalsifiable and
//     immediately Goodharted; the model, its version, and its sampling
//     parameters are checkable, and they say what a number cannot.
//   - **Promotion is signed.** An assertion becomes durable project knowledge by
//     the same machinery as a ticket approval: an attestation bound to the
//     edge's content hash, attributable later. There is a path from claim to
//     trusted; it is never implicit.
package assertion

import (
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/attest"
	"github.com/dividebyzero/claude-experiments/varvig/internal/edge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/graphnode"
	"github.com/dividebyzero/claude-experiments/varvig/internal/identity"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// Inference records how a model produced a claim (§2.1). Every field is
// required: an assertion whose model is unnamed cannot be audited, re-run, or
// distrusted selectively when that model turns out to be bad, and "some agent
// said so" is not provenance.
type Inference struct {
	Model        string
	ModelVersion string
	Sampling     string
}

// Claim is what an execution layer asserts.
type Claim struct {
	Source graphnode.Node
	Target graphnode.Node
	// Type is a producer-qualified, opaque edge type — `agent:couples-with`,
	// `agent:duplicates`. There is no registry and the core never reads it.
	Type string
	// ObservedUnder is the tree or commit the claim is about. A claim with no
	// subject cannot be checked against anything later.
	ObservedUnder multihash.Multihash
	// Principal is the asserting authority.
	Principal string
	// Strength is the authority behind the claim, from the same vocabulary as a
	// governance decision. It is recorded at write time and never upgraded.
	Strength object.Strength
}

// FromInference records a claim a model produced. The Inference is required, so
// a model-derived assertion cannot be written as though a person made it.
func FromInference(r *repo.Repo, c Claim, inf Inference, now int64) (multihash.Multihash, error) {
	if inf.Model == "" || inf.ModelVersion == "" || inf.Sampling == "" {
		return nil, fmt.Errorf("assertion: an inference must name its model, version, and sampling parameters")
	}
	return write(r, c, inf, now)
}

// FromPrincipal records a claim a person or a non-inferring process made, with
// no model provenance because none applies.
func FromPrincipal(r *repo.Repo, c Claim, now int64) (multihash.Multihash, error) {
	return write(r, c, Inference{}, now)
}

func write(r *repo.Repo, c Claim, inf Inference, now int64) (multihash.Multihash, error) {
	if c.Principal == "" {
		return nil, fmt.Errorf("assertion: a claim must name the principal asserting it")
	}
	if c.Strength == object.StrengthUnknown {
		return nil, fmt.Errorf("assertion: a claim must record the authority behind it")
	}
	e, err := edge.New(edge.Spec{
		Source:        c.Source,
		Target:        c.Target,
		Type:          c.Type,
		ObservedUnder: c.ObservedUnder,
		Provenance: edge.Provenance{
			// Not a parameter. Everything written here is an assertion; a derived
			// edge has no stored form at all.
			Class:        edge.Asserted,
			Principal:    c.Principal,
			Strength:     c.Strength,
			Model:        inf.Model,
			ModelVersion: inf.ModelVersion,
			Sampling:     inf.Sampling,
		},
	})
	if err != nil {
		return nil, err
	}
	// PutOnce: an agent that re-derives the same belief on every run should not
	// grow the note chain for it.
	id, _, err := edge.PutOnce(r, e, c.Principal, now)
	return id, err
}

// Promote makes an assertion durable project knowledge by signing an
// attestation that names the edge note's content hash.
//
// Binding to the note hash rather than to the edge's endpoints is A4 for the
// fifth time: the hash is the exact bytes that were read and approved, so a
// later assertion between the same endpoints is a different edge and does not
// inherit this approval. That is the same reason a ticket approval does not
// carry forward across a revision (tickets §2.2).
//
// This is the only path from claim to trusted, and it is signed and auditable.
// Nothing promotes implicitly, and no consumer treats an unpromoted assertion as
// anything but a claim.
func Promote(r *repo.Repo, noteID multihash.Multihash, signer identity.Signer, strength object.Strength, rationale string, now int64) (multihash.Multihash, error) {
	if noteID == nil {
		return nil, fmt.Errorf("assertion: promotion must name the edge it approves")
	}
	obj, err := attest.SignDecision(signer, noteID, attest.Approve, strength, rationale, now)
	if err != nil {
		return nil, err
	}
	return attest.Attach(r, obj, principalOf(signer), now)
}

// Promoted reports whether an edge note carries an approving attestation, and
// returns them so a caller can see who approved and how strongly.
//
// It answers a question, and answers it about one specific edge's bytes. A
// caller that wants to know whether *some* edge like this one was ever approved
// is asking a different, weaker question, and this deliberately does not answer
// it.
func Promoted(r *repo.Repo, noteID multihash.Multihash) (bool, []object.Attestation, error) {
	// attest.List skips any attestation whose signature does not verify — a note
	// payload is not authority, the signature is — so an unsigned or forged
	// promotion cannot make an assertion look approved.
	entries, err := attest.List(r, noteID)
	if err != nil {
		return false, nil, err
	}
	var approvals []object.Attestation
	for _, e := range entries {
		if e.Attestation.Decision == attest.Approve {
			approvals = append(approvals, e.Attestation)
		}
	}
	return len(approvals) > 0, approvals, nil
}

// principalOf names the signer for the note's author field. It is a label on the
// note, not authority: the authority is the signature the attestation carries.
func principalOf(signer identity.Signer) string {
	if signer == nil {
		return ""
	}
	return attest.Fingerprint(signer.Public())
}
