package core

// Derived edges over the affected-set index (builder G4).
//
// There is one index, with two query surfaces. §1.3's affected-set index and the
// context graph's dependency edges compute overlapping facts from the same
// analyzers, so building them separately guarantees they disagree — which is why
// this reads the existing graph rather than growing a second one. `Affected`
// answers "what does this change reach"; `DerivedEdges` answers "what depends on
// what". Same index, same analyzers, same cache.
//
// Derived edges exist only in query results. They are never written: package
// edge has no function that turns one into a note, and no provenance class that
// could describe one. What they carry instead is the reproduction recipe — the
// analyzer modules in force and the tree they ran against — so a peer that
// disagrees re-runs them rather than adjudicating between two stored opinions
// (GRAPH.md §11.1).

import (
	"sort"

	"github.com/dividebyzero/claude-experiments/varvig/internal/affected"
	"github.com/dividebyzero/claude-experiments/varvig/internal/edge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/graphnode"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// EdgeTypeImportsFile is the edge the file-dependency index produces: this file
// imports that one.
//
// The granularity is in the name on purpose. The index is file-level — it
// resolves import directives to files, and in a package-oriented language to
// every file of the imported package — so it cannot say which *symbol* was
// used. A later symbol-level analyzer contributes `varvig:references-symbol`
// alongside this, additively: edge types are an open namespace with no registry
// (§11.2), so nothing has to be migrated to gain precision, and nothing silently
// changes meaning.
const EdgeTypeImportsFile = "varvig:imports-file"

// DerivedEdgesResult is the dependency edges of one tree, with the coverage that
// makes them safe to read.
//
// Coverage travels with the edges rather than beside them because the failure it
// prevents is specific: an unanalyzed language yields no edges, in a form
// indistinguishable from a genuine absence of dependency (§5, §11.4). A caller
// holding edges without coverage cannot tell those apart.
type DerivedEdgesResult struct {
	Edges    []edge.DerivedEdge
	Coverage Coverage
	// Analyzers is the analyzer set in force, which is also each edge's
	// reproduction recipe.
	Analyzers []AnalyzerRef
}

// DerivedEdges computes the file-dependency edges of a tree.
//
// The result is deterministic: edges are sorted by source path, then target
// path, then type, so a from-scratch rebuild and an incremental one produce the
// same sequence and can be compared directly. That comparison is what keeps
// "the index is disposable" true rather than aspirational.
func DerivedEdges(r *repo.Repo, treeOrChange multihash.Multihash) (DerivedEdgesResult, error) {
	tree, err := TreeOf(r, treeOrChange)
	if err != nil {
		return DerivedEdgesResult{}, err
	}
	graph, wasm, err := buildGraph(r, tree)
	if err != nil {
		return DerivedEdgesResult{}, err
	}

	analyzers := make([]multihash.Multihash, 0, len(wasm))
	for _, wa := range wasm {
		analyzers = append(analyzers, wa.ID)
	}

	var out []edge.DerivedEdge
	for path := range graph.Files {
		for _, dep := range graph.Deps(path) {
			e, err := derivedFileEdge(graph, path, dep, tree, analyzers)
			if err != nil {
				return DerivedEdgesResult{}, err
			}
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SourcePath() != out[j].SourcePath() {
			return out[i].SourcePath() < out[j].SourcePath()
		}
		if out[i].TargetPath() != out[j].TargetPath() {
			return out[i].TargetPath() < out[j].TargetPath()
		}
		return out[i].Type() < out[j].Type()
	})

	return DerivedEdgesResult{
		Edges:     out,
		Coverage:  coverageOf(graph.Files, analyzerExts(wasm)),
		Analyzers: analyzerRefs(wasm),
	}, nil
}

// derivedFileEdge builds one edge. The endpoints are the files' content hashes —
// immutable, so nothing can change underneath the edge — and the paths ride
// along as the resolution under this tree.
func derivedFileEdge(graph *affected.Graph, from, to string, tree multihash.Multihash, analyzers []multihash.Multihash) (edge.DerivedEdge, error) {
	src, err := graphnode.Object(graph.Files[from])
	if err != nil {
		return edge.DerivedEdge{}, err
	}
	dst, err := graphnode.Object(graph.Files[to])
	if err != nil {
		return edge.DerivedEdge{}, err
	}
	return edge.Derived(edge.DerivedSpec{
		Source:        src,
		Target:        dst,
		SourcePath:    from,
		TargetPath:    to,
		Type:          EdgeTypeImportsFile,
		ObservedUnder: tree,
		Analyzers:     analyzers,
	})
}
