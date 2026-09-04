package attest

import (
	"github.com/dividebyzero/claude-experiments/varvig/internal/identity"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
)

// Decision and Strength are the governance vocabulary, re-exported here so a
// caller names the domain (`attest.Approve`, `attest.Strong`) rather than the
// wire-format package (design addendum, U1: the shells hold no object-store
// vocabulary). They are type aliases, so every existing attest function that
// takes an object.Decision / object.Strength accepts these unchanged.
type (
	Decision = object.Decision
	Strength = object.Strength
)

// Governance decisions.
const (
	DecisionUnknown = object.DecisionUnknown
	Approve         = object.DecisionApprove
	Veto            = object.DecisionVeto
	Delegate        = object.DecisionDelegate
	RequestChange   = object.DecisionRequestChange
)

// Attestation strengths, ordered.
const (
	StrengthUnknown = object.StrengthUnknown
	Weak            = object.StrengthWeak
	Delegated       = object.StrengthDelegated
	Strong          = object.StrengthStrong
)

// SignDecision builds and signs an attestation from the governance vocabulary,
// so a caller need not name the underlying object type. It is the authoring
// counterpart to Sign for callers outside the object package.
func SignDecision(signer identity.Signer, target multihash.Multihash, d Decision, s Strength, rationale string, now int64) (*object.Object, error) {
	return Sign(signer, object.Attestation{
		Target:    target,
		Decision:  d,
		Strength:  s,
		Timestamp: now,
		Rationale: rationale,
	})
}
