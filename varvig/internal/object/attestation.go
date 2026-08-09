package object

import (
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// Decision is the kind of governance decision an attestation records
// (tickets §2.1). Status is never stored as a field anywhere; it is derived
// from the set of attestations bound to an intent revision.
type Decision uint64

const (
	DecisionUnknown       Decision = 0
	DecisionApprove       Decision = 1
	DecisionVeto          Decision = 2
	DecisionDelegate      Decision = 3
	DecisionRequestChange Decision = 4
)

func (d Decision) String() string {
	switch d {
	case DecisionApprove:
		return "approve"
	case DecisionVeto:
		return "veto"
	case DecisionDelegate:
		return "delegate"
	case DecisionRequestChange:
		return "request-change"
	default:
		return fmt.Sprintf("decision-%d", uint64(d))
	}
}

// Strength records how much a signature is worth (tickets §2.4). It is ordered
// weak < delegated < strong and is recorded at signing time; no code path ever
// upgrades it. A bridge asserting a keyless principal's workflow transition can
// only ever produce weak; a principal signing with their own key produces
// strong.
type Strength uint64

const (
	StrengthUnknown   Strength = 0
	StrengthWeak      Strength = 1
	StrengthDelegated Strength = 2
	StrengthStrong    Strength = 3
)

func (s Strength) String() string {
	switch s {
	case StrengthWeak:
		return "weak"
	case StrengthDelegated:
		return "delegated"
	case StrengthStrong:
		return "strong"
	default:
		return fmt.Sprintf("strength-%d", uint64(s))
	}
}

// Satisfies reports whether an attestation of strength s meets a requirement
// for at least required. weak never satisfies a strong requirement; there is
// deliberately no path that raises a lower strength to a higher one.
func (s Strength) Satisfies(required Strength) bool {
	if s == StrengthUnknown || required == StrengthUnknown {
		return false
	}
	return s >= required
}

// Attestation is the decoded view of a signed governance decision bound to a
// specific intent revision hash (tickets §2.1, §2.2). The binding is to Target
// exactly: editing the spec produces a new revision with a new hash, which no
// existing attestation covers, so an approval cannot silently carry forward.
type Attestation struct {
	Target    multihash.Multihash // the intent revision this decision binds to
	Decision  Decision
	Strength  Strength
	Timestamp int64
	Rationale string              // optional
	Policy    multihash.Multihash // policy module in force at signing, optional
}

// NewAttestation builds an unsigned attestation object. The signature is
// attached separately via SetSignature (see package attest), and SignableBytes
// excludes it so the signed bytes commit to Target, Decision, and Strength.
func NewAttestation(a Attestation) *Object {
	fields := []field{
		{tag: tagAttestTarget, val: append([]byte(nil), a.Target...)},
		{tag: tagAttestDecision, val: appendUvarint(nil, uint64(a.Decision))},
		{tag: tagAttestStrength, val: appendUvarint(nil, uint64(a.Strength))},
		{tag: tagAttestTimestamp, val: appendUvarint(nil, uint64(a.Timestamp))},
	}
	if a.Rationale != "" {
		fields = append(fields, field{tag: tagAttestRationale, val: []byte(a.Rationale)})
	}
	if a.Policy != nil {
		fields = append(fields, field{tag: tagAttestPolicy, val: append([]byte(nil), a.Policy...)})
	}
	return newObject(TypeAttestation, fields)
}

// AsAttestation decodes the typed view of an attestation object.
func (o *Object) AsAttestation() (Attestation, error) {
	if o.typ != TypeAttestation {
		return Attestation{}, fmt.Errorf("object: not an attestation (%s)", o.typ)
	}
	var a Attestation
	if v, ok := o.Field(tagAttestTarget); ok {
		a.Target = multihash.Multihash(append([]byte(nil), v...))
	}
	if v, ok := o.Field(tagAttestRationale); ok {
		a.Rationale = string(v)
	}
	if v, ok := o.Field(tagAttestPolicy); ok {
		a.Policy = multihash.Multihash(append([]byte(nil), v...))
	}
	dec, err := readUvarintField(o, tagAttestDecision)
	if err != nil {
		return Attestation{}, fmt.Errorf("%w: bad attestation decision", ErrMalformed)
	}
	a.Decision = Decision(dec)
	str, err := readUvarintField(o, tagAttestStrength)
	if err != nil {
		return Attestation{}, fmt.Errorf("%w: bad attestation strength", ErrMalformed)
	}
	a.Strength = Strength(str)
	ts, err := readUvarintField(o, tagAttestTimestamp)
	if err != nil {
		return Attestation{}, fmt.Errorf("%w: bad attestation timestamp", ErrMalformed)
	}
	a.Timestamp = int64(ts)
	return a, nil
}

// readUvarintField reads a whole-value uvarint field, requiring the field to be
// present and to consume its entire value (no trailing bytes).
func readUvarintField(o *Object, tag uint64) (uint64, error) {
	v, ok := o.Field(tag)
	if !ok {
		return 0, fmt.Errorf("%w: missing field %d", ErrMalformed, tag)
	}
	n, k, err := readUvarint(v)
	if err != nil {
		return 0, err
	}
	if k != len(v) {
		return 0, fmt.Errorf("%w: trailing bytes in field %d", ErrMalformed, tag)
	}
	return n, nil
}
