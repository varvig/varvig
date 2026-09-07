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

// Reproducible reports whether an edge of this class can be recomputed by any
// peer from content alone. Only a reproducible edge may gate a merge outcome
// (§11.3), and this is the predicate that says so — named, so a consumer that
// requires derived input asks a question rather than comparing a constant.
func (c Class) Reproducible() bool { return c == ClassDerived }

// Classified is one edge in a merged listing, still carrying its class. Merging
// does not erase provenance: it is the mixing that is dangerous, so a merged
// entry that lost its class would defeat the point of partitioning.
type Classified struct {
	Class Class
	// Derived is set when Class is ClassDerived; Stored otherwise. Exactly one
	// is meaningful, which is why the class travels with them.
	Derived edge.DerivedEdge
	Stored  edge.StoredEdge
}

// Merged flattens the partitions, and requires being asked.
//
// It exists because some callers genuinely want one list — a display, an
// export — and refusing outright would push them into building it themselves,
// worse. What it does not do is hide the distinction: every entry names its
// class, so a consumer that wanted derived edges and got an assertion can still
// tell, and a consumer that never looks had to write Merged() to get here.
func (r Result) Merged() []Classified {
	out := make([]Classified, 0, len(r.derived)+len(r.imported)+len(r.asserted))
	for _, e := range r.derived {
		out = append(out, Classified{Class: ClassDerived, Derived: e})
	}
	for _, e := range r.imported {
		out = append(out, Classified{Class: ClassImported, Stored: e.Edge})
	}
	for _, e := range r.asserted {
		out = append(out, Classified{Class: ClassAsserted, Stored: e.Edge})
	}
	return out
}

// GatingEdges returns only the edges permitted to change a merge outcome: the
// derived ones.
//
// It is a named function rather than a comment on Derived() because the rule it
// enforces is the one most likely to be broken by accident — a caller reaching
// for Merged() because it was convenient. Conflict detection, scheduling and
// promotion call this.
func (r Result) GatingEdges() []edge.DerivedEdge { return r.derived }
