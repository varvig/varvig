package edge

import (
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/graphnode"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// DerivedEdge is an edge the index computed from content-addressed inputs by
// re-running a deterministic analyzer. It is produced only by the index and
// appears only in query results.
//
// It has no stored representation, and that is the point. Storing a derived edge
// creates a second source of truth that can disagree with the objects it was
// derived from, with nothing able to adjudicate which is right (GRAPH.md §11.1).
// So it is recomputed instead — which is exactly what §4.3 permits, because
// indices are caches and may churn freely when no identity depends on them.
//
// There is deliberately no function anywhere that turns one of these into a
// StoredEdge or into a note. Not an unexported one, not a helper, not a
// round-trip through a Spec. If you find yourself wanting one, the thing you
// have is an assertion, and it should be written as one with full provenance
// (§7: "the line is reproducibility, not intelligence").
//
// What replaces provenance here is the reproduction recipe: the analyzer modules
// that produced the edge and the tree they ran against. A peer that disagrees
// can re-run them and find out who is right, which is a stronger guarantee than
// any recorded opinion.
type DerivedEdge struct {
	source        graphnode.Node
	target        graphnode.Node
	edgeType      string
	observedUnder multihash.Multihash
	analyzers     []multihash.Multihash
}

// Derived builds a derived edge. It is called by the index; nothing else has a
// reason to. Unlike New it takes no provenance, because there is none to take:
// a derived edge's warrant is that anyone can recompute it.
func Derived(source, target graphnode.Node, edgeType string, observedUnder multihash.Multihash, analyzers []multihash.Multihash) (DerivedEdge, error) {
	if source == nil || target == nil {
		return DerivedEdge{}, fmt.Errorf("edge: a derived edge needs both endpoints")
	}
	if err := validType(edgeType); err != nil {
		return DerivedEdge{}, err
	}
	if len(observedUnder) == 0 {
		return DerivedEdge{}, fmt.Errorf("edge: a derived edge must record the tree it was observed under")
	}
	return DerivedEdge{
		source:        source,
		target:        target,
		edgeType:      edgeType,
		observedUnder: observedUnder,
		analyzers:     append([]multihash.Multihash(nil), analyzers...),
	}, nil
}

func (e DerivedEdge) Source() graphnode.Node             { return e.source }
func (e DerivedEdge) Target() graphnode.Node             { return e.target }
func (e DerivedEdge) Type() string                       { return e.edgeType }
func (e DerivedEdge) ObservedUnder() multihash.Multihash { return e.observedUnder }

// Analyzers is the reproduction recipe: the module hashes that produced this
// edge. Empty means only built-in extraction ran.
func (e DerivedEdge) Analyzers() []multihash.Multihash {
	return append([]multihash.Multihash(nil), e.analyzers...)
}
