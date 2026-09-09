package graphquery

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/edge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/graphnode"
	"github.com/dividebyzero/claude-experiments/varvig/internal/hook"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

func flatTree(t *testing.T, r *repo.Repo, files map[string]string) multihash.Multihash {
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
	return tree
}

// TestPresentAbsentAndUnknownAreDistinguishable is G5's headline acceptance: a
// repo containing an unanalyzed language must return unknown-outside-coverage,
// and it must be distinguishable from absence.
func TestPresentAbsentAndUnknownAreDistinguishable(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tree := flatTree(t, r, map[string]string{
		"a.js":    "import './b.js'\n",
		"b.js":    "export const b = 1\n",
		"lone.js": "export const l = 1\n",
		"x.rb":    "require_relative 'y'\n", // nothing covers Ruby here
		"y.rb":    "\n",
	})
	q, err := New(r, tree)
	if err != nil {
		t.Fatal(err)
	}

	// Present: an analyzer saw the import.
	if got, err := q.Edge("a.js", "b.js"); err != nil {
		t.Fatal(err)
	} else if got.State() != StatePresent {
		t.Errorf("a.js -> b.js = %s, want present", got.State())
	}

	// Absent under coverage: both files were read, there is no such edge. This
	// is a fact about the code.
	absentAnswer, err := q.Edge("a.js", "lone.js")
	if err != nil {
		t.Fatal(err)
	}
	if absentAnswer.State() != StateAbsentUnderCoverage {
		t.Errorf("a.js -> lone.js = %s, want absent-under-coverage", absentAnswer.State())
	}

	// Unknown outside coverage: nothing parses Ruby, so nothing has looked. The
	// dependency is real in the source, and the honest answer is not "no".
	unknownAnswer, err := q.Edge("x.rb", "y.rb")
	if err != nil {
		t.Fatal(err)
	}
	if unknownAnswer.State() != StateUnknownOutsideCoverage {
		t.Fatalf("x.rb -> y.rb = %s, want unknown-outside-coverage", unknownAnswer.State())
	}

	// The two non-present answers must not be interchangeable.
	if absentAnswer.State() == unknownAnswer.State() {
		t.Fatal("absence and ignorance came back as the same answer")
	}
	// And a caller collapsing to a boolean gets to decide what unknown means,
	// which is the whole point: the same answer yields both, deliberately.
	if unknownAnswer.PresentOr(true) == unknownAnswer.PresentOr(false) {
		t.Error("PresentOr ignored the caller's treatment of unknown")
	}
	// Absence does not depend on that choice — it is settled.
	if absentAnswer.PresentOr(true) != false || absentAnswer.PresentOr(false) != false {
		t.Error("an absent-under-coverage answer must be false either way")
	}
}

// TestRemovingAnAnalyzerTurnsPresentIntoUnknown is the second acceptance, and
// the sharper one: a previously-present answer must become unknown, never
// absent. Becoming absent would be the system inventing a fact from the loss of
// a tool.
func TestRemovingAnAnalyzerTurnsPresentIntoUnknown(t *testing.T) {
	mod := buildRubyAnalyzer(t)

	withAnalyzer, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hook.SetHook(withAnalyzer, "analyze:.rb", mod, "jan"); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"a.rb": "require_relative 'b'\n",
		"b.rb": "\n",
	}
	treeWith := flatTree(t, withAnalyzer, files)
	qWith, err := New(withAnalyzer, treeWith)
	if err != nil {
		t.Fatal(err)
	}
	before, err := qWith.Edge("a.rb", "b.rb")
	if err != nil {
		t.Fatal(err)
	}
	if before.State() != StatePresent {
		t.Fatalf("with the analyzer registered, a.rb -> b.rb = %s, want present", before.State())
	}

	// The same tree in a repo with no analyzer registered.
	without, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	treeWithout := flatTree(t, without, files)
	qWithout, err := New(without, treeWithout)
	if err != nil {
		t.Fatal(err)
	}
	after, err := qWithout.Edge("a.rb", "b.rb")
	if err != nil {
		t.Fatal(err)
	}
	if after.State() == StateAbsentUnderCoverage {
		t.Fatal("removing the analyzer turned a present answer into absent; " +
			"losing a tool is not evidence about the code")
	}
	if after.State() != StateUnknownOutsideCoverage {
		t.Errorf("without the analyzer, a.rb -> b.rb = %s, want unknown-outside-coverage",
			after.State())
	}
	if qWithout.Coverage().Complete() {
		t.Error("coverage reported complete with no analyzer for the only language present")
	}
}

// TestResultsArePartitionedByClass: no query returns a flat list, and merging
// keeps each edge's class.
func TestResultsArePartitionedByClass(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tree := flatTree(t, r, map[string]string{
		"a.js": "import './b.js'\n",
		"b.js": "export const b = 1\n",
	})
	files, err := treeFiles(t, r, tree)
	if err != nil {
		t.Fatal(err)
	}

	// An imported edge anchored on a.js's content.
	src, err := graphnode.Object(files["a.js"])
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := graphnode.External("somesystem", "row/1")
	if err != nil {
		t.Fatal(err)
	}
	imported, err := edge.New(edge.Spec{
		Source: src, Target: foreign, Type: "somesystem:tracks",
		ObservedUnder: files["a.js"],
		Provenance: edge.Provenance{
			Class: edge.Imported, Principal: "conn", Strength: object.StrengthWeak,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := edge.Put(r, imported, "conn", 100); err != nil {
		t.Fatal(err)
	}

	q, err := New(r, tree)
	if err != nil {
		t.Fatal(err)
	}
	res, err := q.Dependencies("a.js")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Derived()) != 1 {
		t.Errorf("Derived() = %d, want 1", len(res.Derived()))
	}
	if len(res.Imported()) != 1 {
		t.Errorf("Imported() = %d, want 1", len(res.Imported()))
	}
	if len(res.Asserted()) != 0 {
		t.Errorf("Asserted() = %d, want 0", len(res.Asserted()))
	}

	// Only the derived edge may gate anything.
	if len(res.GatingEdges()) != 1 {
		t.Errorf("GatingEdges() = %d, want just the derived edge", len(res.GatingEdges()))
	}

	// Reproducibility is the gating predicate, and only derived satisfies it.
	// There is no flat listing to check: the partition above is the whole API,
	// which is what keeps a claim from being read as a reproducible fact.
	if !ClassDerived.Reproducible() {
		t.Error("a derived edge must be reproducible")
	}
	if ClassImported.Reproducible() || ClassAsserted.Reproducible() {
		t.Error("only a derived edge is reproducible by a peer")
	}
}

// TestEveryResultCarriesCoverage: coverage rides on the result, not beside it.
func TestEveryResultCarriesCoverage(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tree := flatTree(t, r, map[string]string{
		"a.js":  "import './b.js'\n",
		"b.js":  "export const b = 1\n",
		"z.zzz": "unknown language\n",
	})
	q, err := New(r, tree)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.js", "b.js", "z.zzz", "does-not-exist"} {
		for _, get := range []func(string) (Result, error){q.Dependencies, q.Dependents} {
			res, err := get(name)
			if err != nil {
				t.Fatal(err)
			}
			if res.Coverage().Analyzed == 0 && res.Coverage().Unanalyzed == 0 {
				t.Errorf("result for %q carries an empty coverage descriptor", name)
			}
			if res.Coverage().Complete() {
				t.Errorf("coverage for %q reported complete despite the .zzz file", name)
			}
		}
	}
}

func treeFiles(t *testing.T, r *repo.Repo, tree multihash.Multihash) (map[string]multihash.Multihash, error) {
	t.Helper()
	q, err := New(r, tree)
	if err != nil {
		return nil, err
	}
	return q.blobOf, nil
}

// buildRubyAnalyzer compiles a tiny wasip1 analyzer emitting require_relative
// targets, so these tests exercise a real module in the real sandbox.
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
