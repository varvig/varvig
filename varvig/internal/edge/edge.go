// Package edge is the context graph's edge records (GRAPH.md §2, §3.2).
//
// Edges are not uniform, and treating them uniformly is what turns a graph into
// a second source of truth. There are three kinds and they have different homes:
//
//	derived    Commit changes File, File defines Symbol   index only, never stored
//	imported   a foreign row bound to a varvig object      a note, weak strength
//	asserted   an agent's claim about the code             a note, with provenance
//
// This package holds the two that are storable and, just as importantly, makes
// the third unstorable. DerivedEdge and StoredEdge are distinct types with no
// conversion between them: no function here takes a DerivedEdge and produces a
// note, or produces a StoredEdge, or reads one to build the other. Persisting a
// derived edge is a compile error rather than a review catch (GRAPH.md §11.1).
//
// The type separation is the half a compiler enforces. The other half is that
// StoredEdge cannot even *spell* derived provenance: Class has exactly two
// values, Imported and Asserted, and no third. So the standing invariant "the
// object store contains zero notes carrying derived provenance" is not a CI
// sweep that could miss a case — there is no way to write one down.
//
// Edge type is an opaque, producer-qualified string. The core validates its
// shape and never its value: nothing in storage, sync, or retention branches on
// what an edge type says. There is no registry and no enumeration, and the
// absence of a list is the mitigation (§11.2) — an unknown type from a newer
// producer round-trips through an older binary untouched.
package edge

import (
	"fmt"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/graphnode"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
)

// Class is the provenance class of a *stored* edge.
//
// It has two values and will never have three. A derived edge has no stored
// representation at all, so there is nothing for a third constant to describe —
// and adding one would be the moment the graph became a second source of truth,
// because it would let a claim that no peer can recompute be written down as
// though it were reproducible fact.
type Class uint8

const (
	// Imported: an edge a connector brought in from a foreign system. Its
	// authority is the connector, which holds no key of its own, so it is always
	// weak (tickets §2.4).
	Imported Class = iota + 1
	// Asserted: an edge some principal claims holds. It may inform planning; it
	// may never change a merge outcome (§11.3).
	Asserted
)

func (c Class) String() string {
	switch c {
	case Imported:
		return "imported"
	case Asserted:
		return "asserted"
	}
	return "unknown"
}

// Strength is the attestation strength of the producing principal, reused from
// governance rather than grown in parallel (§11.3).
type Strength = object.Strength

// Provenance answers "how much should I trust this edge", and answers it with
// facts that can be checked rather than with a number.
//
// There are deliberately no confidence scores. A float is unfalsifiable and
// immediately Goodharted — §5 already names that failure mode for tests, and a
// scored graph invites it at a larger blast radius. Where ranking is genuinely
// needed, derive it from the class at query time rather than storing an opinion
// (GRAPH.md §4).
type Provenance struct {
	// Class is imported or asserted. Never derived; see the package doc.
	Class Class
	// Principal is the producing principal: the connector for an imported edge,
	// the asserting party for an asserted one.
	Principal string
	// Module is the content hash of a module the producer ran, where one applies.
	// It never makes an edge derived — a derived edge is not stored at all — but
	// it lets a reader find what produced this claim.
	Module multihash.Multihash
	// Strength is recorded at write time and never upgraded, exactly as an
	// attestation's is (tickets §2.4).
	Strength Strength
	// Model, ModelVersion and Sampling record an inference that produced an
	// asserted edge (§2.1). Empty for an imported edge, which ran no model.
	Model        string
	ModelVersion string
	Sampling     string
}

// Validity is the range of history over which an edge was observed to hold.
// From is where it was first seen; To is where it stopped, and is nil while the
// edge still holds. This is what makes semantic blame answerable rather than
// approximate (design §1.6).
type Validity struct {
	From multihash.Multihash
	To   multihash.Multihash
}

// Spec is the input to a stored edge. It is a separate type from StoredEdge so
// that a StoredEdge can only come from New, with its fields unexported and
// already validated — the same discipline as a graph node.
type Spec struct {
	Source        graphnode.Node
	Target        graphnode.Node
	Type          string
	ObservedUnder multihash.Multihash
	Provenance    Provenance
	Validity      Validity
}

// StoredEdge is an edge that lives in the object store, as a note. It is the
// only type the note writer accepts.
type StoredEdge struct {
	source        graphnode.Node
	target        graphnode.Node
	edgeType      string
	observedUnder multihash.Multihash
	provenance    Provenance
	validity      Validity
	// unknown carries fields written by a newer producer that this binary does
	// not model, so they survive a read-and-rewrite untouched (§4.4, A3).
	unknown map[string]rawField
	// unknownProv is the same, one level down, for the provenance object — the
	// field most likely to grow.
	unknownProv map[string]rawField
}

// New validates a spec and returns a stored edge. It is the only way to make
// one, so every StoredEdge in existence has been through these checks.
func New(s Spec) (StoredEdge, error) {
	if s.Source == nil || s.Target == nil {
		return StoredEdge{}, fmt.Errorf("edge: an edge needs both endpoints")
	}
	if err := validType(s.Type); err != nil {
		return StoredEdge{}, err
	}
	if len(s.ObservedUnder) == 0 {
		return StoredEdge{}, fmt.Errorf("edge: an edge must record the tree or commit it was observed under")
	}
	switch s.Provenance.Class {
	case Imported:
		// A connector signs on behalf of a principal who holds no key, so it can
		// only ever produce weak. Enforcing it here rather than at the bridge
		// makes G3's rule true under any input, including a compromised bridge.
		if s.Provenance.Strength != object.StrengthWeak {
			return StoredEdge{}, fmt.Errorf(
				"edge: an imported edge is always weak, not %s", s.Provenance.Strength)
		}
		if s.Provenance.Model != "" || s.Provenance.ModelVersion != "" || s.Provenance.Sampling != "" {
			return StoredEdge{}, fmt.Errorf("edge: an imported edge ran no model; model provenance belongs to an assertion")
		}
	case Asserted:
		if s.Provenance.Strength == object.StrengthUnknown {
			return StoredEdge{}, fmt.Errorf("edge: an asserted edge must record its strength")
		}
	default:
		return StoredEdge{}, fmt.Errorf("edge: provenance class is required (imported or asserted)")
	}
	if s.Provenance.Principal == "" {
		return StoredEdge{}, fmt.Errorf("edge: an edge must name the principal that produced it")
	}
	return StoredEdge{
		source:        s.Source,
		target:        s.Target,
		edgeType:      s.Type,
		observedUnder: s.ObservedUnder,
		provenance:    s.Provenance,
		validity:      s.Validity,
	}, nil
}

// Accessors. A stored edge is immutable once built; there are no setters.
func (e StoredEdge) Source() graphnode.Node             { return e.source }
func (e StoredEdge) Target() graphnode.Node             { return e.target }
func (e StoredEdge) Type() string                       { return e.edgeType }
func (e StoredEdge) ObservedUnder() multihash.Multihash { return e.observedUnder }
func (e StoredEdge) Provenance() Provenance             { return e.provenance }
func (e StoredEdge) Validity() Validity                 { return e.validity }

// Retention says whether writing this edge pins its endpoints or lets them go
// with the thing they describe. It is computed from endpoint class (GRAPH.md
// §11.5) — a writer cannot mark an ephemeral edge durable, correctly or
// otherwise — and an edge touching an ephemeral endpoint is collectable with it.
func (e StoredEdge) Retention() graphnode.Retention {
	if graphnode.RetentionOf(e.source) == graphnode.Collectable ||
		graphnode.RetentionOf(e.target) == graphnode.Collectable {
		return graphnode.Collectable
	}
	return graphnode.Durable
}

// validType checks an edge type's *shape*, never its value.
//
// The distinction matters and is easy to lose: requiring "producer:verb" is a
// format rule the design states (§11.2), while looking at which producer or
// which verb would be the ossification event. Nothing in this package, or
// anywhere in storage, sync, or retention, may compare an edge type against a
// known value — a static guard in this package's tests enforces that.
func validType(t string) error {
	producer, verb, ok := strings.Cut(t, ":")
	if !ok {
		return fmt.Errorf("edge: type %q must be producer-qualified, as in producer:verb", t)
	}
	if producer == "" || verb == "" {
		return fmt.Errorf("edge: type %q needs both a producer and a verb", t)
	}
	if strings.ContainsAny(t, " \t\n\x00") {
		return fmt.Errorf("edge: type %q contains whitespace", t)
	}
	return nil
}
