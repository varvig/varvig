package graphquery

import (
	"github.com/dividebyzero/claude-experiments/varvig/internal/core"
	"github.com/dividebyzero/claude-experiments/varvig/internal/edge"
)

// Result is the answer to a graph query: edges, partitioned by provenance class,
// with the coverage that makes them safe to read.
//
// Every field is unexported and there is one constructor, which requires
// coverage. That is the whole mechanism behind "there is no result type without
// coverage" (GRAPH.md §11.4): a caller cannot build a Result by literal, cannot
// omit coverage, and cannot construct the tidier struct that would have made
// this optional.
type Result struct {
	derived   []edge.DerivedEdge
	imported  []edge.Entry
	asserted  []edge.Entry
	coverage  core.Coverage
	analyzers []core.AnalyzerRef
}

// newResult is the only constructor. Coverage is a required argument rather than
// a field a caller may forget.
func newResult(derived []edge.DerivedEdge, imported, asserted []edge.Entry,
	coverage core.Coverage, analyzers []core.AnalyzerRef) Result {
	return Result{
		derived: derived, imported: imported, asserted: asserted,
		coverage: coverage, analyzers: analyzers,
	}
}

// Derived edges are reproducible: re-run the analyzers and get them back. They
// are the only class permitted to gate anything (§11.3).
func (r Result) Derived() []edge.DerivedEdge { return r.derived }

// Imported edges are a foreign system's word, carried at weak strength.
func (r Result) Imported() []edge.Entry { return r.imported }

// Asserted edges are somebody's claim. They may inform planning; they may never
// change a merge outcome.
func (r Result) Asserted() []edge.Entry { return r.asserted }

// Coverage says how much of the tree an analyzer understood. It is always
// present, because a result without it cannot be read safely.
func (r Result) Coverage() core.Coverage { return r.coverage }

// Analyzers is the analyzer set the derived half was produced with — the
// reproduction recipe, and what coverage was computed from.
func (r Result) Analyzers() []core.AnalyzerRef { return r.analyzers }

// Class names an edge's provenance class in a merged listing.
type Class uint8

const (
	// ClassDerived: recomputable from content by a deterministic analyzer.
	ClassDerived Class = iota + 1
	// ClassImported: brought in by a connector, traceable to a foreign system.
	ClassImported
	// ClassAsserted: claimed by a principal, not reproducible by a peer.
	ClassAsserted
)

func (c Class) String() string {
	switch c {
	case ClassDerived:
		return "derived"
	case ClassImported:
		return "imported"
	case ClassAsserted:
		return "asserted"
	}
	return "unknown"
}

// There is deliberately no Merged() and no flat, unpartitioned listing.
//
// An earlier draft of this file had one, on the reasoning that a caller wanting
// a single list would otherwise build a worse one by hand. But the builder
// instructions are explicit — report the caller, do not add the API, the
// partition is the mitigation — and no caller ever needed it. An unused API that
// exists to be convenient is exactly how the distinction between a reproducible
// fact and an agent's claim gets flattened later. Add it when a real caller
// appears, and report that caller.

// Reproducible reports whether an edge of this class can be recomputed by any
// peer from content alone. Only a reproducible edge may gate a merge outcome
// (§11.3), and this is the predicate that says so — named, so a consumer that
// requires derived input asks a question rather than comparing a constant.
func (c Class) Reproducible() bool { return c == ClassDerived }

// GatingEdges returns only the edges permitted to change a merge outcome: the
// derived ones.
//
// It is a named function rather than a comment on Derived() because the rule it
// enforces is the one most likely to be broken by accident — a caller reaching
// for Merged() because it was convenient. Conflict detection, scheduling and
// promotion call this.
func (r Result) GatingEdges() []edge.DerivedEdge { return r.derived }
