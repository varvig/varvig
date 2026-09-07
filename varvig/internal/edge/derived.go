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
	sourcePath    string
	targetPath    string
	edgeType      string
	observedUnder multihash.Multihash
	analyzers     []multihash.Multihash
}

// DerivedSpec is the input to Derived, mirroring Spec so the two kinds of edge
// are built the same way even though only one of them can be stored.
type DerivedSpec struct {
	Source graphnode.Node
	Target graphnode.Node
	// SourcePath and TargetPath are the endpoints' *resolutions* under
	// ObservedUnder — where each node was found in that tree.
	//
	// They are recorded rather than used as identity, which is the distinction
	// GRAPH.md §3.1 draws: `src/auth.ts` is not a node, it is one resolution of
	// a node, true under this tree and possibly false under the next. The node
	// stays the content hash; the path is how a human finds it.
	SourcePath string
	TargetPath string
	Type       string
	// ObservedUnder is the tree the analysis ran against.
	ObservedUnder multihash.Multihash
	// Analyzers is the reproduction recipe: the module hashes in force.
	Analyzers []multihash.Multihash
}

// Derived builds a derived edge. It is called by the index; nothing else has a
// reason to. Unlike New it takes no provenance, because there is none to take:
// a derived edge's warrant is that anyone can recompute it.
func Derived(s DerivedSpec) (DerivedEdge, error) {
	if s.Source == nil || s.Target == nil {
		return DerivedEdge{}, fmt.Errorf("edge: a derived edge needs both endpoints")
	}
	if err := validType(s.Type); err != nil {
		return DerivedEdge{}, err
	}
	if len(s.ObservedUnder) == 0 {
		return DerivedEdge{}, fmt.Errorf("edge: a derived edge must record the tree it was observed under")
	}
	if s.SourcePath == "" || s.TargetPath == "" {
		return DerivedEdge{}, fmt.Errorf("edge: a derived edge must record where each endpoint resolved")
	}
	return DerivedEdge{
		source:        s.Source,
		target:        s.Target,
		sourcePath:    s.SourcePath,
		targetPath:    s.TargetPath,
		edgeType:      s.Type,
		observedUnder: s.ObservedUnder,
		analyzers:     append([]multihash.Multihash(nil), s.Analyzers...),
	}, nil
}

// SourcePath and TargetPath are where the endpoints resolved under
// ObservedUnder. Two distinct paths can share a content hash, so the path is
// what tells a reader which file is meant — and it is only meaningful together
// with ObservedUnder.
func (e DerivedEdge) SourcePath() string { return e.sourcePath }
func (e DerivedEdge) TargetPath() string { return e.targetPath }

func (e DerivedEdge) Source() graphnode.Node             { return e.source }
func (e DerivedEdge) Target() graphnode.Node             { return e.target }
func (e DerivedEdge) Type() string                       { return e.edgeType }
func (e DerivedEdge) ObservedUnder() multihash.Multihash { return e.observedUnder }

// Analyzers is the reproduction recipe: the module hashes that produced this
// edge. Empty means only built-in extraction ran.
func (e DerivedEdge) Analyzers() []multihash.Multihash {
	return append([]multihash.Multihash(nil), e.analyzers...)
}
