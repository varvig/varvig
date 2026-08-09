package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/spec"
)

// Tools are shaped coarsely and domain-first (auth design §8.1): fewer,
// domain-shaped operations rather than one wrapper per read-API endpoint,
// because the agent's context window is the scarce resource. Every tool returns
// hashes so the agent's reads are pinned and reproducible, and every resolved
// hash is folded into the task's read set for provenance (§8.2).

// toolList is advertised by tools/list. inputSchema is minimal JSON Schema.
var toolList = []map[string]any{
	{
		"name":        "fetch_tree",
		"description": "List a directory within the task's scope. Returns the tree hash and each entry's hash. Omit path for the scope root.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "repo-relative directory path; defaults to the scope root"},
			},
		},
	},
	{
		"name":        "fetch_blob",
		"description": "Read a file's contents by path, within the task's scope. Returns the blob hash and the content.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "repo-relative file path"},
			},
			"required": []string{"path"},
		},
	},
	{
		"name":        "fetch_change_with_intent",
		"description": "Fetch a change intent-first: its intent (message), then provenance evidence, then the diff. Defaults to the task's base change.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"change": map[string]any{"type": "string", "description": "change hash or ref; defaults to the task base"},
			},
		},
	},
	{
		"name":        "fetch_evidence",
		"description": "Fetch just the provenance evidence attached to a change (authority, model, tooling, intent). Defaults to the task's base change.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"change": map[string]any{"type": "string", "description": "change hash or ref; defaults to the task base"},
			},
		},
	},
	{
		"name":        "list_proposals",
		"description": "List the speculative changes this task has proposed but not promoted.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	},
	{
		"name":        "propose",
		"description": "Propose a change: overlay file contents onto the base tree and record a signed, speculative change. Every path must be within the task's scope. This never moves a ref — promotion is a separate, human-gated step.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{"type": "string", "description": "the change's intent"},
				"files": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":       map[string]any{"type": "string"},
							"content":    map[string]any{"type": "string"},
							"executable": map[string]any{"type": "boolean"},
						},
						"required": []string{"path", "content"},
					},
				},
			},
			"required": []string{"message", "files"},
		},
	},
}

// toolHandler runs one tool with its raw arguments and returns a JSON-able
// result. A returned error becomes an in-band tool error (isError), not a
// JSON-RPC protocol error.
type toolHandler func(g *Gate, args json.RawMessage) (any, error)

var toolHandlers = map[string]toolHandler{
	"fetch_tree":               toolFetchTree,
	"fetch_blob":               toolFetchBlob,
	"fetch_change_with_intent": toolFetchChange,
	"fetch_evidence":           toolFetchEvidence,
	"list_proposals":           toolListProposals,
	"propose":                  toolPropose,
}

// handleToolsCall validates the credential, dispatches the named tool, and wraps
// the outcome in an MCP tool result.
func (g *Gate) handleToolsCall(c *conn, req *request) error {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return c.replyError(req.ID, codeInvalidParams, err.Error())
	}
	// Expiry is checked on every call: an expired grant can do nothing, and no
	// revocation infrastructure is needed for the common case (§6.2).
	if !g.grant.Valid(g.clock()) {
		return c.replyResult(req.ID, toolError("task credential expired"))
	}
	h, ok := toolHandlers[params.Name]
	if !ok {
		return c.replyResult(req.ID, toolError("unknown tool: "+params.Name))
	}
	result, err := h(g, params.Arguments)
	if err != nil {
		return c.replyResult(req.ID, toolError(err.Error()))
	}
	return c.replyResult(req.ID, toolOK(result))
}

// specTask names the speculation bucket this task proposes into. It is the
// grant's short id (a valid, slash-free task name), so proposals from one task
// group together and list_proposals can show only this task's work.
func (g *Gate) specTask() string { return g.grant.ID }

// --- read tools ---

func toolFetchTree(g *Gate, raw json.RawMessage) (any, error) {
	var a struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(raw, &a)
	if g.base == nil {
		return nil, errors.New("task has no base state to read")
	}
	path, err := g.resolvePath(a.Path)
	if err != nil {
		return nil, err
	}
	listing, err := g.q.Tree(g.base, path)
	if err != nil {
		return nil, err
	}
	g.record(g.base.Hex(), listing.Root, listing.Hash)
	for _, e := range listing.Entries {
		g.record(e.Hash)
	}
	return map[string]any{"base": g.base.Hex(), "listing": listing}, nil
}

func toolFetchBlob(g *Gate, raw json.RawMessage) (any, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Path) == "" {
		return nil, errors.New("path is required")
	}
	if g.base == nil {
		return nil, errors.New("task has no base state to read")
	}
	path, err := g.resolvePath(a.Path)
	if err != nil {
		return nil, err
	}
	dir, file := splitDirFile(path)
	listing, err := g.q.Tree(g.base, dir)
	if err != nil {
		return nil, err
	}
	var blobHash multihash.Multihash
	for _, e := range listing.Entries {
		if e.Name == file && e.Kind == object.TypeBlob.String() {
			id, err := multihash.ParseHex(e.Hash)
			if err != nil {
				return nil, err
			}
			blobHash = id
			break
		}
	}
	if blobHash == nil {
		return nil, fmt.Errorf("no file %q within scope", path)
	}
	content, err := g.q.Blob(blobHash)
	if err != nil {
		return nil, err
	}
	g.record(g.base.Hex(), listing.Hash, blobHash.Hex())
	return map[string]any{"path": path, "hash": blobHash.Hex(), "content": string(content)}, nil
}

func toolFetchChange(g *Gate, raw json.RawMessage) (any, error) {
	var a struct {
		Change string `json:"change"`
	}
	_ = json.Unmarshal(raw, &a)
	id, err := g.resolveChange(a.Change)
	if err != nil {
		return nil, err
	}
	view, err := g.q.Change(id)
	if err != nil {
		return nil, err
	}
	g.record(view.Hash, view.Tree)
	return view, nil
}

func toolFetchEvidence(g *Gate, raw json.RawMessage) (any, error) {
	var a struct {
		Change string `json:"change"`
	}
	_ = json.Unmarshal(raw, &a)
	id, err := g.resolveChange(a.Change)
	if err != nil {
		return nil, err
	}
	view, err := g.q.Change(id)
	if err != nil {
		return nil, err
	}
	g.record(view.Hash)
	return map[string]any{
		"change":   view.Hash,
		"author":   view.Author,
		"signed":   view.Signed,
		"evidence": view.Evidence, // nil if the change carries no provenance
	}, nil
}

func toolListProposals(g *Gate, _ json.RawMessage) (any, error) {
	props, err := g.q.Proposals(g.specTask())
	if err != nil {
		return nil, err
	}
	for _, p := range props {
		g.record(p.Change)
	}
	return map[string]any{"task": g.specTask(), "proposals": props}, nil
}

// --- propose (write path: proposals, never promotions — §8.1) ---

func toolPropose(g *Gate, raw json.RawMessage) (any, error) {
	var a struct {
		Message string `json:"message"`
		Files   []struct {
			Path       string `json:"path"`
			Content    string `json:"content"`
			Executable bool   `json:"executable"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Message) == "" {
		return nil, errors.New("message (the change's intent) is required")
	}
	if len(a.Files) == 0 {
		return nil, errors.New("propose requires at least one file")
	}

	// Start from the base tree and overlay the proposed files, enforcing scope
	// on every path — the capability is the read set (§8.1).
	var baseTree multihash.Multihash
	if g.base != nil {
		bt, err := treeOfChange(g.repo, g.base)
		if err != nil {
			return nil, err
		}
		baseTree = bt
	}
	files, err := flattenTree(g.repo.Objects, baseTree)
	if err != nil {
		return nil, err
	}
	touched := make([]string, 0, len(a.Files))
	for _, f := range a.Files {
		path, err := g.resolvePath(f.Path)
		if err != nil {
			return nil, err
		}
		if path == "" || strings.HasSuffix(path, "/") {
			return nil, fmt.Errorf("invalid file path %q", f.Path)
		}
		blobID, err := g.repo.Objects.Put(object.NewBlob([]byte(f.Content)))
		if err != nil {
			return nil, err
		}
		mode := uint32(modeFile)
		if f.Executable {
			mode = 0o100755
		}
		files[path] = fileEnt{id: blobID, mode: mode}
		g.record(blobID.Hex())
		touched = append(touched, path)
	}
	newTree, err := buildTree(g.repo.Objects, files)
	if err != nil {
		return nil, err
	}

	// Provenance folds the task's read set into the change record, so what the
	// agent read and what it proposed are one auditable object (§8.2). Authority
	// is the task key's fingerprint; the change is signed by the ephemeral key.
	prov := object.NewProvenance(object.Provenance{
		Authority:   g.grant.Fingerprint(),
		TaskSpec:    a.Message,
		ContextRead: strings.Join(g.grant.Reads.Hashes(), " "),
	})
	provID, err := g.repo.Objects.Put(prov)
	if err != nil {
		return nil, err
	}

	var parents []multihash.Multihash
	if g.base != nil {
		parents = append(parents, g.base)
	}
	change := object.NewChange(object.Change{
		Tree:       newTree,
		Parents:    parents,
		Message:    a.Message,
		Timestamp:  g.clock().Unix(),
		Author:     g.grant.Fingerprint(),
		Provenance: provID,
	})
	if err := provenance.Sign(change, g.grant.PrivateKey()); err != nil {
		return nil, err
	}
	changeID, err := g.repo.Objects.Put(change)
	if err != nil {
		return nil, err
	}

	// Propose-only: record the change in the speculation pool under this task.
	// This is append-only and never moves a ref — promotion is separate and
	// human-gated (§8.1). Parallel tasks cannot damage one another (§10.6).
	pool := spec.Open(g.repo.GitDir())
	if err := pool.Add(g.specTask(), changeID, g.clock().Unix()); err != nil {
		return nil, err
	}

	return map[string]any{
		"task":       g.specTask(),
		"change":     changeID.Hex(),
		"tree":       newTree.Hex(),
		"provenance": provID.Hex(),
		"parents":    hexes(parents),
		"read_set":   g.grant.Reads.Hashes(),
		"promoted":   false, // always: the gate can never promote
	}, nil
}

// --- helpers ---

// resolveChange maps a change argument (hash or ref) to an identity, defaulting
// to the task's base. Change metadata (intent, evidence) is not file content, so
// this is not path-scoped; file reads (fetch_tree/fetch_blob) are.
func (g *Gate) resolveChange(arg string) (multihash.Multihash, error) {
	if strings.TrimSpace(arg) == "" {
		if g.base == nil {
			return nil, errors.New("task has no base change; pass an explicit change")
		}
		return g.base, nil
	}
	return g.q.Resolve(arg)
}

func treeOfChange(r *repo.Repo, id multihash.Multihash) (multihash.Multihash, error) {
	o, err := r.Objects.Get(id)
	if err != nil {
		return nil, err
	}
	if o.Type() == object.TypeChange {
		c, err := o.AsChange()
		if err != nil {
			return nil, err
		}
		return c.Tree, nil
	}
	return id, nil
}

func splitDirFile(path string) (dir, file string) {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i], path[i+1:]
	}
	return "", path
}

func hexes(ids []multihash.Multihash) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.Hex()
	}
	return out
}

// --- MCP tool-result envelopes ---

// toolOK wraps a successful result as MCP tool content. The JSON payload is
// provided both as text (universally supported) and as structuredContent.
func toolOK(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return toolError("internal: " + err.Error())
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(b)}},
		"structuredContent": v,
		"isError":           false,
	}
}

// toolError reports a tool-execution failure in-band (isError), which is how MCP
// surfaces tool errors to the model rather than as a protocol fault.
func toolError(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	}
}
