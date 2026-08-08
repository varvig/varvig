package affected

import (
	"context"
	"sort"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// RepoContext is the tree-wide information resolution needs: every repo path,
// and the Go module path (for package-import resolution).
type RepoContext struct {
	Paths    map[string]bool
	GoModule string
}

// ModuleRunner runs a wasm analyzer module with input on stdin and returns its
// stdout. It is injected (the caller wires the wasm runtime), so this package
// stays free of any wasm dependency.
type ModuleRunner func(ctx context.Context, module, input []byte) ([]byte, error)

// WasmAnalyzer is a language analyzer supplied as a wasm module (design §3.3):
// new languages are learned without recompiling Varvig. It handles files with the
// given extension, receives {path, content} on stdin, and emits one specifier
// per line ("package <path>" for a package import, otherwise a path import).
type WasmAnalyzer struct {
	Ext    string              // e.g. ".rb" (lowercase, with dot)
	Module []byte              // the wasm module
	ID     multihash.Multihash // module identity, for cache keying
	Run    ModuleRunner
}

// Options configures graph construction.
type Options struct {
	Cache   Cache
	Context RepoContext    // Paths is filled in by BuildGraph; GoModule may be preset
	Wasm    []WasmAnalyzer // per-extension wasm analyzers
}

// Graph is the file-dependency graph of one tree.
type Graph struct {
	Files      map[string]multihash.Multihash
	deps       map[string][]string
	dependents map[string][]string
}

// BuildGraph analyzes every file in a tree and resolves its intra-repo
// dependencies into edges. Per-file analysis is cached by content (and, for a
// wasm analyzer, by the analyzer module too), so unchanged files are never
// re-analyzed (design §1.3, §4.3). A nil cache disables caching.
func BuildGraph(objs ObjectStore, treeID multihash.Multihash, opts Options) (*Graph, error) {
	files, err := FlattenTree(objs, treeID)
	if err != nil {
		return nil, err
	}
	pathset := make(map[string]bool, len(files))
	for p := range files {
		pathset[p] = true
	}
	ctx := opts.Context
	ctx.Paths = pathset
	if ctx.GoModule == "" {
		ctx.GoModule = detectGoModule(objs, files)
	}

	wasmByExt := map[string]*WasmAnalyzer{}
	for i := range opts.Wasm {
		wa := &opts.Wasm[i]
		wasmByExt[strings.ToLower(wa.Ext)] = wa
	}

	g := &Graph{Files: files, deps: map[string][]string{}, dependents: map[string][]string{}}
	for p, blobID := range files {
		specs, err := specifiersFor(objs, p, blobID, opts.Cache, wasmByExt)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, spec := range specs {
			for _, target := range resolveSpecifier(p, spec, ctx) {
				if target == p || seen[target] {
					continue
				}
				seen[target] = true
				g.deps[p] = append(g.deps[p], target)
				g.dependents[target] = append(g.dependents[target], p)
			}
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

func specifiersFor(objs ObjectStore, p string, blobID multihash.Multihash, cache Cache, wasmByExt map[string]*WasmAnalyzer) ([]Specifier, error) {
	ext := strings.ToLower(pathExt(p))
	wa := wasmByExt[ext]

	key := blobID
	if wa != nil {
		key = combinedKey(blobID, wa.ID)
	}
	if cache != nil {
		if enc, ok := cache.Get(key); ok {
			return decodeSpecs(enc), nil
		}
	}

	var specs []Specifier
	if wa != nil {
		out, err := runWasmAnalyzer(objs, p, blobID, wa)
		if err != nil {
			return nil, err
		}
		specs = out
	} else {
		obj, err := objs.Get(blobID)
		if err != nil {
			return nil, err
		}
		content, _ := obj.BlobContent()
		specs = extractBuiltin(p, content)
	}
	if cache != nil {
		cache.Put(key, encodeSpecs(specs))
	}
	return specs, nil
}

// Dependents returns the files that directly import p.
func (g *Graph) Dependents(p string) []string { return g.dependents[p] }

// Deps returns the files p directly imports.
func (g *Graph) Deps(p string) []string { return g.deps[p] }

// Affected returns the transitive closure of changed files plus everything that
// (transitively) depends on them.
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

func encodeSpecs(specs []Specifier) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.encode())
	}
	return out
}

func decodeSpecs(enc []string) []Specifier {
	out := make([]Specifier, 0, len(enc))
	for _, e := range enc {
		if s, ok := decodeSpecifier(e); ok {
			out = append(out, s)
		}
	}
	return out
}

func combinedKey(blobID, moduleID multihash.Multihash) multihash.Multihash {
	buf := append(append([]byte(nil), blobID...), moduleID...)
	k, _ := multihash.Sum(multihash.BLAKE3, buf)
	return k
}

func pathExt(p string) string {
	i := strings.LastIndexByte(p, '.')
	if i < 0 {
		return ""
	}
	if s := strings.LastIndexByte(p, '/'); s > i {
		return ""
	}
	return p[i:]
}

// detectGoModule reads the module path from a go.mod at the tree root, if any.
func detectGoModule(objs ObjectStore, files map[string]multihash.Multihash) string {
	id, ok := files["go.mod"]
	if !ok {
		return ""
	}
	obj, err := objs.Get(id)
	if err != nil {
		return ""
	}
	content, _ := obj.BlobContent()
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}
