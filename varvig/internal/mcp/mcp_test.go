package mcp

import (
	"bytes"
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
)

// gateFixture is a repo with a base change whose tree has two subtrees,
// src/auth and src/web, plus a top-level README — enough to exercise scope.
type gateFixture struct {
	repo      *repo.Repo
	base      multihash.Multihash
	loginBlob multihash.Multihash
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
	authTree := put(object.NewTree([]object.Entry{
		{Name: "login.go", Mode: 0o100644, Kind: object.TypeBlob, ID: login},
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
	return &gateFixture{repo: r, base: base, loginBlob: login}
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

func itoa(i int) string {
	return string(rune('0' + i)) // ids 0..9 suffice for tests
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
	want := map[string]bool{
		"fetch_tree": true, "fetch_blob": true, "fetch_change_with_intent": true,
		"fetch_evidence": true, "list_proposals": true, "propose": true,
	}
	if len(list.Tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(list.Tools), len(want))
	}
	for _, tl := range list.Tools {
		if !want[tl.Name] {
			t.Errorf("unexpected tool %q", tl.Name)
		}
	}
}

func TestFetchWithinScope(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "src/auth", time.Hour)
	resps := drive(t, gate,
		req(1, "tools/call", `{"name":"fetch_tree","arguments":{}}`),
		req(2, "tools/call", `{"name":"fetch_blob","arguments":{"path":"src/auth/login.go"}}`),
	)
	// fetch_tree at the scope root lists the auth directory.
	tr := decodeTool(t, resps[0])
	if tr.IsError {
		t.Fatalf("fetch_tree errored: %s", tr.Content[0].Text)
	}
	var treeOut struct {
		Base    string `json:"base"`
		Listing struct {
			Entries []struct {
				Name string `json:"name"`
				Hash string `json:"hash"`
			} `json:"entries"`
		} `json:"listing"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &treeOut); err != nil {
		t.Fatal(err)
	}
	if len(treeOut.Listing.Entries) != 1 || treeOut.Listing.Entries[0].Name != "login.go" {
		t.Fatalf("scope-root listing = %+v, want just login.go", treeOut.Listing.Entries)
	}

	// fetch_blob returns the file content and its hash.
	br := decodeTool(t, resps[1])
	if br.IsError {
		t.Fatalf("fetch_blob errored: %s", br.Content[0].Text)
	}
	var blobOut struct {
		Path    string `json:"path"`
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

func TestScopeEnforcement(t *testing.T) {
	f := newGateFixture(t)
	gate, _ := newGate(f, "src/auth", time.Hour)
	resps := drive(t, gate,
		req(1, "tools/call", `{"name":"fetch_blob","arguments":{"path":"src/web/index.html"}}`),
		req(2, "tools/call", `{"name":"fetch_tree","arguments":{"path":"src/web"}}`),
		req(3, "tools/call", `{"name":"fetch_blob","arguments":{"path":"README.md"}}`),
	)
	for i, r := range resps {
		tr := decodeTool(t, r)
		if !tr.IsError {
			t.Errorf("request %d should be refused as out-of-scope", i+1)
		}
		if !strings.Contains(tr.Content[0].Text, "scope") {
			t.Errorf("request %d error should mention scope, got %q", i+1, tr.Content[0].Text)
		}
	}
}

func TestProposeIsScopedSignedAndNeverPromotes(t *testing.T) {
	f := newGateFixture(t)
	gate, grant := newGate(f, "src/auth", time.Hour)
	resps := drive(t, gate,
		// Read first, so the read set is non-empty and gets folded into provenance.
		req(1, "tools/call", `{"name":"fetch_blob","arguments":{"path":"src/auth/login.go"}}`),
		req(2, "tools/call", `{"name":"propose","arguments":{"message":"add helper","files":[{"path":"src/auth/helper.go","content":"package auth\n\nfunc H() {}\n"}]}}`),
		req(3, "tools/call", `{"name":"propose","arguments":{"message":"sneak","files":[{"path":"src/web/evil.html","content":"x"}]}}`),
	)

	// The out-of-scope proposal is refused.
	if bad := decodeTool(t, resps[2]); !bad.IsError {
		t.Error("proposing outside scope must be refused")
	}

	pr := decodeTool(t, resps[1])
	if pr.IsError {
		t.Fatalf("propose errored: %s", pr.Content[0].Text)
	}
	var prop struct {
		Task       string   `json:"task"`
		Change     string   `json:"change"`
		Tree       string   `json:"tree"`
		Provenance string   `json:"provenance"`
		ReadSet    []string `json:"read_set"`
		Promoted   bool     `json:"promoted"`
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
	if len(prop.ReadSet) == 0 {
		t.Fatal("read set should include the earlier fetch_blob")
	}

	// The ref did not move — propose is append-only, never a promotion (§8.1).
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

	// Provenance records the read set (§8.2): ContextRead contains the login blob.
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
	if !strings.Contains(pv.ContextRead, f.loginBlob.Hex()) {
		t.Errorf("provenance ContextRead %q should contain the read blob %s", pv.ContextRead, f.loginBlob.Hex())
	}
	if pv.Authority != grant.Fingerprint() {
		t.Errorf("provenance authority = %q, want task fingerprint %q", pv.Authority, grant.Fingerprint())
	}
}

func TestListProposalsShowsTaskWork(t *testing.T) {
	f := newGateFixture(t)
	gate, grant := newGate(f, "src/auth", time.Hour)
	resps := drive(t, gate,
		req(1, "tools/call", `{"name":"propose","arguments":{"message":"c1","files":[{"path":"src/auth/a.go","content":"package auth\n"}]}}`),
		req(2, "tools/call", `{"name":"list_proposals","arguments":{}}`),
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

func TestExpiredCredentialCannotAct(t *testing.T) {
	f := newGateFixture(t)
	g, _ := task.New("/", true, time.Minute, time.Unix(1000, 0))
	gate := NewGate(f.repo, g, f.base)
	// Advance the clock past expiry.
	gate.SetClock(func() time.Time { return time.Unix(1000+61, 0) })
	resps := drive(t, gate,
		req(1, "tools/call", `{"name":"fetch_tree","arguments":{}}`),
	)
	tr := decodeTool(t, resps[0])
	if !tr.IsError || !strings.Contains(tr.Content[0].Text, "expired") {
		t.Fatalf("expired credential should be refused, got %+v", tr)
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
	// A notification has no id and must produce no response.
	resps := drive(t, gate, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if len(resps) != 0 {
		t.Fatalf("notification produced %d responses, want 0", len(resps))
	}
}

func bytesEqualHex(a, b multihash.Multihash) bool { return a.Hex() == b.Hex() }
