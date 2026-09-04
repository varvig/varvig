package mcp

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/task"
	"github.com/dividebyzero/claude-experiments/varvig/internal/ticket"
)

// gateFixture is a repo with a base change whose tree has two subtrees,
// src/auth and src/web, plus a top-level README — enough to exercise scope. The
// auth subtree also carries a multi-line file (for line ranges / cursors) and a
// symlink (for the scope-escape suite).
type gateFixture struct {
	repo      *repo.Repo
	base      multihash.Multihash
	loginBlob multihash.Multihash
	multiBlob multihash.Multihash
	linkBlob  multihash.Multihash
	webBlob   multihash.Multihash
	readme    multihash.Multihash
}

func newGateFixture(t *testing.T) *gateFixture {
	t.Helper()
	r, err := repo.Init(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	put := func(o *object.Object) multihash.Multihash {
		id, err := r.Objects.Put(o)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	login := put(object.NewBlob([]byte("package auth\n")))
	multi := put(object.NewBlob([]byte("l1\nl2\nl3\nl4\nl5"))) // 5 lines, no trailing newline
	link := put(object.NewBlob([]byte("../../etc/passwd")))    // a symlink target string
	authTree := put(object.NewTree([]object.Entry{
		{Name: "login.go", Mode: 0o100644, Kind: object.TypeBlob, ID: login},
		{Name: "multi.go", Mode: 0o100644, Kind: object.TypeBlob, ID: multi},
		{Name: "link", Mode: 0o120000, Kind: object.TypeBlob, ID: link}, // symlink stored as a blob
	}))
	index := put(object.NewBlob([]byte("<html></html>\n")))
	webTree := put(object.NewTree([]object.Entry{
		{Name: "index.html", Mode: 0o100644, Kind: object.TypeBlob, ID: index},
	}))
	srcTree := put(object.NewTree([]object.Entry{
		{Name: "auth", Mode: 0o40000, Kind: object.TypeTree, ID: authTree},
		{Name: "web", Mode: 0o40000, Kind: object.TypeTree, ID: webTree},
	}))
	readme := put(object.NewBlob([]byte("# proj\n")))
	rootTree := put(object.NewTree([]object.Entry{
		{Name: "src", Mode: 0o40000, Kind: object.TypeTree, ID: srcTree},
		{Name: "README.md", Mode: 0o100644, Kind: object.TypeBlob, ID: readme},
	}))
	base := put(object.NewChange(object.Change{Tree: rootTree, Message: "init", Author: "jan", Timestamp: 100}))
	if err := r.Refs.Create("refs/heads/main", base, "test", "seed"); err != nil {
		t.Fatal(err)
	}
	return &gateFixture{repo: r, base: base, loginBlob: login, multiBlob: multi, linkBlob: link, webBlob: index, readme: readme}
}

// pipe adapts a reader and writer into one io.ReadWriter for Serve.
type pipe struct {
	r io.Reader
	w io.Writer
}

func (p *pipe) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipe) Write(b []byte) (int, error) { return p.w.Write(b) }

// drive feeds newline-delimited JSON-RPC requests through the gate and returns
// the decoded responses (notifications yield none, so len may be < len(reqs)).
func drive(t *testing.T, g *Gate, reqs ...string) []response {
	t.Helper()
	var out bytes.Buffer
	rw := &pipe{r: strings.NewReader(strings.Join(reqs, "\n")), w: &out}
	if err := g.Serve(rw); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resps []response
	dec := json.NewDecoder(&out)
	for {
		var r response
		if err := dec.Decode(&r); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode response: %v", err)
		}
		resps = append(resps, r)
	}
	return resps
}

// toolResult is the decoded MCP tool-call result envelope.
type toolResult struct {
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	Content           []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func decodeTool(t *testing.T, r response) toolResult {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %v", r.Error)
	}
	var tr toolResult
	if err := json.Unmarshal(r.Result, &tr); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	return tr
}

// errCode pulls the machine-readable code out of a tool-error envelope (§8).
func errCode(t *testing.T, tr toolResult) string {
	t.Helper()
	var sc struct {
		Code  string `json:"code"`
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &sc); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return sc.Code
}

func newGate(f *gateFixture, scope string, ttl time.Duration) (*Gate, *task.Grant) {
	g, _ := task.New(scope, true, ttl, time.Unix(1000, 0))
	gate := NewGate(f.repo, g, f.base)
	gate.SetClock(func() time.Time { return time.Unix(1000, 0) })
	return gate, g
}

func req(id int, method, params string) string {
	if params == "" {
		params = "null"
	}
	return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"` + method + `","params":` + params + `}`
}

func call(id int, name, args string) string {
	return req(id, "tools/call", `{"name":"`+name+`","arguments":`+args+`}`)
}

func itoa(i int) string {
	return string(rune('0' + i)) // ids 0..9 suffice for tests
}

// advertisedTools is the exact surface the gate exposes: the ten read/propose
// tools (§4) plus read-only ticket access. Asserting the exact set catches an
// accidental addition or removal — the submission guard of §9.
var advertisedTools = []string{
	"varvig_task_context", "varvig_resolve", "varvig_list_tree", "varvig_read_file",
	"varvig_find_files", "varvig_search_text", "varvig_read_change", "varvig_read_log",
	"varvig_read_ticket", "varvig_list_proposals", "varvig_propose",
}

func TestInitializeAndToolsList(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "/", time.Hour)
	resps := drive(t, gate,
		req(1, "initialize", `{"protocolVersion":"2025-06-18"}`),
		req(2, "tools/list", ""),
	)
	if len(resps) != 2 {
		t.Fatalf("got %d responses, want 2", len(resps))
	}
	var init map[string]any
	if err := json.Unmarshal(resps[0].Result, &init); err != nil {
		t.Fatal(err)
	}
	if init["protocolVersion"] != "2025-06-18" {
		t.Errorf("initialize should echo the client protocol version, got %v", init["protocolVersion"])
	}
	si, _ := init["serverInfo"].(map[string]any)
	if si["name"] != serverName {
		t.Errorf("serverInfo.name = %v, want %s", si["name"], serverName)
	}

	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resps[1].Result, &list); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tl := range list.Tools {
		got[tl.Name] = true
	}
	if len(list.Tools) != len(advertisedTools) {
		t.Fatalf("got %d tools, want %d", len(list.Tools), len(advertisedTools))
	}
	for _, name := range advertisedTools {
		if !got[name] {
			t.Errorf("missing required tool %q", name)
		}
	}
}

// TestAnnotationAssertion is the directory-submission blocker the release smoke
// test enforces (§9): every advertised tool carries a title and the applicable
// readOnly/destructive hints, propose is the only writer, and — critically — no
// tool named *promote* exists. There is no promotion tool; do not add one.
func TestAnnotationAssertion(t *testing.T) {
	for _, tl := range toolList {
		name, _ := tl["name"].(string)
		if name == "" {
			t.Fatalf("tool with no name: %+v", tl)
		}
		if strings.Contains(name, "promote") {
			t.Fatalf("a *promote* tool exists (%q); the gate must never promote", name)
		}
		if title, _ := tl["title"].(string); title == "" {
			t.Errorf("tool %q: missing top-level title", name)
		}
		ann, ok := tl["annotations"].(map[string]any)
		if !ok {
			t.Errorf("tool %q: missing annotations block", name)
			continue
		}
		if title, _ := ann["title"].(string); title == "" {
			t.Errorf("tool %q: annotations.title is empty", name)
		}
		ro, roOK := ann["readOnlyHint"].(bool)
		if !roOK {
			t.Errorf("tool %q: annotations.readOnlyHint absent", name)
		}
		dh, dhOK := ann["destructiveHint"].(bool)
		if !dhOK {
			t.Errorf("tool %q: annotations.destructiveHint absent", name)
		}
		// The write path is append-only, so nothing is destructive (§4.2).
		if dh {
			t.Errorf("tool %q: destructiveHint must be false — the write path is append-only", name)
		}
		if name == "varvig_propose" {
			if ro {
				t.Errorf("varvig_propose must not be readOnly")
			}
		} else if !ro {
			t.Errorf("tool %q should be readOnly", name)
		}
	}
	if len(toolHandlers) != len(advertisedTools) {
		t.Errorf("got %d handlers, want %d", len(toolHandlers), len(advertisedTools))
	}
}

func TestTaskContext(t *testing.T) {
	f := newGateFixture(t)
	gate, grant := newGate(f, "src/auth", time.Hour)
	gate.SetIdentity("task", "task:demo")
	resps := drive(t, gate, call(1, "varvig_task_context", `{}`))
	tr := decodeTool(t, resps[0])
	if tr.IsError {
		t.Fatalf("task_context errored: %s", tr.Content[0].Text)
	}
	var out struct {
		Mode        string `json:"mode"`
		Principal   string `json:"principal"`
		Scope       string `json:"scope"`
		Base        string `json:"base"`
		ProposeOnly bool   `json:"propose_only"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &out); err != nil {
		t.Fatal(err)
	}
	if out.Mode != "task" || out.Principal != "task:demo" {
		t.Errorf("mode/principal = %q/%q, want task/task:demo", out.Mode, out.Principal)
	}
	if out.Scope != grant.Scopes.String() {
		t.Errorf("scope = %q, want %q", out.Scope, grant.Scopes.String())
	}
	if out.Base != f.base.Hex() {
		t.Errorf("base = %q, want %q", out.Base, f.base.Hex())
	}
	if !out.ProposeOnly {
		t.Error("task_context must report propose_only=true")
	}
}

// TestEveryResponseNamesTheBase pins §4.1: every tool response includes the base
// hash it resolved against.
func TestEveryResponseNamesTheBase(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "/", time.Hour)
	resps := drive(t, gate,
		call(1, "varvig_list_tree", `{}`),
		call(2, "varvig_read_file", `{"path":"README.md"}`),
		call(3, "varvig_read_change", `{}`),
	)
	for i, r := range resps {
		tr := decodeTool(t, r)
		var out struct {
			Base string `json:"base"`
		}
		if err := json.Unmarshal(tr.StructuredContent, &out); err != nil {
			t.Fatalf("response %d: %v", i+1, err)
		}
		if out.Base != f.base.Hex() {
			t.Errorf("response %d base = %q, want %q", i+1, out.Base, f.base.Hex())
		}
	}
}

func TestReadWithinScope(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "src/auth", time.Hour)
	resps := drive(t, gate,
		call(1, "varvig_list_tree", `{}`),
		call(2, "varvig_read_file", `{"path":"src/auth/login.go"}`),
	)
	tr := decodeTool(t, resps[0])
	if tr.IsError {
		t.Fatalf("list_tree errored: %s", tr.Content[0].Text)
	}
	var treeOut struct {
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &treeOut); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range treeOut.Entries {
		names[e.Name] = true
	}
	if !names["login.go"] || !names["multi.go"] {
		t.Fatalf("scope-root listing = %+v, want login.go and multi.go", treeOut.Entries)
	}

	br := decodeTool(t, resps[1])
	if br.IsError {
		t.Fatalf("read_file errored: %s", br.Content[0].Text)
	}
	var blobOut struct {
		Hash    string `json:"hash"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(br.StructuredContent, &blobOut); err != nil {
		t.Fatal(err)
	}
	if blobOut.Content != "package auth\n" {
		t.Errorf("content = %q, want %q", blobOut.Content, "package auth\n")
	}
	if blobOut.Hash != f.loginBlob.Hex() {
		t.Errorf("hash = %s, want %s", blobOut.Hash, f.loginBlob.Hex())
	}
}

// TestScopeEnforcement covers path-string escapes: out-of-scope paths, absolute
// paths, and ".." traversal all come back coded out_of_scope with a message
// naming the scope (§8, §9).
func TestScopeEnforcement(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "src/auth", time.Hour)
	resps := drive(t, gate,
		call(1, "varvig_read_file", `{"path":"src/web/index.html"}`),
		call(2, "varvig_list_tree", `{"path":"src/web"}`),
		call(3, "varvig_read_file", `{"path":"README.md"}`),
		call(4, "varvig_read_file", `{"path":"src/auth/../web/index.html"}`),
		call(5, "varvig_read_file", `{"path":"/etc/passwd"}`),
	)
	for i, r := range resps {
		tr := decodeTool(t, r)
		if !tr.IsError {
			t.Errorf("request %d should be refused as out-of-scope", i+1)
			continue
		}
		if code := errCode(t, tr); code != codeOutOfScope {
			t.Errorf("request %d code = %q, want %q", i+1, code, codeOutOfScope)
		}
		if !strings.Contains(tr.Content[0].Text, "scope") {
			t.Errorf("request %d error should mention scope, got %q", i+1, tr.Content[0].Text)
		}
	}
}

// TestReachabilityScope is the §9.4 case the string check misses: a blob hash
// that belongs to an out-of-scope subtree, fetched directly via resolve, must be
// rejected on reachability — while an in-scope hash resolves fine.
func TestReachabilityScope(t *testing.T) {
	f := newGateFixture(t)
	gate, grant := newGate(f, "src/auth", time.Hour)
	resps := drive(t, gate,
		call(1, "varvig_resolve", `{"ref":"`+f.loginBlob.Hex()+`"}`), // in scope
		call(2, "varvig_resolve", `{"ref":"`+f.webBlob.Hex()+`"}`),   // out-of-scope subtree
	)
	if in := decodeTool(t, resps[0]); in.IsError {
		t.Fatalf("resolving an in-scope blob failed: %s", in.Content[0].Text)
	}
	out := decodeTool(t, resps[1])
	if !out.IsError {
		t.Fatal("resolving an out-of-scope subtree hash must be refused (§9.4)")
	}
	if code := errCode(t, out); code != codeOutOfScope {
		t.Errorf("out-of-scope reachability code = %q, want %q", code, codeOutOfScope)
	}
	// Read logged even though the call errored after resolving (§5): the
	// out-of-scope hash was resolved, so it is in the read set.
	if !contains(grant.Reads.Hashes(), f.webBlob.Hex()) {
		t.Error("resolved hash must be recorded even when the tool then errors (§5)")
	}
}

// TestSymlinkIsNotFollowed rounds out the scope-escape suite: a symlink whose
// target points outside scope is returned as its literal target text — the gate
// reads objects, it does not dereference links against a filesystem.
func TestSymlinkIsNotFollowed(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "src/auth", time.Hour)
	resps := drive(t, gate, call(1, "varvig_read_file", `{"path":"src/auth/link"}`))
	tr := decodeTool(t, resps[0])
	if tr.IsError {
		t.Fatalf("reading the symlink object errored: %s", tr.Content[0].Text)
	}
	var out struct {
		Content string `json:"content"`
		Hash    string `json:"hash"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &out); err != nil {
		t.Fatal(err)
	}
	if out.Content != "../../etc/passwd" {
		t.Errorf("symlink content = %q, want the literal target (not followed)", out.Content)
	}
	if out.Hash != f.linkBlob.Hex() {
		t.Errorf("symlink hash = %s, want %s", out.Hash, f.linkBlob.Hex())
	}
}

func TestFindFilesAndSearchText(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "src/auth", time.Hour)
	resps := drive(t, gate,
		call(1, "varvig_find_files", `{"glob":"*.go"}`),
		call(2, "varvig_search_text", `{"query":"package auth"}`),
		call(3, "varvig_search_text", `{"query":"l[0-9]","regex":true}`),
	)
	// find_files: only the two .go files under src/auth, never src/web.
	ff := decodeTool(t, resps[0])
	var found struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(ff.StructuredContent, &found); err != nil {
		t.Fatal(err)
	}
	if len(found.Files) != 2 {
		t.Fatalf("find *.go got %d files, want 2 (login.go, multi.go)", len(found.Files))
	}
	for _, fl := range found.Files {
		if !strings.HasPrefix(fl.Path, "src/auth/") {
			t.Errorf("find returned out-of-scope path %q", fl.Path)
		}
	}
	// search_text literal: matches the login file.
	st := decodeTool(t, resps[1])
	var lit struct {
		Results []struct {
			Path    string `json:"path"`
			Matches []struct {
				Line int    `json:"line"`
				Text string `json:"text"`
			} `json:"matches"`
		} `json:"results"`
	}
	if err := json.Unmarshal(st.StructuredContent, &lit); err != nil {
		t.Fatal(err)
	}
	if len(lit.Results) != 1 || lit.Results[0].Matches[0].Text != "package auth" {
		t.Fatalf("literal search = %+v, want one match on 'package auth'", lit.Results)
	}
	// search_text regex: five matches in multi.go.
	re := decodeTool(t, resps[2])
	var rx struct {
		Results []struct {
			Path    string          `json:"path"`
			Matches json.RawMessage `json:"matches"`
		} `json:"results"`
	}
	if err := json.Unmarshal(re.StructuredContent, &rx); err != nil {
		t.Fatal(err)
	}
	var multiMatches int
	for _, r := range rx.Results {
		if r.Path == "src/auth/multi.go" {
			var ms []any
			_ = json.Unmarshal(r.Matches, &ms)
			multiMatches = len(ms)
		}
	}
	if multiMatches != 5 {
		t.Errorf("regex search matched %d lines in multi.go, want 5", multiMatches)
	}
}

// TestReadFileLineRangeAndCursorStability exercises §6 line ranges and the
// cursor-stability property: the same opaque cursor always resumes at the same
// place.
func TestReadFileLineRangeAndCursorStability(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "src/auth", time.Hour)

	// A bounded range that stops short of the end must report truncated and hand
	// back a cursor.
	first := drive(t, gate, call(1, "varvig_read_file", `{"path":"src/auth/multi.go","start":1,"end":2}`))
	var p1 struct {
		Content   string `json:"content"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
		Truncated bool   `json:"truncated"`
		Cursor    string `json:"cursor"`
	}
	if err := json.Unmarshal(decodeTool(t, first[0]).StructuredContent, &p1); err != nil {
		t.Fatal(err)
	}
	if p1.Content != "l1\nl2" || p1.StartLine != 1 || p1.EndLine != 2 {
		t.Fatalf("range read = %+v, want lines l1..l2", p1)
	}
	if !p1.Truncated || p1.Cursor == "" {
		t.Fatal("a short range must be marked truncated with a cursor")
	}

	// The same cursor, used twice, yields byte-identical continuations.
	cont := drive(t, gate,
		call(2, "varvig_read_file", `{"path":"src/auth/multi.go","cursor":"`+p1.Cursor+`"}`),
		call(3, "varvig_read_file", `{"path":"src/auth/multi.go","cursor":"`+p1.Cursor+`"}`),
	)
	a := decodeTool(t, cont[0]).StructuredContent
	b := decodeTool(t, cont[1]).StructuredContent
	if !bytes.Equal(a, b) {
		t.Fatalf("same cursor produced different continuations:\n a=%s\n b=%s", a, b)
	}
	var p2 struct {
		Content   string `json:"content"`
		StartLine int    `json:"start_line"`
	}
	if err := json.Unmarshal(a, &p2); err != nil {
		t.Fatal(err)
	}
	if p2.StartLine != 3 || p2.Content != "l3\nl4\nl5" {
		t.Errorf("continuation = %+v, want lines l3..l5", p2)
	}
}

// TestReadLogCompleteness verifies §5: reads populate the task read set with the
// hashes actually resolved (base, containing tree, blob).
func TestReadLogCompleteness(t *testing.T) {
	f := newGateFixture(t)
	gate, grant := newGate(f, "/", time.Hour)
	drive(t, gate, call(1, "varvig_read_file", `{"path":"README.md"}`))
	hashes := grant.Reads.Hashes()
	if !contains(hashes, f.base.Hex()) {
		t.Error("read log missing the base hash")
	}
	if !contains(hashes, f.readme.Hex()) {
		t.Error("read log missing the file blob hash")
	}
}

// TestReadChangeIntentFirst pins the §6 contract in the way that is actually
// testable (a JSON object has no key order): intent and the evidence summary are
// always present in full, and the changed-paths section is the one that carries
// the cursor — i.e. the part that truncates first when the cap binds.
func TestReadChangeIntentFirst(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "/", time.Hour)
	tr := decodeTool(t, drive(t, gate, call(1, "varvig_read_change", `{}`))[0])
	if tr.IsError {
		t.Fatalf("read_change errored: %s", tr.Content[0].Text)
	}
	var out struct {
		Intent  string `json:"intent"`
		Changed []struct {
			Op   string `json:"op"`
			Path string `json:"path"`
		} `json:"changed"`
		TruncatedField string `json:"truncated_field"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &out); err != nil {
		t.Fatal(err)
	}
	if out.Intent != "init" {
		t.Errorf("intent = %q, want %q", out.Intent, "init")
	}
	// The base change introduces the whole tree as additions.
	if len(out.Changed) == 0 || out.Changed[0].Op != "add" {
		t.Errorf("changed section = %+v, want additions for the root change", out.Changed)
	}
	// When read_change does truncate, it is the changed section that is cut first.
	if out.TruncatedField != "" && out.TruncatedField != "changed" {
		t.Errorf("read_change truncated %q first, want the changed section (§6)", out.TruncatedField)
	}
}

func TestProposeIsScopedSignedAndNeverPromotes(t *testing.T) {
	f := newGateFixture(t)
	gate, grant := newGate(f, "src/auth", time.Hour)
	resps := drive(t, gate,
		// Read first, so the read set is non-empty and gets folded into provenance.
		call(1, "varvig_read_file", `{"path":"src/auth/login.go"}`),
		call(2, "varvig_propose", `{"message":"add helper","files":[{"path":"src/auth/helper.go","content":"package auth\n\nfunc H() {}\n"}]}`),
		call(3, "varvig_propose", `{"message":"sneak","files":[{"path":"src/web/evil.html","content":"x"}]}`),
	)

	// The out-of-scope proposal is refused, coded.
	bad := decodeTool(t, resps[2])
	if !bad.IsError {
		t.Error("proposing outside scope must be refused")
	} else if code := errCode(t, bad); code != codeOutOfScope {
		t.Errorf("out-of-scope propose code = %q, want %q", code, codeOutOfScope)
	}

	pr := decodeTool(t, resps[1])
	if pr.IsError {
		t.Fatalf("propose errored: %s", pr.Content[0].Text)
	}
	var prop struct {
		Change   string   `json:"change"`
		Tree     string   `json:"tree"`
		ReadSet  []string `json:"read_set"`
		Promoted bool     `json:"promoted"`
	}
	if err := json.Unmarshal(pr.StructuredContent, &prop); err != nil {
		t.Fatal(err)
	}
	if prop.Promoted {
		t.Fatal("the gate must never promote")
	}
	if prop.Change == "" || prop.Tree == "" {
		t.Fatal("propose must return change and tree hashes")
	}
	if !contains(prop.ReadSet, f.loginBlob.Hex()) {
		t.Fatal("read set should include the earlier read_file")
	}

	// The ref did not move — propose is append-only, never a promotion (§4.2).
	head, err := f.repo.Refs.Resolve("refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqualHex(head, f.base) {
		t.Fatalf("refs/heads/main moved to %s; propose must not promote", head.Hex())
	}

	// The proposed change is signed by the task's ephemeral key and verifies.
	changeID, err := multihash.ParseHex(prop.Change)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := f.repo.Objects.Get(changeID)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := provenance.Verify(obj)
	if err != nil {
		t.Fatalf("proposed change does not verify: %v", err)
	}
	if string(pub) != string(grant.PublicKey().Key) {
		t.Fatal("proposed change is not signed by the task key")
	}
}

// TestRoundTripProposePromote is the §9 round-trip: propose via the gate, promote
// by moving the ref (what the CLI does), and confirm the promoted change object
// still carries the full read log in its provenance.
func TestRoundTripProposePromote(t *testing.T) {
	f := newGateFixture(t)
	gate, grant := newGate(f, "src/auth", time.Hour)
	resps := drive(t, gate,
		call(1, "varvig_read_file", `{"path":"src/auth/login.go"}`),
		call(2, "varvig_propose", `{"message":"add helper","files":[{"path":"src/auth/helper.go","content":"package auth\n"}]}`),
	)
	_ = decodeTool(t, resps[0])
	var prop struct {
		Change string `json:"change"`
	}
	if err := json.Unmarshal(decodeTool(t, resps[1]).StructuredContent, &prop); err != nil {
		t.Fatal(err)
	}
	changeID, err := multihash.ParseHex(prop.Change)
	if err != nil {
		t.Fatal(err)
	}
	// Promote: move the ref from base to the proposed change (CLI-equivalent CAS).
	if err := f.repo.Refs.CompareAndSwap("refs/heads/main", f.base, changeID, "human", "promote"); err != nil {
		t.Fatalf("promote (ref CAS): %v", err)
	}
	head, err := f.repo.Refs.Resolve("refs/heads/main")
	if err != nil || !bytesEqualHex(head, changeID) {
		t.Fatalf("head after promote = %v (err %v), want the proposed change", head, err)
	}
	// The promoted change still carries the full read log in provenance (§8.2).
	obj, err := f.repo.Objects.Get(changeID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := obj.AsChange()
	if err != nil {
		t.Fatal(err)
	}
	provObj, err := f.repo.Objects.Get(c.Provenance)
	if err != nil {
		t.Fatal(err)
	}
	pv, err := provObj.AsProvenance()
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range grant.Reads.Hashes() {
		if !strings.Contains(pv.ContextRead, h) {
			t.Errorf("promoted change provenance missing read-log hash %s", h)
		}
	}
	if pv.Authority != grant.Fingerprint() {
		t.Errorf("provenance authority = %q, want task fingerprint %q", pv.Authority, grant.Fingerprint())
	}
}

// TestReadTicket covers read-only ticket access: an agent can list tickets and
// read a ticket's intent and discussion, even when the ticket is outside its
// file subtree (tickets are intent, not file content), and the read is logged
// as provenance. There is no way to attest — governance stays human-only.
func TestReadTicket(t *testing.T) {
	f := newGateFixture(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tid, err := ticket.New(f.repo, "add rate limiting to the login path", priv, "jan", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := ticket.AddComment(f.repo, tid, ticket.Comment{Author: "jan", Body: "start with the auth subtree"}, 1001); err != nil {
		t.Fatal(err)
	}

	// A narrowly-scoped task can still read tickets — they carry no file content.
	gate, grant := newGate(f, "src/auth", time.Hour)

	// List mode: no argument returns the ticket.
	lr := decodeTool(t, drive(t, gate, call(1, "varvig_read_ticket", `{}`))[0])
	if lr.IsError {
		t.Fatalf("list tickets errored: %s", lr.Content[0].Text)
	}
	var list struct {
		Tickets []struct {
			ID   string `json:"id"`
			Spec string `json:"spec"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal(lr.StructuredContent, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tickets) != 1 || list.Tickets[0].ID != tid.Hex() {
		t.Fatalf("ticket list = %+v, want the one ticket %s", list.Tickets, tid.Hex())
	}

	// Detail mode: spec + discussion.
	dr := decodeTool(t, drive(t, gate, call(2, "varvig_read_ticket", `{"ticket":"`+tid.Hex()+`"}`))[0])
	if dr.IsError {
		t.Fatalf("read ticket errored: %s", dr.Content[0].Text)
	}
	var detail struct {
		Spec           string `json:"spec"`
		Implementation string `json:"implementation"`
		Comments       []struct {
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(dr.StructuredContent, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Spec != "add rate limiting to the login path" {
		t.Errorf("spec = %q", detail.Spec)
	}
	if detail.Implementation != "open" {
		t.Errorf("implementation = %q, want open (nothing fulfills it yet)", detail.Implementation)
	}
	if len(detail.Comments) != 1 || detail.Comments[0].Body != "start with the auth subtree" {
		t.Errorf("comments = %+v, want the one discussion entry", detail.Comments)
	}

	// Land a commit that fulfills the ticket, then re-read: derived status flips
	// to "implemented" and names the commit behind it (the ticket→commit link).
	baseObj, err := f.repo.Objects.Get(f.base)
	if err != nil {
		t.Fatal(err)
	}
	bc, err := baseObj.AsChange()
	if err != nil {
		t.Fatal(err)
	}
	fulfill, err := f.repo.Objects.Put(object.NewChange(object.Change{
		Tree:      bc.Tree,
		Parents:   []multihash.Multihash{f.base},
		Message:   "implement rate limiting",
		Author:    "jan",
		Timestamp: 1002,
		Fulfills:  tid,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Refs.CompareAndSwap("refs/heads/main", f.base, fulfill, "jan", "promote"); err != nil {
		t.Fatal(err)
	}
	ir := decodeTool(t, drive(t, gate, call(4, "varvig_read_ticket", `{"ticket":"`+tid.Hex()+`"}`))[0])
	var impl struct {
		Implementation string   `json:"implementation"`
		Implementers   []string `json:"implementers"`
	}
	if err := json.Unmarshal(ir.StructuredContent, &impl); err != nil {
		t.Fatal(err)
	}
	if impl.Implementation != "implemented" {
		t.Errorf("implementation = %q, want implemented", impl.Implementation)
	}
	if !contains(impl.Implementers, fulfill.Hex()) {
		t.Errorf("implementers = %v, want to include %s", impl.Implementers, fulfill.Hex())
	}

	// The ticket read is recorded as provenance (§5).
	if !contains(grant.Reads.Hashes(), tid.Hex()) {
		t.Error("reading a ticket must record its id in the read log")
	}

	// A malformed id is a coded not_found.
	br := decodeTool(t, drive(t, gate, call(3, "varvig_read_ticket", `{"ticket":"nothex"}`))[0])
	if !br.IsError || errCode(t, br) != codeNotFound {
		t.Errorf("bad ticket id should be not_found, got %+v", br)
	}
}

func TestListProposalsShowsTaskWork(t *testing.T) {
	f := newGateFixture(t)
	gate, grant := newGate(f, "src/auth", time.Hour)
	resps := drive(t, gate,
		call(1, "varvig_propose", `{"message":"c1","files":[{"path":"src/auth/a.go","content":"package auth\n"}]}`),
		call(2, "varvig_list_proposals", `{}`),
	)
	_ = decodeTool(t, resps[0])
	lr := decodeTool(t, resps[1])
	var out struct {
		Task      string `json:"task"`
		Proposals []struct {
			Change string `json:"change"`
		} `json:"proposals"`
	}
	if err := json.Unmarshal(lr.StructuredContent, &out); err != nil {
		t.Fatal(err)
	}
	if out.Task != grant.ID {
		t.Errorf("proposals task = %q, want grant id %q", out.Task, grant.ID)
	}
	if len(out.Proposals) != 1 {
		t.Fatalf("got %d proposals, want 1", len(out.Proposals))
	}
}

func TestExpiredCredentialIsCoded(t *testing.T) {
	f := newGateFixture(t)
	g, _ := task.New("/", true, time.Minute, time.Unix(1000, 0))
	gate := NewGate(f.repo, g, f.base)
	gate.SetClock(func() time.Time { return time.Unix(1000+61, 0) }) // past expiry
	resps := drive(t, gate, call(1, "varvig_list_tree", `{}`))
	tr := decodeTool(t, resps[0])
	if !tr.IsError {
		t.Fatal("expired credential should be refused")
	}
	if code := errCode(t, tr); code != codeCredentialExpired {
		t.Errorf("expired code = %q, want %q", code, codeCredentialExpired)
	}
}

func TestUnknownMethodIsProtocolError(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "/", time.Hour)
	resps := drive(t, gate, req(1, "no/such/method", ""))
	if resps[0].Error == nil || resps[0].Error.Code != codeMethodNotFound {
		t.Fatalf("unknown method should be a JSON-RPC method-not-found error, got %+v", resps[0])
	}
}

func TestNotificationGetsNoReply(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "/", time.Hour)
	resps := drive(t, gate, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if len(resps) != 0 {
		t.Fatalf("notification produced %d responses, want 0", len(resps))
	}
}

func bytesEqualHex(a, b multihash.Multihash) bool { return a.Hex() == b.Hex() }

func contains(hs []string, want string) bool {
	for _, h := range hs {
		if h == want {
			return true
		}
	}
	return false
}
