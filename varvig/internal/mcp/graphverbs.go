package mcp

// The context-graph query verbs, through the gate. The CLI half is
// `varvig graph`; both render the same internal/graphquery results from the
// shared core, so an agent in a task checkout and an operator at a terminal
// cannot be told different things about the same tree (design addendum, U1).
//
// The gate adds three things and nothing else: scope confinement, an honest
// count of what confinement withheld, and the coverage descriptor that keeps an
// unanalyzed language from reading as an absence of dependency.
//
// Results stay partitioned. No verb here returns a flat edge list, because
// derived, imported and asserted edges carry different authority and an agent
// that receives them mixed will act on an assertion as though it were a
// reproducible fact (GRAPH.md §11.3).

import (
	"encoding/json"

	"github.com/dividebyzero/claude-experiments/varvig/internal/edge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/graphquery"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// graphQueryFor opens a query over the change under inspection, or the task's
// base when none is named.
func (g *Gate) graphQueryFor(changeArg string) (*graphquery.Query, multihash.Multihash, error) {
	id, err := g.resolveChange(changeArg)
	if err != nil {
		return nil, nil, err
	}
	q, err := graphquery.New(g.repo, id)
	if err != nil {
		return nil, nil, gerr(codeInternal, "cannot open a graph query: %v", err)
	}
	return q, id, nil
}

// toolGraph answers what a file depends on, or what depends on it.
func toolGraph(g *Gate, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Path      string `json:"path"`
		Direction string `json:"direction"`
		Change    string `json:"change"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	// The queried path is a targeted read, so it is refused outright when out of
	// scope — the same rule read_file follows. Only the *results* are filtered.
	path, err := g.resolvePath(a.Path)
	if err != nil {
		return nil, err
	}
	if a.Direction == "" {
		a.Direction = directionDependencies
	}
	if a.Direction != directionDependencies && a.Direction != directionDependents {
		return nil, gerr(codeInvalidArgs, "direction must be %q or %q, not %q",
			directionDependencies, directionDependents, a.Direction)
	}

	q, id, err := g.graphQueryFor(a.Change)
	if err != nil {
		return nil, err
	}
	get := q.Dependencies
	if a.Direction == directionDependents {
		get = q.Dependents
	}
	res, err := get(path)
	if err != nil {
		return nil, gerr(codeInternal, "graph query failed: %v", err)
	}

	derived, derivedOut := g.derivedPayload(path, res.Derived())

	return map[string]any{
		"base":      g.baseHex(),
		"change":    id.Hex(),
		"path":      path,
		"direction": a.Direction,
		// Partitioned, always. A caller that wants one list builds it, and can
		// still see which class each edge came from.
		"derived":  derived,
		"imported": g.storedPayload(res.Imported()),
		"asserted": g.storedPayload(res.Asserted()),
		// Only derived edges may gate a merge outcome; saying so in the payload
		// keeps the rule visible to the agent reading it.
		"gating_class": "derived",
		// Only the derived half can be withheld by scope: a stored edge's far
		// endpoint is a foreign identifier rather than a repo path, and its
		// varvig-side endpoint is the queried file itself, which is in scope by
		// the time the query runs. Reporting a constant zero for those would be
		// a number that looks like a measurement and is not one.
		"out_of_scope_derived": derivedOut,
		"coverage":             coveragePayload(res.Coverage()),
	}, nil
}

// toolGraphEdge answers whether a dependency edge holds, three ways.
func toolGraphEdge(g *Gate, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		From   string `json:"from"`
		To     string `json:"to"`
		Change string `json:"change"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	from, err := g.resolvePath(a.From)
	if err != nil {
		return nil, err
	}
	to, err := g.resolvePath(a.To)
	if err != nil {
		return nil, err
	}
	q, id, err := g.graphQueryFor(a.Change)
	if err != nil {
		return nil, err
	}
	ans, err := q.Edge(from, to)
	if err != nil {
		return nil, gerr(codeInternal, "graph query failed: %v", err)
	}
	return map[string]any{
		"base":   g.baseHex(),
		"change": id.Hex(),
		"from":   from,
		"to":     to,
		// Three-valued, never a boolean. "absent_under_coverage" is a fact about
		// the code; "unknown_outside_coverage" is a fact about the tooling, and
		// it is not evidence of absence.
		"state":    ans.State().String(),
		"reason":   ans.Reason(),
		"coverage": coveragePayload(q.Coverage()),
	}, nil
}

const (
	directionDependencies = "dependencies"
	directionDependents   = "dependents"
)

// derivedPayload renders the derived edges the task may see, and counts the
// rest. Paths outside scope are counted, never listed: a task needs to know its
// file is reached from code it cannot read without learning that code's layout.
func (g *Gate) derivedPayload(path string, es []edge.DerivedEdge) ([]map[string]any, int) {
	out := make([]map[string]any, 0, len(es))
	withheld := 0
	for _, e := range es {
		other := e.TargetPath()
		if e.SourcePath() != path {
			other = e.SourcePath()
		}
		if !g.grant.Covers(other) || g.deny.Denied(other) {
			withheld++
			continue
		}
		out = append(out, map[string]any{
			"path":           other,
			"type":           e.Type(),
			"node":           e.Target().Key(),
			"source_node":    e.Source().Key(),
			"observed_under": e.ObservedUnder().Hex(),
		})
	}
	return out, withheld
}

// storedPayload renders imported or asserted edges. Nothing is withheld here:
// their far endpoint is a foreign identifier, not a repo path, and their
// varvig-side endpoint is the file already established to be in scope.
func (g *Gate) storedPayload(es []edge.Entry) []map[string]any {
	out := make([]map[string]any, 0, len(es))
	for _, en := range es {
		p := en.Edge.Provenance()
		rec := map[string]any{
			"note":           en.Note.Hex(),
			"source":         en.Edge.Source().Key(),
			"target":         en.Edge.Target().Key(),
			"type":           en.Edge.Type(),
			"observed_under": en.Edge.ObservedUnder().Hex(),
			"provenance": map[string]any{
				"class":     p.Class.String(),
				"principal": p.Principal,
				"strength":  p.Strength.String(),
			},
		}
		if p.Model != "" {
			rec["provenance"].(map[string]any)["model"] = p.Model
			rec["provenance"].(map[string]any)["model_version"] = p.ModelVersion
			rec["provenance"].(map[string]any)["sampling"] = p.Sampling
		}
		out = append(out, rec)
	}
	return out
}
