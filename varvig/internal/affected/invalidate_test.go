package affected

import (
	"context"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// TestCacheKeyCoversTheBuiltinExtractor: an entry produced by an older built-in
// extractor must not be trusted by a newer one.
//
// The hole this closes was real and silent: a built-in analyzed file used to be
// keyed by its content alone, so a varvig upgrade that improved an extractor
// went on serving the old extractor's answers off disk forever, with nothing to
// notice. The poisoned entry below is exactly the shape that used to be trusted.
func TestCacheKeyCoversTheBuiltinExtractor(t *testing.T) {
	r, tree := treeFromFiles(t, map[string]string{
		"app.js":   "import './real.js'\n",
		"real.js":  "export const x = 1\n",
		"other.js": "export const y = 2\n",
	})
	files, err := FlattenTree(r.Objects, tree)
	if err != nil {
		t.Fatal(err)
	}
	cache := NewMemCache()
	// An entry keyed by content alone — what an older binary would have written.
	cache.Put(files["app.js"], []string{SpecPath + "\t./other.js"})

	g, err := BuildGraph(r.Objects, tree, Options{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range g.Deps("app.js") {
		if d == "other.js" {
			t.Fatalf("a cache entry keyed by content alone was trusted: app.js deps = %v",
				g.Deps("app.js"))
		}
	}
	if got := g.Deps("app.js"); len(got) != 1 || got[0] != "real.js" {
		t.Errorf("app.js deps = %v, want [real.js] from a fresh analysis", got)
	}
}

// TestChangingAnAnalyzerInvalidatesExactlyItsEntries is G4's acceptance: a new
// analyzer module hash must invalidate its own output and nothing else. The
// index is keyed per file, so a change to the .rb analyzer must not cost the
// .js entries their cache hits.
func TestChangingAnAnalyzerInvalidatesExactlyItsEntries(t *testing.T) {
	r, tree := treeFromFiles(t, map[string]string{
		"app.js": "import './lib.js'\n",
		"lib.js": "export const x = 1\n",
		"a.rb":   "require_relative 'b'\n",
		"b.rb":   "\n",
	})
	files, err := FlattenTree(r.Objects, tree)
	if err != nil {
		t.Fatal(err)
	}

	// A counting cache, so we can see exactly which entries missed.
	counted := &countingCache{inner: NewMemCache(), misses: map[string]int{}}
	rbV1 := stubAnalyzer(t, ".rb", "v1", "./b.rb")
	if _, err := BuildGraph(r.Objects, tree, Options{Cache: counted, Wasm: []WasmAnalyzer{rbV1}}); err != nil {
		t.Fatal(err)
	}
	firstMisses := counted.total()
	if firstMisses == 0 {
		t.Fatal("a cold cache must miss")
	}

	// Same analyzer, same tree: everything hits.
	counted.reset()
	if _, err := BuildGraph(r.Objects, tree, Options{Cache: counted, Wasm: []WasmAnalyzer{rbV1}}); err != nil {
		t.Fatal(err)
	}
	if n := counted.total(); n != 0 {
		t.Fatalf("a warm cache missed %d times", n)
	}

	// A new .rb analyzer. Only the two .rb files may miss.
	counted.reset()
	rbV2 := stubAnalyzer(t, ".rb", "v2", "./b.rb")
	if _, err := BuildGraph(r.Objects, tree, Options{Cache: counted, Wasm: []WasmAnalyzer{rbV2}}); err != nil {
		t.Fatal(err)
	}
	if n := counted.total(); n != 2 {
		t.Errorf("changing the .rb analyzer caused %d misses, want exactly 2 (a.rb, b.rb); "+
			"the .js entries must keep their cache hits", n)
	}
	_ = files
}

// countingCache counts misses so a test can assert which entries an analyzer
// change invalidated.
type countingCache struct {
	inner  Cache
	misses map[string]int
}

func (c *countingCache) Get(id multihash.Multihash) ([]string, bool) {
	v, ok := c.inner.Get(id)
	if !ok {
		c.misses[id.Hex()]++
	}
	return v, ok
}
func (c *countingCache) Put(id multihash.Multihash, specs []string) { c.inner.Put(id, specs) }
func (c *countingCache) reset()                                     { c.misses = map[string]int{} }
func (c *countingCache) total() int {
	n := 0
	for _, v := range c.misses {
		n += v
	}
	return n
}

// stubAnalyzer builds a wasm-shaped analyzer whose module bytes (and therefore
// its identity) depend on version, and which always emits one specifier. No
// real wasm runs: the runner is injected, which is why this package carries no
// wasm dependency.
func stubAnalyzer(t *testing.T, ext, version, emit string) WasmAnalyzer {
	t.Helper()
	mod := []byte("stub-analyzer-" + version)
	id, err := multihash.Sum(multihash.BLAKE3, mod)
	if err != nil {
		t.Fatal(err)
	}
	return WasmAnalyzer{
		Ext: ext, Module: mod, ID: id,
		Run: func(_ context.Context, _, _ []byte) ([]byte, error) { return []byte(emit + "\n"), nil },
	}
}
