package graphquery

import (
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/core"
	"github.com/dividebyzero/claude-experiments/varvig/internal/edge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// Query is a graph read over one tree. It builds the derived edges once and
// answers from them, so coverage is computed from what actually ran rather than
// re-derived per question — the design assumes coverage is cheap metadata, and
// holding it on the query is what makes that true.
type Query struct {
	r    *repo.Repo
	tree multihash.Multihash

	edges     []edge.DerivedEdge
	coverage  core.Coverage
	analyzers []core.AnalyzerRef

	// dependsOn and dependedOnBy index the derived edges by resolution path.
	dependsOn    map[string][]edge.DerivedEdge
	dependedOnBy map[string][]edge.DerivedEdge
	// blobOf maps a path to the content it resolved to under this tree, which is
	// the anchor stored edges attach to.
	blobOf map[string]multihash.Multihash
}

// New opens a query over a tree or change.
func New(r *repo.Repo, treeOrChange multihash.Multihash) (*Query, error) {
	tree, err := core.TreeOf(r, treeOrChange)
	if err != nil {
		return nil, err
	}
	res, err := core.DerivedEdges(r, tree)
	if err != nil {
		return nil, err
	}
	files, err := core.TreeFiles(r, tree)
	if err != nil {
		return nil, err
	}
	q := &Query{
		r: r, tree: tree,
		edges: res.Edges, coverage: res.Coverage, analyzers: res.Analyzers,
		dependsOn:    map[string][]edge.DerivedEdge{},
		dependedOnBy: map[string][]edge.DerivedEdge{},
		blobOf:       files,
	}
	for _, e := range res.Edges {
		q.dependsOn[e.SourcePath()] = append(q.dependsOn[e.SourcePath()], e)
		q.dependedOnBy[e.TargetPath()] = append(q.dependedOnBy[e.TargetPath()], e)
	}
	return q, nil
}

// Coverage is the query's coverage descriptor, available before any question is
// asked so a caller can decide whether the answers are worth having.
func (q *Query) Coverage() core.Coverage { return q.coverage }

// Dependencies returns what a file depends on: the derived edges out of it, plus
// any stored edges anchored on its content, partitioned by class.
func (q *Query) Dependencies(path string) (Result, error) {
	return q.resultFor(path, q.dependsOn[path])
}

// Dependents returns what depends on a file.
func (q *Query) Dependents(path string) (Result, error) {
	return q.resultFor(path, q.dependedOnBy[path])
}

// resultFor assembles a partitioned result. Stored edges are read from the note
// anchored on the file's content and split by their recorded class; there is no
// path here that mixes them with the derived ones.
func (q *Query) resultFor(path string, derived []edge.DerivedEdge) (Result, error) {
	var imported, asserted []edge.Entry
	if blob, ok := q.blobOf[path]; ok {
		stored, err := edge.List(q.r, blob)
		if err != nil {
			return Result{}, err
		}
		for _, e := range stored {
			switch e.Edge.Provenance().Class {
			case edge.Imported:
				imported = append(imported, e)
			case edge.Asserted:
				asserted = append(asserted, e)
			}
		}
	}
	return newResult(derived, imported, asserted, q.coverage, q.analyzers), nil
}

// Edge answers whether a dependency edge holds from one file to another, and
// answers it three ways.
//
// The distinction it exists for: "no edge" is a fact about the code only when an
// analyzer read both endpoints. When one of them is a language nothing parses,
// there is no edge *recorded* and no edge *ruled out*, and reporting those the
// same way is how a caller concludes there is no dependency when there may be
// one (§5, §11.4).
func (q *Query) Edge(from, to string) (Answer, error) {
	subject := fmt.Sprintf("%s -> %s", from, to)
	for _, e := range q.dependsOn[from] {
		if e.TargetPath() == to {
			return present(subject), nil
		}
	}
	// No edge recorded. Whether that is absence or ignorance depends on whether
	// anything read the files.
	for _, p := range []string{from, to} {
		if _, ok := q.blobOf[p]; !ok {
			return unknown(subject, fmt.Sprintf("%s is not in this tree", p)), nil
		}
		if !core.PathCovered(q.analyzers, p) {
			return unknown(subject, fmt.Sprintf("no analyzer covers %s", p)), nil
		}
	}
	return absent(subject), nil
}
