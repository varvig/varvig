package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/graphquery"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
)

// graphFixture builds a tree where a dependency crosses a scope boundary:
// src/auth/token.js is imported by src/web/app.js, and src/auth also holds a
// file in a language no analyzer covers.
func graphFixture(t *testing.T, f *gateFixture) multihash.Multihash {
	t.Helper()
	put := func(o *object.Object) multihash.Multihash {
		id, err := f.repo.Objects.Put(o)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	token := put(object.NewBlob([]byte("export const t = 1\n")))
	helper := put(object.NewBlob([]byte("import { t } from './token.js'\n")))
	legacy := put(object.NewBlob([]byte("require_relative 'token'\n"))) // uncovered
	authTree := put(object.NewTree([]object.Entry{
		{Name: "token.js", Mode: 0o100644, Kind: object.TypeBlob, ID: token},
		{Name: "helper.js", Mode: 0o100644, Kind: object.TypeBlob, ID: helper},
		{Name: "legacy.rb", Mode: 0o100644, Kind: object.TypeBlob, ID: legacy},
	}))
	app := put(object.NewBlob([]byte("import { t } from '../auth/token.js'\n")))
	webTree := put(object.NewTree([]object.Entry{
		{Name: "app.js", Mode: 0o100644, Kind: object.TypeBlob, ID: app},
	}))
	srcTree := put(object.NewTree([]object.Entry{
		{Name: "auth", Mode: 0o40000, Kind: object.TypeTree, ID: authTree},
		{Name: "web", Mode: 0o40000, Kind: object.TypeTree, ID: webTree},
	}))
	rootTree := put(object.NewTree([]object.Entry{
		{Name: "src", Mode: 0o40000, Kind: object.TypeTree, ID: srcTree},
	}))
	return put(object.NewChange(object.Change{Tree: rootTree, Message: "graph", Timestamp: 100}))
}

type graphReply struct {
	Path      string `json:"path"`
	Direction string `json:"direction"`
	Derived   []struct {
		Path string `json:"path"`
		Type string `json:"type"`
		Node string `json:"node"`
	} `json:"derived"`
	Imported          []map[string]any `json:"imported"`
	Asserted          []map[string]any `json:"asserted"`
	GatingClass       string           `json:"gating_class"`
	OutOfScopeDerived int              `json:"out_of_scope_derived"`
	Coverage          struct {
		Complete        bool     `json:"complete"`
		UnanalyzedTypes []string `json:"unanalyzed_types"`
	} `json:"coverage"`
}

func callGraph(t *testing.T, f *gateFixture, scope, args string) graphReply {
	t.Helper()
	gate, _ := newGate(f, scope, time.Hour)
	tr := decodeTool(t, drive(t, gate, call(1, "varvig_graph", args))[0])
	if tr.IsError {
		t.Fatalf("varvig_graph errored: %s", tr.StructuredContent)
	}
	var got graphReply
	if err := json.Unmarshal(tr.StructuredContent, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestGraphThroughGate: the verb answers, partitions, and names the gating class.
func TestGraphThroughGate(t *testing.T) {
	f := newGateFixture(t)
	ch := graphFixture(t, f)

	got := callGraph(t, f, "/", `{"path":"src/auth/token.js","direction":"dependents","change":"`+ch.Hex()+`"}`)
	if got.Direction != "dependents" {
		t.Errorf("direction = %q, want dependents", got.Direction)
	}
	paths := map[string]bool{}
	for _, e := range got.Derived {
		paths[e.Path] = true
		if e.Node == e.Path {
			t.Error("an endpoint was reported as a bare path; it must be a content-addressed node")
		}
	}
	for _, want := range []string{"src/auth/helper.js", "src/web/app.js"} {
		if !paths[want] {
			t.Errorf("dependents = %v, missing %s", paths, want)
		}
	}
	if got.GatingClass != "derived" {
		t.Errorf("gating_class = %q, want derived", got.GatingClass)
	}
	if got.Coverage.Complete {
		t.Error("coverage reported complete despite the .rb file")
	}
}

// TestGraphConfinesToScopeButCountsBeyond: a task scoped to src/auth learns that
// something outside its scope depends on its file, without learning what.
func TestGraphConfinesToScopeButCountsBeyond(t *testing.T) {
	f := newGateFixture(t)
	ch := graphFixture(t, f)

	got := callGraph(t, f, "src/auth", `{"path":"src/auth/token.js","direction":"dependents","change":"`+ch.Hex()+`"}`)
	for _, e := range got.Derived {
		if e.Path == "src/web/app.js" {
			t.Fatalf("an out-of-scope dependent leaked: %v", got.Derived)
		}
	}
	if got.OutOfScopeDerived != 1 {
		t.Errorf("out_of_scope_derived = %d, want 1 — the task must know its file is reached from outside",
			got.OutOfScopeDerived)
	}
	// The in-scope dependent is still reported.
	found := false
	for _, e := range got.Derived {
		if e.Path == "src/auth/helper.js" {
			found = true
		}
	}
	if !found {
		t.Errorf("the in-scope dependent was dropped: %v", got.Derived)
	}
}

// TestGraphRefusesAnOutOfScopePath: the queried path is a targeted read, so it
// is refused rather than silently answered as empty. An empty answer would read
// as "nothing depends on this".
func TestGraphRefusesAnOutOfScopePath(t *testing.T) {
	f := newGateFixture(t)
	ch := graphFixture(t, f)
	gate, _ := newGate(f, "src/auth", time.Hour)
	tr := decodeTool(t, drive(t, gate, call(1, "varvig_graph",
		`{"path":"src/web/app.js","change":"`+ch.Hex()+`"}`))[0])
	if !tr.IsError {
		t.Fatal("querying an out-of-scope path must be refused, not answered empty")
	}
	if c := errCode(t, tr); c != codeOutOfScope {
		t.Errorf("error code = %q, want %q", c, codeOutOfScope)
	}
}

// TestGraphEdgeIsThreeValuedThroughTheGate: the three states survive the wire,
// and unknown is never rendered as absent.
func TestGraphEdgeIsThreeValuedThroughTheGate(t *testing.T) {
	f := newGateFixture(t)
	ch := graphFixture(t, f)
	gate, _ := newGate(f, "/", time.Hour)

	ask := func(from, to string) (string, string) {
		t.Helper()
		tr := decodeTool(t, drive(t, gate, call(1, "varvig_graph_edge",
			`{"from":"`+from+`","to":"`+to+`","change":"`+ch.Hex()+`"}`))[0])
		if tr.IsError {
			t.Fatalf("varvig_graph_edge errored: %s", tr.StructuredContent)
		}
		var got struct {
			State  string `json:"state"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(tr.StructuredContent, &got); err != nil {
			t.Fatal(err)
		}
		return got.State, got.Reason
	}

	if s, _ := ask("src/web/app.js", "src/auth/token.js"); s != graphquery.StatePresent.String() {
		t.Errorf("a real import came back %q, want present", s)
	}
	if s, _ := ask("src/auth/helper.js", "src/web/app.js"); s != graphquery.StateAbsentUnderCoverage.String() {
		t.Errorf("a genuine non-dependency came back %q, want absent-under-coverage", s)
	}
	s, reason := ask("src/auth/legacy.rb", "src/auth/token.js")
	if s != graphquery.StateUnknownOutsideCoverage.String() {
		t.Fatalf("an unanalyzed language came back %q, want unknown-outside-coverage", s)
	}
	if reason == "" {
		t.Error("an unknown answer must say why")
	}
}

// TestGraphParametersAreUsedNotJustAccepted is C1: a documented parameter that
// is accepted and dropped is worse than one that is refused, because the caller
// cannot tell. Each parameter is exercised by showing that changing it changes
// the answer.
func TestGraphParametersAreUsedNotJustAccepted(t *testing.T) {
	f := newGateFixture(t)
	ch := graphFixture(t, f)
	hex := ch.Hex()

	// direction: the two values must give different answers.
	deps := callGraph(t, f, "/", `{"path":"src/auth/helper.js","direction":"dependencies","change":"`+hex+`"}`)
	rdeps := callGraph(t, f, "/", `{"path":"src/auth/helper.js","direction":"dependents","change":"`+hex+`"}`)
	if len(deps.Derived) == len(rdeps.Derived) && len(deps.Derived) > 0 {
		t.Error("direction may be accepted and ignored: both values gave the same count")
	}
	if len(deps.Derived) != 1 || deps.Derived[0].Path != "src/auth/token.js" {
		t.Errorf("dependencies of helper.js = %v, want [token.js]", deps.Derived)
	}
	if len(rdeps.Derived) != 0 {
		t.Errorf("dependents of helper.js = %v, want none", rdeps.Derived)
	}

	// path: a different path must give a different answer.
	other := callGraph(t, f, "/", `{"path":"src/auth/token.js","direction":"dependencies","change":"`+hex+`"}`)
	if other.Path != "src/auth/token.js" {
		t.Errorf("path echoed as %q", other.Path)
	}
	if len(other.Derived) != 0 {
		t.Errorf("token.js imports nothing, got %v", other.Derived)
	}

	// change: omitting it must fall back to the task base, which is a different
	// tree than the fixture change — so the answers must differ.
	base := callGraph(t, f, "/", `{"path":"src/auth/token.js","direction":"dependents"}`)
	named := callGraph(t, f, "/", `{"path":"src/auth/token.js","direction":"dependents","change":"`+hex+`"}`)
	if len(base.Derived) == len(named.Derived) {
		t.Error("change may be accepted and ignored: the task base and the named change agreed")
	}

	// An unknown direction is refused rather than silently defaulted.
	gate, _ := newGate(f, "/", time.Hour)
	tr := decodeTool(t, drive(t, gate, call(1, "varvig_graph",
		`{"path":"src/auth/token.js","direction":"sideways","change":"`+hex+`"}`))[0])
	if !tr.IsError {
		t.Error("an unrecognized direction must be refused, not defaulted")
	}
}

// TestGraphGateMatchesCore is G6's equivalence acceptance: the same query
// through the gate and through the core the CLI renders must agree.
func TestGraphGateMatchesCore(t *testing.T) {
	f := newGateFixture(t)
	ch := graphFixture(t, f)

	q, err := graphquery.New(f.repo, ch)
	if err != nil {
		t.Fatal(err)
	}
	want, err := q.Dependents("src/auth/token.js")
	if err != nil {
		t.Fatal(err)
	}
	got := callGraph(t, f, "/", `{"path":"src/auth/token.js","direction":"dependents","change":"`+ch.Hex()+`"}`)

	if len(got.Derived) != len(want.Derived()) {
		t.Fatalf("gate reported %d derived edges, core %d", len(got.Derived), len(want.Derived()))
	}
	coreSet := map[string]bool{}
	for _, e := range want.Derived() {
		coreSet[e.SourcePath()] = true
	}
	for _, e := range got.Derived {
		if !coreSet[e.Path] {
			t.Errorf("gate reported %s, which the core did not", e.Path)
		}
	}
	if got.Coverage.Complete != want.Coverage().Complete() {
		t.Errorf("coverage disagreed: gate complete=%v, core complete=%v",
			got.Coverage.Complete, want.Coverage().Complete())
	}
}
