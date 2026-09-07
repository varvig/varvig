package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/hook"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// flatTree stores a flat tree of files (path -> content) plus a change over it,
// and returns the change id.
func flatTree(t *testing.T, r *repo.Repo, files map[string]string, parents ...multihash.Multihash) multihash.Multihash {
	t.Helper()
	entries := make([]object.Entry, 0, len(files))
	for name, content := range files {
		id, err := r.Objects.Put(object.NewBlob([]byte(content)))
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, object.Entry{
			Name: name, Mode: 0o100644, Kind: object.TypeBlob, ID: id,
		})
	}
	tree, err := r.Objects.Put(object.NewTree(entries))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := r.Objects.Put(object.NewChange(object.Change{
		Tree: tree, Parents: parents, Message: "m", Timestamp: 1,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

// TestAffectedPullsInDependents is the property the CLI verb has always had and
// the gate could not reach: a changed file drags its importers in with it.
// Path-style imports are used because they resolve to a single file, so a flat
// tree is enough to exercise a real edge.
func TestAffectedPullsInDependents(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := flatTree(t, r, map[string]string{
		"lib.js":   "export const x = 1\n",
		"app.js":   "import { x } from './lib.js'\n",
		"other.js": "export const y = 2\n",
	})
	next := flatTree(t, r, map[string]string{
		"lib.js":   "export const x = 2\n", // edited
		"app.js":   "import { x } from './lib.js'\n",
		"other.js": "export const y = 2\n",
	}, base)

	res, err := Affected(r, base, next)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) != 1 || res.Changed[0] != "lib.js" {
		t.Fatalf("Changed = %v, want [lib.js]", res.Changed)
	}
	// app.js imports lib.js, so editing lib.js affects app.js too.
	if !contains(res.Affected, "app.js") {
		t.Fatalf("Affected = %v, want app.js pulled in as a dependent of lib.js", res.Affected)
	}
	if !res.Pulled("app.js") {
		t.Error("Pulled(app.js) = false; it is in the set by dependency, not by edit")
	}
	// A changed file is never "pulled in" — it was edited directly.
	if res.Pulled("lib.js") {
		t.Error("Pulled(lib.js) = true for a directly changed file")
	}
	// An unrelated file is not affected at all.
	if contains(res.Affected, "other.js") {
		t.Errorf("Affected = %v, must not contain the unrelated other.js", res.Affected)
	}
}

// TestAffectedNilBaseIsAllAdditions: a nil base is the empty tree, so every file
// in the new tree is an addition rather than an error.
func TestAffectedNilBaseIsAllAdditions(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ch := flatTree(t, r, map[string]string{"a.go": "package a\n", "b.go": "package b\n"})
	res, err := Affected(r, nil, ch)
	if err != nil {
		t.Fatalf("Affected with a nil base must succeed: %v", err)
	}
	if len(res.Changed) != 2 {
		t.Fatalf("Changed = %v, want both files as additions", res.Changed)
	}
}

// TestAffectedIsIncrementalAndRebuildable pins the §4.3 property the whole design
// rests on: the index is a cache. Deleting it entirely must change no answer.
func TestAffectedIsIncrementalAndRebuildable(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := flatTree(t, r, map[string]string{"a.go": "package a\n"})
	next := flatTree(t, r, map[string]string{"a.go": "package a\n// edit\n", "b.go": "package b\n"}, base)

	first, err := Affected(r, base, next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(IndexDir(r), "deps")); err != nil {
		t.Fatalf("the specifier index was not written to IndexDir: %v", err)
	}

	// Warm: the same answer must come back off the cache.
	warm, err := Affected(r, base, next)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(first.Affected, warm.Affected) {
		t.Errorf("warm run disagreed with cold: %v vs %v", warm.Affected, first.Affected)
	}

	// Cold: delete the whole index and rebuild from scratch. Nothing is lost.
	if err := os.RemoveAll(IndexDir(r)); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := Affected(r, base, next)
	if err != nil {
		t.Fatalf("rebuild after deleting the index failed: %v", err)
	}
	if !equal(first.Affected, rebuilt.Affected) {
		t.Errorf("from-scratch rebuild lost information: %v vs %v", rebuilt.Affected, first.Affected)
	}
	if !equal(first.Changed, rebuilt.Changed) {
		t.Errorf("from-scratch rebuild changed the change set: %v vs %v", rebuilt.Changed, first.Changed)
	}
}

// TestAnalyzersEmptyWithoutManifest: a repo that has registered no analyzer
// reports an empty analyzer set rather than failing. The built-in extractors
// still run, so the graph is not empty — the analyzer set records only what was
// supplied as wasm.
func TestAnalyzersEmptyWithoutManifest(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	as, err := Analyzers(r)
	if err != nil {
		t.Fatalf("Analyzers on a repo with no hook manifest: %v", err)
	}
	if len(as) != 0 {
		t.Errorf("Analyzers = %v, want none registered", as)
	}
	ch := flatTree(t, r, map[string]string{"a.go": "package a\n"})
	res, err := Affected(r, nil, ch)
	if err != nil {
		t.Fatal(err)
	}
	if res.Analyzers != nil && len(res.Analyzers) != 0 {
		t.Errorf("AffectedResult.Analyzers = %v, want empty", res.Analyzers)
	}
}

// TestFirstParent covers the three cases a shell needs and cannot compute itself.
func TestFirstParent(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := flatTree(t, r, map[string]string{"a.go": "package a\n"})
	child := flatTree(t, r, map[string]string{"a.go": "package a\n// e\n"}, root)

	got, err := FirstParent(r, child)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !got.Equal(root) {
		t.Errorf("FirstParent(child) = %v, want %v", got, root)
	}

	got, err = FirstParent(r, root)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("FirstParent(root change) = %v, want nil", got)
	}

	if got, err = FirstParent(r, nil); err != nil || got != nil {
		t.Errorf("FirstParent(nil) = %v, %v; want nil, nil", got, err)
	}

	// A tree is not a change: no parent, and not an error either.
	tree, err := TreeOf(r, root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err = FirstParent(r, tree); err != nil || got != nil {
		t.Errorf("FirstParent(tree) = %v, %v; want nil, nil", got, err)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCoverageNamesTheGap is the §5 / GRAPH.md §11.4 property: an affected set
// over a language no analyzer understands must say so, because an absent edge
// there is a coverage gap, not a fact about dependencies.
func TestCoverageNamesTheGap(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ch := flatTree(t, r, map[string]string{
		"app.js":   "import './lib.js'\n", // covered by a built-in
		"lib.js":   "export const x = 1\n",
		"main.rb":  "require_relative 'other'\n", // no analyzer covers Ruby
		"README":   "docs\n",                     // no extension at all
		"other.rb": "\n",
	})
	res, err := Affected(r, nil, ch)
	if err != nil {
		t.Fatal(err)
	}
	if res.Coverage.Complete() {
		t.Fatal("Coverage.Complete() = true, but Ruby and an extensionless file are uncovered")
	}
	if res.Coverage.Analyzed != 2 {
		t.Errorf("Coverage.Analyzed = %d, want 2 (the two .js files)", res.Coverage.Analyzed)
	}
	if res.Coverage.Unanalyzed != 3 {
		t.Errorf("Coverage.Unanalyzed = %d, want 3 (.rb x2 and README)", res.Coverage.Unanalyzed)
	}
	if !equal(res.Coverage.UnanalyzedExts, []string{"", ".rb"}) {
		t.Errorf("UnanalyzedExts = %q, want [\"\" \".rb\"]", res.Coverage.UnanalyzedExts)
	}
}

// TestCoverageCompleteWhenFullyAnalyzed: the other side of the same property —
// a tree the built-ins fully understand reports complete coverage, so a caller
// can trust an absence there.
func TestCoverageCompleteWhenFullyAnalyzed(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ch := flatTree(t, r, map[string]string{
		"a.js": "export const a = 1\n",
		"b.ts": "export const b = 2\n",
		"c.go": "package c\n",
		"d.py": "x = 1\n",
	})
	res, err := Affected(r, nil, ch)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Coverage.Complete() {
		t.Fatalf("Coverage.Complete() = false for an all-built-in tree; gaps: %q",
			res.Coverage.UnanalyzedExts)
	}
	if res.Coverage.Analyzed != 4 {
		t.Errorf("Coverage.Analyzed = %d, want 4", res.Coverage.Analyzed)
	}
}

// TestCoverageCountsRegisteredAnalyzer pins the seam between an analyzer's
// declared extension and a file's extension, end to end through a real
// registered wasm analyzer.
//
// It exists because a refactor briefly routed the analyzer's ".rb" through
// lowerExt, where extOf(".rb") is "" — a dotfile has no extension — so every
// registered analyzer mapped to the empty string and stopped counting toward
// coverage. Nothing in the suite noticed: the coverage tests all used built-in
// languages, and a unit test of coverageOf alone would not have caught it either,
// because the bug was in the caller that builds the extension set.
func TestCoverageCountsRegisteredAnalyzer(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mod := buildRubyAnalyzer(t)
	if _, err := hook.SetHook(r, "analyze:.rb", mod, "jan"); err != nil {
		t.Fatal(err)
	}

	ch := flatTree(t, r, map[string]string{
		"a.rb": "require_relative 'b'\n",
		"b.rb": "\n",
	})

	res, err := Affected(r, nil, ch)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Analyzers) != 1 || res.Analyzers[0].Ext != ".rb" {
		t.Fatalf("Analyzers = %+v, want one for .rb", res.Analyzers)
	}
	if !res.Coverage.Complete() {
		t.Errorf("a registered .rb analyzer did not count toward coverage; gaps: %q",
			res.Coverage.UnanalyzedExts)
	}
	if res.Coverage.Analyzed != 2 {
		t.Errorf("Coverage.Analyzed = %d, want 2", res.Coverage.Analyzed)
	}

	// The analyzer actually ran: a.rb's dependency on b.rb was discovered by it,
	// not by any built-in.
	edges, err := DerivedEdges(r, ch)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range edges.Edges {
		if e.SourcePath() == "a.rb" && e.TargetPath() == "b.rb" {
			found = true
		}
	}
	if !found {
		t.Errorf("the registered analyzer produced no edge; edges = %d", len(edges.Edges))
	}
}

// buildRubyAnalyzer compiles a tiny wasip1 analyzer that emits each
// require_relative target, mirroring the fixture in package affected. It is a
// real module run in the real sandbox, so this test exercises the whole path:
// hook manifest, analyzer resolution, wasm host, cache key, coverage.
func buildRubyAnalyzer(t *testing.T) []byte {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	const src = `package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"regexp"
)

type in struct {
	Path    string ` + "`json:\"path\"`" + `
	Content string ` + "`json:\"content\"`" + `
}

func main() {
	b, _ := io.ReadAll(os.Stdin)
	var i in
	json.Unmarshal(b, &i)
	c, _ := base64.StdEncoding.DecodeString(i.Content)
	re := regexp.MustCompile(` + "`" + `require_relative\s+['"]([^'"]+)['"]` + "`" + `)
	for _, m := range re.FindAllStringSubmatch(string(c), -1) {
		os.Stdout.WriteString("./" + m[1] + ".rb\n")
	}
}
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module a\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "m.wasm")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build wasm fixture: %v\n%s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
