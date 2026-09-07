package main

// The context-graph query verb. It is the CLI half of G6; the gate half is
// varvig_graph / varvig_graph_edge, and both render the same
// internal/graphquery results so an operator and an agent cannot be told
// different things about the same tree.
//
// Every result prints its coverage, and an incomplete one says so before the
// edges rather than after: a reader who stops at the first line should not come
// away with a confident answer the analysis cannot support (§5).

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/core"
	"github.com/dividebyzero/claude-experiments/varvig/internal/edge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/graphquery"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

func cmdGraph(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: varvig graph deps|rdeps <path> [<rev>] | varvig graph edge <from> <to> [<rev>]")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	switch args[0] {
	case "deps", "rdeps":
		if len(args) < 2 || len(args) > 3 {
			return fmt.Errorf("usage: varvig graph %s <path> [<rev>]", args[0])
		}
		rev, err := graphRev(r, args[2:])
		if err != nil {
			return err
		}
		q, err := graphquery.New(r, rev)
		if err != nil {
			return err
		}
		get := q.Dependencies
		label := "depends on"
		if args[0] == "rdeps" {
			get, label = q.Dependents, "depended on by"
		}
		res, err := get(args[1])
		if err != nil {
			return err
		}
		return printGraphResult(args[1], label, res)

	case "edge":
		if len(args) < 3 || len(args) > 4 {
			return errors.New("usage: varvig graph edge <from> <to> [<rev>]")
		}
		rev, err := graphRev(r, args[3:])
		if err != nil {
			return err
		}
		q, err := graphquery.New(r, rev)
		if err != nil {
			return err
		}
		ans, err := q.Edge(args[1], args[2])
		if err != nil {
			return err
		}
		fmt.Printf("%s: %s\n", ans.Subject(), ans.State())
		fmt.Printf("  %s\n", ans.Reason())
		printCoverage(q.Coverage())
		return nil
	}
	return fmt.Errorf("unknown graph subcommand %q", args[0])
}

// graphRev resolves the optional revision argument, defaulting to HEAD.
func graphRev(r *repo.Repo, args []string) (multihash.Multihash, error) {
	if len(args) == 1 {
		return resolve(r, args[0])
	}
	headRef, err := r.Head()
	if err != nil {
		return nil, err
	}
	return r.Refs.Resolve(headRef)
}

// printGraphResult renders a partitioned result. The classes are printed
// separately and labelled, because that is the distinction the partition exists
// to preserve: a derived edge is reproducible, an imported one is a foreign
// system's word, an asserted one is a claim.
func printGraphResult(path, label string, res graphquery.Result) error {
	printCoverage(res.Coverage())

	d := res.Derived()
	fmt.Printf("%s %s (derived, %d):\n", path, label, len(d))
	for _, e := range d {
		fmt.Printf("  %s  [%s]\n", otherEnd(path, e), e.Type())
	}

	if im := res.Imported(); len(im) > 0 {
		fmt.Printf("%s, imported (%d):\n", path, len(im))
		for _, en := range im {
			printStoredEdge(en.Edge)
		}
	}
	if as := res.Asserted(); len(as) > 0 {
		fmt.Printf("%s, asserted (%d):\n", path, len(as))
		for _, en := range as {
			printStoredEdge(en.Edge)
		}
	}
	return nil
}

// otherEnd names the endpoint that is not the queried path, by its resolution
// under the tree — the path is how a human finds the file, while the node itself
// is a content hash.
func otherEnd(path string, e edge.DerivedEdge) string {
	if e.SourcePath() == path {
		return e.TargetPath()
	}
	return e.SourcePath()
}

func printStoredEdge(e edge.StoredEdge) {
	p := e.Provenance()
	fmt.Printf("  %s -> %s  [%s] %s/%s by %s\n",
		e.Source().Key(), e.Target().Key(), e.Type(), p.Class, p.Strength, p.Principal)
}

// printCoverage prints the coverage descriptor, and prints it first when it is
// incomplete. An absent edge under incomplete coverage may be a gap in the
// tooling rather than a fact about the code, and a reader needs to know that
// before reading the list, not after.
func printCoverage(c core.Coverage) {
	if c.Complete() {
		fmt.Printf("coverage: complete (%d files analyzed)\n", c.Analyzed)
		return
	}
	fmt.Printf("coverage: INCOMPLETE — %d of %d files analyzed; no analyzer for %s\n",
		c.Analyzed, c.Analyzed+c.Unanalyzed,
		strings.Join(namedTypes(c.UnanalyzedExts), ", "))
	fmt.Printf("  an absent edge here may be an unanalyzed language, not an absence of dependency\n")
}
