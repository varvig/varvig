package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/core"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
)

// affectedFixture builds a two-commit history in which one file imports another
// across a scope boundary: src/auth/token.js is imported by src/web/app.js. A
// task scoped to src/auth can therefore change something that affects code it
// may not read — the case the gate verb has to handle honestly.
func affectedFixture(t *testing.T, f *gateFixture) (base, head multihash.Multihash) {
	t.Helper()
	put := func(o *object.Object) multihash.Multihash {
		id, err := f.repo.Objects.Put(o)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	build := func(tokenBody string) multihash.Multihash {
		token := put(object.NewBlob([]byte(tokenBody)))
		authTree := put(object.NewTree([]object.Entry{
			{Name: "token.js", Mode: 0o100644, Kind: object.TypeBlob, ID: token},
		}))
		app := put(object.NewBlob([]byte("import { t } from '../auth/token.js'\n")))
		webTree := put(object.NewTree([]object.Entry{
			{Name: "app.js", Mode: 0o100644, Kind: object.TypeBlob, ID: app},
		}))
		srcTree := put(object.NewTree([]object.Entry{
			{Name: "auth", Mode: 0o40000, Kind: object.TypeTree, ID: authTree},
			{Name: "web", Mode: 0o40000, Kind: object.TypeTree, ID: webTree},
		}))
		return put(object.NewTree([]object.Entry{
			{Name: "src", Mode: 0o40000, Kind: object.TypeTree, ID: srcTree},
		}))
	}
	base = put(object.NewChange(object.Change{
		Tree: build("export const t = 1\n"), Message: "init", Timestamp: 100,
	}))
	head = put(object.NewChange(object.Change{
		Tree: build("export const t = 2\n"), Parents: []multihash.Multihash{base},
		Message: "bump", Timestamp: 200,
	}))
	return base, head
}

type affectedReply struct {
	Change     string   `json:"change"`
	Parent     string   `json:"parent"`
	Changed    []string `json:"changed"`
	Affected   []string `json:"affected"`
	Pulled     []string `json:"pulled"`
	OutOfScope struct {
		Changed  int `json:"changed"`
		Affected int `json:"affected"`
	} `json:"out_of_scope"`
	Coverage struct {
		Complete        bool     `json:"complete"`
		AnalyzedFiles   int      `json:"analyzed_files"`
		UnanalyzedFiles int      `json:"unanalyzed_files"`
		UnanalyzedTypes []string `json:"unanalyzed_types"`
	} `json:"coverage"`
}

func callAffected(t *testing.T, f *gateFixture, scope, change string) affectedReply {
	t.Helper()
	gate, _ := newGate(f, scope, time.Hour)
	tr := decodeTool(t, drive(t, gate, call(1, "varvig_affected",
		`{"change":"`+change+`"}`))[0])
	if tr.IsError {
		t.Fatalf("varvig_affected errored: %s", tr.StructuredContent)
	}
	var got affectedReply
	if err := json.Unmarshal(tr.StructuredContent, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestAffectedThroughGate is the capability G0 exists to deliver: the affected
// set was CLI-only, so an agent could not ask it at all.
func TestAffectedThroughGate(t *testing.T) {
	f := newGateFixture(t)
	_, head := affectedFixture(t, f)

	got := callAffected(t, f, "/", head.Hex())
	if len(got.Changed) != 1 || got.Changed[0] != "src/auth/token.js" {
		t.Fatalf("changed = %v, want [src/auth/token.js]", got.Changed)
	}
	// app.js imports token.js, so it is affected without being changed.
	if !contains(got.Affected, "src/web/app.js") {
		t.Fatalf("affected = %v, want src/web/app.js pulled in", got.Affected)
	}
	if len(got.Pulled) != 1 || got.Pulled[0] != "src/web/app.js" {
		t.Errorf("pulled = %v, want [src/web/app.js]", got.Pulled)
	}
	if got.Parent == "" {
		t.Error("parent was not reported; an affected set is always against a base")
	}
}

// TestAffectedConfinesToScopeButCountsBeyond: a task scoped to src/auth must not
// learn the paths outside its scope, and must not be told nothing is out there
// either. Silence here would let an agent conclude its change is contained when
// it is not.
func TestAffectedConfinesToScopeButCountsBeyond(t *testing.T) {
	f := newGateFixture(t)
	_, head := affectedFixture(t, f)

	got := callAffected(t, f, "src/auth", head.Hex())
	for _, p := range got.Affected {
		if p == "src/web/app.js" {
			t.Fatalf("affected leaked an out-of-scope path: %v", got.Affected)
		}
	}
	for _, p := range got.Pulled {
		if p == "src/web/app.js" {
			t.Fatalf("pulled leaked an out-of-scope path: %v", got.Pulled)
		}
	}
	if got.OutOfScope.Affected != 1 {
		t.Errorf("out_of_scope.affected = %d, want 1 — the task must know its change reaches beyond its scope",
			got.OutOfScope.Affected)
	}
	if got.OutOfScope.Changed != 0 {
		t.Errorf("out_of_scope.changed = %d, want 0; the change is entirely inside src/auth",
			got.OutOfScope.Changed)
	}
}

// TestAffectedAlwaysCarriesCoverage pins the §5 / GRAPH.md §11.4 property at the
// wire: no affected result may arrive without a coverage descriptor, because an
// absent dependency and an unanalyzed language are not the same answer.
func TestAffectedAlwaysCarriesCoverage(t *testing.T) {
	f := newGateFixture(t)
	_, head := affectedFixture(t, f)

	gate, _ := newGate(f, "/", time.Hour)
	tr := decodeTool(t, drive(t, gate, call(1, "varvig_affected",
		`{"change":"`+head.Hex()+`"}`))[0])
	if tr.IsError {
		t.Fatalf("varvig_affected errored: %s", tr.StructuredContent)
	}
	var raw map[string]any
	if err := json.Unmarshal(tr.StructuredContent, &raw); err != nil {
		t.Fatal(err)
	}
	cov, ok := raw["coverage"].(map[string]any)
	if !ok {
		t.Fatalf("no coverage descriptor in the result: %s", tr.StructuredContent)
	}
	for _, k := range []string{"complete", "analyzed_files", "unanalyzed_files", "unanalyzed_types"} {
		if _, ok := cov[k]; !ok {
			t.Errorf("coverage is missing %q: %v", k, cov)
		}
	}
	// This fixture is all .js, which the built-in analyzer understands.
	if c, _ := cov["complete"].(bool); !c {
		t.Errorf("coverage.complete = false for an all-JavaScript tree: %v", cov)
	}
}

// TestAffectedGateMatchesCore is the U1 equivalence property, and the reason G0
// came before the graph work: both shells present one core's answer, so an
// operator running the CLI query and an agent running the gate query in the same
// checkout must not get different sets. An unscoped gate call is exactly what
// the CLI renders.
func TestAffectedGateMatchesCore(t *testing.T) {
	f := newGateFixture(t)
	base, head := affectedFixture(t, f)

	// The CLI's path: core.Affected against the change's first parent.
	parent, err := core.FirstParent(f.repo, head)
	if err != nil {
		t.Fatal(err)
	}
	if parent == nil || !parent.Equal(base) {
		t.Fatalf("FirstParent = %v, want %v", parent, base)
	}
	want, err := core.Affected(f.repo, parent, head)
	if err != nil {
		t.Fatal(err)
	}

	got := callAffected(t, f, "/", head.Hex())
	if !equalStrings(got.Changed, want.Changed) {
		t.Errorf("changed: gate %v, core %v", got.Changed, want.Changed)
	}
	if !equalStrings(got.Affected, want.Affected) {
		t.Errorf("affected: gate %v, core %v", got.Affected, want.Affected)
	}
	if got.Coverage.Complete != want.Coverage.Complete() ||
		got.Coverage.AnalyzedFiles != want.Coverage.Analyzed ||
		got.Coverage.UnanalyzedFiles != want.Coverage.Unanalyzed {
		t.Errorf("coverage: gate %+v, core %+v", got.Coverage, want.Coverage)
	}
}
