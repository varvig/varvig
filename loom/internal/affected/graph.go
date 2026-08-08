package affected

import (
	"sort"

	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
)

// Graph is the file-dependency graph of one tree: which files import which, and
// the reverse (who depends on a file). The reverse edges drive the affected-set
// query.
type Graph struct {
	Files      map[string]multihash.Multihash
	deps       map[string][]string // path -> files it imports
	dependents map[string][]string // path -> files that import it
}

// BuildGraph analyzes every file in a tree and resolves its intra-repo
// dependencies into edges. Per-file analysis is cached by blob id (content
// only), so an unchanged file across commits is never re-analyzed — the
// content-addressed, incremental property of design §1.3. A nil cache disables
// caching.
func BuildGraph(objs ObjectStore, treeID multihash.Multihash, cache Cache) (*Graph, error) {
	files, err := FlattenTree(objs, treeID)
	if err != nil {
		return nil, err
	}
	pathset := make(map[string]bool, len(files))
	for p := range files {
		pathset[p] = true
	}
	g := &Graph{
		Files:      files,
		deps:       map[string][]string{},
		dependents: map[string][]string{},
	}
	for p, blobID := range files {
		specs, err := specifiersFor(objs, p, blobID, cache)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, spec := range specs {
			target, ok := resolve(p, spec, pathset)
			if !ok || target == p || seen[target] {
				continue
			}
			seen[target] = true
			g.deps[p] = append(g.deps[p], target)
			g.dependents[target] = append(g.dependents[target], p)
		}
	}
	for k := range g.deps {
		sort.Strings(g.deps[k])
	}
	for k := range g.dependents {
		sort.Strings(g.dependents[k])
	}
	return g, nil
}

func specifiersFor(objs ObjectStore, p string, blobID multihash.Multihash, cache Cache) ([]string, error) {
	if cache != nil {
		if specs, ok := cache.Get(blobID); ok {
			return specs, nil
		}
	}
	obj, err := objs.Get(blobID)
	if err != nil {
		return nil, err
	}
	content, _ := obj.BlobContent()
	specs := extractSpecifiers(p, content)
	if cache != nil {
		cache.Put(blobID, specs)
	}
	return specs, nil
}

// Dependents returns the files that directly import p.
func (g *Graph) Dependents(p string) []string { return g.dependents[p] }

// Deps returns the files p directly imports.
func (g *Graph) Deps(p string) []string { return g.deps[p] }

// Affected returns the transitive closure of changed files plus everything that
// (transitively) depends on them — the set whose behavior the change may alter.
// Changed files with no analyzer still appear (textual fallback); they simply
// have no dependents to pull in.
func (g *Graph) Affected(changed []string) []string {
	seen := map[string]bool{}
	var queue []string
	for _, c := range changed {
		if !seen[c] {
			seen[c] = true
			queue = append(queue, c)
		}
	}
	for len(queue) > 0 {
		p := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, dep := range g.dependents[p] {
			if !seen[dep] {
				seen[dep] = true
				queue = append(queue, dep)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
