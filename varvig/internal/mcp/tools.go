package mcp

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/blocked"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/readapi"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
	"github.com/dividebyzero/claude-experiments/varvig/internal/spec"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// The tool surface is small and domain-shaped (MCP spec §4): the ten
// read/propose tools plus read-only ticket access, not one wrapper per endpoint,
// because the agent's context window is the scarce resource (§8.1). Every tool
// declares a title and the applicable annotations —
// a directory-submission requirement the release smoke test asserts (§9) — and
// every response names the base hash it was resolved against (§4.1). There is no
// promotion tool, and because the write path is append-only, no destructive
// agent-facing tool at all.

var toolList = []map[string]any{
	{
		"name":        "varvig_task_context",
		"title":       "Task context",
		"description": "Report who this task is, its operating mode, its scope, and the base state its reads resolve against. Reads nothing.",
		"annotations": readOnlyAnnotations("Task context"),
		"inputSchema": objectSchema(nil, nil),
	},
	{
		"name":        "varvig_resolve",
		"title":       "Resolve a ref or hash",
		"description": "Resolve a ref or partial hash to a full object hash. A resolved file/subtree object outside the task's scope is rejected.",
		"annotations": readOnlyAnnotations("Resolve a ref or hash"),
		"inputSchema": objectSchema(map[string]any{
			"ref": strProp("a ref, full hash, or unambiguous hash prefix"),
		}, []string{"ref"}),
	},
	{
		"name":        "varvig_list_tree",
		"title":       "List a directory",
		"description": "List a directory at the task's base within scope. Returns the tree hash and each entry's hash. Paginates with an opaque cursor.",
		"annotations": readOnlyAnnotations("List a directory"),
		"inputSchema": objectSchema(map[string]any{
			"path":   strProp("repo-relative directory; defaults to the scope root"),
			"cursor": strProp("opaque continuation from a previous truncated listing"),
		}, nil),
	},
	{
		"name":        "varvig_read_file",
		"title":       "Read a file",
		"description": "Read a file's contents by path, within scope. Accepts a 1-based line range; without one, returns the whole file when it fits the cap, else the head plus a cursor.",
		"annotations": readOnlyAnnotations("Read a file"),
		"inputSchema": objectSchema(map[string]any{
			"path":   strProp("repo-relative file path"),
			"start":  intProp("first 1-based line to return"),
			"end":    intProp("last 1-based line to return (inclusive)"),
			"cursor": strProp("opaque continuation from a previous truncated read"),
		}, []string{"path"}),
	},
	{
		"name":        "varvig_find_files",
		"title":       "Find files by glob",
		"description": "Find files within scope whose path matches a glob (bare pattern matches the basename; a pattern with '/' matches the whole repo-relative path). Paginates with an opaque cursor.",
		"annotations": readOnlyAnnotations("Find files by glob"),
		"inputSchema": objectSchema(map[string]any{
			"glob":   strProp("glob pattern, e.g. *.go (single-level * only)"),
			"cursor": strProp("opaque continuation from a previous truncated result"),
		}, []string{"glob"}),
	},
	{
		"name":        "varvig_search_text",
		"title":       "Search file contents",
		"description": "Search file contents within scope for a literal string or regex. Returns matches with surrounding lines, capped per file so one file cannot consume the budget. Paginates with an opaque cursor.",
		"annotations": readOnlyAnnotations("Search file contents"),
		"inputSchema": objectSchema(map[string]any{
			"query":  strProp("literal text, or a regular expression when regex is true"),
			"regex":  boolProp("treat query as a regular expression"),
			"path":   strProp("restrict the search to this repo-relative subtree within scope"),
			"cursor": strProp("opaque continuation from a previous truncated search"),
		}, []string{"query"}),
	},
	{
		"name":        "varvig_read_change",
		"title":       "Read a change (intent first)",
		"description": "Read a change intent-first: its intent, a provenance evidence summary, any verification evidence (whether the tree passed its declared checks, and whether that evidence is still current), then the changed paths. Defaults to the task's base change. The changed-paths section is truncated first when the cap binds.",
		"annotations": readOnlyAnnotations("Read a change (intent first)"),
		"inputSchema": objectSchema(map[string]any{
			"change": strProp("change hash or ref; defaults to the task base"),
			"cursor": strProp("opaque continuation over the changed-paths section"),
		}, nil),
	},
	{
		"name":        "varvig_read_log",
		"title":       "Read change history",
		"description": "List the change history for a ref (default: the task base), optionally limited to changes that touch a path within scope. Paginates with an opaque cursor.",
		"annotations": readOnlyAnnotations("Read change history"),
		"inputSchema": objectSchema(map[string]any{
			"ref":    strProp("ref or change hash to start from; defaults to the task base"),
			"path":   strProp("only include changes touching this repo-relative path within scope"),
			"cursor": strProp("opaque continuation from a previous truncated log"),
		}, nil),
	},
	{
		"name":        "varvig_read_ticket",
		"title":       "Read a ticket",
		"description": "Read the repository's intent records (tickets). With no argument, list the tickets (id + spec). With a ticket id, return that ticket's spec, its derived implementation status (open / stale / implemented) and the commits behind it, any external artifacts it names, and its discussion — paginated with an opaque cursor. Read-only: governance decisions (approve / veto) are human-only and are not exposed here.",
		"annotations": readOnlyAnnotations("Read a ticket"),
		"inputSchema": objectSchema(map[string]any{
			"ticket": strProp("ticket id (hex) or refs/varvig/tickets/<id>; omit to list all tickets"),
			"cursor": strProp("opaque continuation from a previous truncated result"),
		}, nil),
	},
	{
		"name":        "varvig_list_proposals",
		"title":       "List proposals",
		"description": "List the speculative, unpromoted changes this task has proposed.",
		"annotations": readOnlyAnnotations("List proposals"),
		"inputSchema": objectSchema(nil, nil),
	},
	{
		"name":        "varvig_propose",
		"title":       "Propose a change",
		"description": "Propose a change: overlay file contents onto the base tree and record a signed, speculative change. Every path must be within scope. This never moves a ref — promotion is a separate, human-gated step.",
		// A write, but append-only: it never overwrites or moves a ref, so it is
		// not destructive (§4.2 — accurate even for a delete). Each call mints a
		// distinct signed change, so not idempotent.
		"annotations": map[string]any{
			"title":           "Propose a change",
			"readOnlyHint":    false,
			"destructiveHint": false,
			"idempotentHint":  false,
			"openWorldHint":   false,
		},
		"inputSchema": objectSchema(map[string]any{
			"message":   strProp("the change's intent (one line)"),
			"reasoning": strProp("the plan followed to produce the change — recorded so a reviewer can judge intent, not only the diff"),
			"files": map[string]any{
				"type":        "array",
				"description": "files to create or overwrite in the proposed state",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":       strProp("repo-relative path, within scope"),
						"content":    strProp("full file content"),
						"executable": boolProp("mark the file executable"),
					},
					"required": []string{"path", "content"},
				},
			},
		}, []string{"message", "files"}),
	},
	{
		"name":  "varvig_report_blocked",
		"title": "Report blocked on scope",
		"description": "Report that this task cannot proceed because it needs something outside its scope. " +
			"This is the third outcome beside a proposal and a failure: it records the path or capability you need, " +
			"why, and the requirement you could not meet, together with every scope boundary you have already hit, " +
			"and routes it to whoever can widen scope. It never widens scope itself and never moves a ref.",
		// A write (it records a signed report as a note), append-only, and not
		// idempotent — each call records a distinct report.
		"annotations": map[string]any{
			"title":           "Report blocked on scope",
			"readOnlyHint":    false,
			"destructiveHint": false,
			"idempotentHint":  false,
			"openWorldHint":   false,
		},
		"inputSchema": objectSchema(map[string]any{
			"need":  strProp("the path or capability you need added to your scope"),
			"why":   strProp("why you need it"),
			"unmet": strProp("the requirement you could not meet without it"),
		}, []string{"need", "why"}),
	},
}

// --- argument decoding ---

// decodeArgs parses a tool call's arguments into dst with a *closed* schema: an
// input field the tool does not model is a refusal, not a silent drop (build spec
// C0.1 / A3). This is the structural fix behind both C1 (a dropped `reasoning`)
// and C2 (a wrong parameter name silently answered from the base): a
// misspelled or unpersisted field now errors here instead of vanishing. Empty or
// absent arguments decode to the zero value, so no-argument tools stay callable.
func decodeArgs(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return gerr(codeInvalidArgs, "bad arguments: %v", err)
	}
	return nil
}

// --- schema helpers ---

func objectSchema(props map[string]any, required []string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// readOnlyAnnotations is the MCP annotation block for a pure read: it observes
// state and neither writes nor destroys, and repeating it changes nothing.
func readOnlyAnnotations(title string) map[string]any {
	return map[string]any{
		"title":           title,
		"readOnlyHint":    true,
		"destructiveHint": false,
		"idempotentHint":  true,
		"openWorldHint":   false,
	}
}

// toolHandler runs one tool with its raw arguments and returns a JSON-able map.
// A returned error becomes an in-band, coded tool error (isError), not a
// JSON-RPC protocol error.
type toolHandler func(g *Gate, args json.RawMessage) (map[string]any, error)

var toolHandlers = map[string]toolHandler{
	"varvig_task_context":   toolTaskContext,
	"varvig_resolve":        toolResolve,
	"varvig_list_tree":      toolListTree,
	"varvig_read_file":      toolReadFile,
	"varvig_find_files":     toolFindFiles,
	"varvig_search_text":    toolSearchText,
	"varvig_read_change":    toolReadChange,
	"varvig_read_log":       toolReadLog,
	"varvig_read_ticket":    toolReadTicket,
	"varvig_list_proposals": toolListProposals,
	"varvig_propose":        toolPropose,
	"varvig_report_blocked": toolReportBlocked,
}

// handleToolsCall validates the credential, dispatches the named tool, and wraps
// the outcome in an MCP tool result whose payload always names the base hash.
func (g *Gate) handleToolsCall(c *conn, req *request) error {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return c.replyError(req.ID, codeInvalidParams, err.Error())
	}
	// Expiry is checked on every call: an expired grant can do nothing, and the
	// distinct credential_expired code lets an orchestrator renew rather than
	// treat it as failure (§3, §8).
	if !g.grant.Valid(g.clock()) {
		return c.replyResult(req.ID, g.toolErr(gerr(codeCredentialExpired,
			"task credential expired; renew the task and retry (scope %q)", g.grant.Scopes)))
	}
	h, ok := toolHandlers[params.Name]
	if !ok {
		return c.replyResult(req.ID, g.toolErr(gerr(codeNotFound, "unknown tool %q", params.Name)))
	}
	result, err := h(g, params.Arguments)
	if err != nil {
		return c.replyResult(req.ID, g.toolErr(err))
	}
	return c.replyResult(req.ID, g.toolOK(result))
}

// specTask names the speculation bucket this task proposes into — the grant's
// short id, so proposals from one task group together.
func (g *Gate) specTask() string { return g.grant.ID }

// --- read tools ---

func toolTaskContext(g *Gate, _ json.RawMessage) (map[string]any, error) {
	return map[string]any{
		"mode":         g.resolvedMode(),
		"principal":    g.resolvedPrincipal(),
		"scope":        g.grant.Scopes.String(),
		"base":         g.baseHex(),
		"propose_only": true,
		"expires_unix": g.grant.NotAfter,
		// The scope-accuracy metric (build spec P1.2): how many distinct scope
		// boundaries this task has hit. Non-zero means the scope may be too narrow
		// — report it with varvig_report_blocked rather than working around it.
		"boundary_hits": g.boundaryHits(),
	}, nil
}

func toolResolve(g *Gate, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Ref string `json:"ref"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Ref) == "" {
		return nil, gerr(codeInvalidArgs, "ref is required")
	}
	id, err := g.rl.Resolve(a.Ref)
	if err != nil {
		return nil, gerr(codeNotFound, "cannot resolve %q (scope %q)", a.Ref, g.grant.Scopes)
	}
	o, err := g.repo.Objects.Get(id)
	if err != nil {
		return nil, gerr(codeNotFound, "resolved %q to %s but it is not stored", a.Ref, id.Hex())
	}
	// Enforce scope on object reachability, not only path strings (§9.4): a
	// blob/tree resolved directly must lie within the task's scope subtree.
	if t := o.Type(); t == object.TypeBlob || t == object.TypeTree {
		in, err := g.inScopeObject(id)
		if err != nil {
			return nil, err
		}
		if !in {
			g.noteBoundaryHit(id.Hex(), t.String()+" resolved outside the task scope")
			return nil, gerr(codeOutOfScope,
				"%s %s is outside the task scope %q", t.String(), id.Hex(), g.grant.Scopes)
		}
	}
	return map[string]any{"ref": a.Ref, "hash": id.Hex(), "type": o.Type().String()}, nil
}

func toolListTree(g *Gate, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Path   string `json:"path"`
		Cursor string `json:"cursor"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if g.base == nil {
		return nil, gerr(codeNotFound, "task has no base state to read")
	}
	cur, err := decodeCursor(a.Cursor)
	if err != nil {
		return nil, err
	}
	path, err := g.resolvePath(a.Path)
	if err != nil {
		return nil, err
	}
	listing, err := g.rl.Tree(g.base, path)
	if err != nil {
		return nil, gerr(codeNotFound, "no directory %q within scope %q", path, g.grant.Scopes)
	}
	page, next, truncated := paginate(listing.Entries, cur.Offset, treePageSize)
	out := map[string]any{
		"path":    listing.Path,
		"tree":    listing.Hash,
		"entries": page,
	}
	addPage(out, next, truncated, "entries")
	return out, nil
}

func toolReadFile(g *Gate, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Path   string `json:"path"`
		Start  int    `json:"start"`
		End    int    `json:"end"`
		Cursor string `json:"cursor"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Path) == "" {
		return nil, gerr(codeInvalidArgs, "path is required")
	}
	if g.base == nil {
		return nil, gerr(codeNotFound, "task has no base state to read")
	}
	cur, err := decodeCursor(a.Cursor)
	if err != nil {
		return nil, err
	}
	path, err := g.resolvePath(a.Path)
	if err != nil {
		return nil, err
	}
	blobHash, err := g.blobAt(path)
	if err != nil {
		return nil, err
	}
	content, err := g.rl.Blob(blobHash)
	if err != nil {
		return nil, gerr(codeNotFound, "cannot read %q", path)
	}
	lines := strings.Split(string(content), "\n")
	total := len(lines)

	// Explicit range wins. A cursor resumes at its line. Otherwise: whole file
	// under the cap, else the head plus a cursor (§6).
	start := a.Start
	end := a.End
	if cur.Line > 0 {
		start = cur.Line
	}
	if start <= 0 {
		start = 1
	}
	truncated := false
	var next string
	if a.Start == 0 && a.End == 0 && cur.Line == 0 && len(content) <= responseCap {
		end = total
	} else if end <= 0 {
		// Head-plus-cursor: cap the slice length.
		end = start + readFileHead - 1
	}
	if end > total {
		end = total
	}
	if end < start {
		end = start
	}
	if end < total {
		truncated = true
		next = encodeCursor(cursor{Line: end + 1})
	}
	body := strings.Join(lines[start-1:end], "\n")
	out := map[string]any{
		"path":        path,
		"hash":        blobHash.Hex(),
		"content":     body,
		"start_line":  start,
		"end_line":    end,
		"total_lines": total,
	}
	addPage(out, next, truncated, "content")
	return out, nil
}

func toolFindFiles(g *Gate, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Glob   string `json:"glob"`
		Cursor string `json:"cursor"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Glob) == "" {
		return nil, gerr(codeInvalidArgs, "glob is required")
	}
	cur, err := decodeCursor(a.Cursor)
	if err != nil {
		return nil, err
	}
	files, err := g.scopeFiles()
	if err != nil {
		return nil, err
	}
	matched := make([]map[string]any, 0)
	for _, f := range files {
		if matchGlob(a.Glob, f.Path) {
			g.record(f.Blob.Hex())
			matched = append(matched, map[string]any{"path": f.Path, "hash": f.Blob.Hex()})
		}
	}
	page, next, truncated := paginate(matched, cur.Offset, findPageSize)
	out := map[string]any{"glob": a.Glob, "files": page}
	addPage(out, next, truncated, "files")
	return out, nil
}

func toolSearchText(g *Gate, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Query  string `json:"query"`
		Regex  bool   `json:"regex"`
		Path   string `json:"path"`
		Cursor string `json:"cursor"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.Query == "" {
		return nil, gerr(codeInvalidArgs, "query is required")
	}
	cur, err := decodeCursor(a.Cursor)
	if err != nil {
		return nil, err
	}
	matcher, err := buildMatcher(a.Query, a.Regex)
	if err != nil {
		return nil, err
	}
	files, err := g.scopeFiles()
	if err != nil {
		return nil, err
	}
	// Optional path restriction, enforced within scope.
	if p := strings.Trim(a.Path, "/"); p != "" {
		rp, err := g.resolvePath(p)
		if err != nil {
			return nil, err
		}
		filtered := make([]fileRef, 0, len(files))
		for _, f := range files {
			if f.Path == rp || strings.HasPrefix(f.Path, rp+"/") {
				filtered = append(filtered, f)
			}
		}
		files = filtered
	}

	results := make([]map[string]any, 0)
	next := ""
	truncated := false
	i := cur.Offset
	for ; i < len(files); i++ {
		f := files[i]
		content, err := g.rl.Blob(f.Blob)
		if err != nil {
			continue
		}
		hits, capped := searchBlob(content, matcher)
		if len(hits) == 0 {
			continue
		}
		results = append(results, map[string]any{
			"path":    f.Path,
			"hash":    f.Blob.Hex(),
			"matches": hits,
			"capped":  capped,
		})
		// Byte cap: stop once the accumulated payload exceeds the budget, handing
		// back a cursor at the next file (§6). Never silent.
		if b, _ := json.Marshal(results); len(b) > responseCap {
			i++
			break
		}
	}
	if i < len(files) {
		truncated = true
		next = encodeCursor(cursor{Offset: i})
	}
	out := map[string]any{"query": a.Query, "results": results}
	addPage(out, next, truncated, "results")
	return out, nil
}

func toolReadChange(g *Gate, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Change string `json:"change"`
		Cursor string `json:"cursor"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	cur, err := decodeCursor(a.Cursor)
	if err != nil {
		return nil, err
	}
	id, err := g.resolveChange(a.Change)
	if err != nil {
		return nil, err
	}
	view, err := g.rl.Change(id)
	if err != nil {
		return nil, gerr(codeNotFound, "no change %q", a.Change)
	}

	// Intent first, then an evidence summary, then the changed paths — and the
	// changed-paths section is what truncates first when the cap binds (§6). A
	// diff-first response quietly rebuilds the diff-centric forge it replaces and loses the premise.
	changed := make([]map[string]any, 0, len(view.ChangedAdd)+len(view.ChangedMod)+len(view.ChangedDel))
	for _, p := range view.ChangedAdd {
		changed = append(changed, map[string]any{"op": "add", "path": p})
	}
	for _, p := range view.ChangedMod {
		changed = append(changed, map[string]any{"op": "modify", "path": p})
	}
	for _, p := range view.ChangedDel {
		changed = append(changed, map[string]any{"op": "delete", "path": p})
	}
	page, next, truncated := paginate(changed, cur.Offset, treePageSize)

	// Verification evidence for the change (build spec P1.3): whether the tree
	// passed its declared checks, and whether that evidence is still current for
	// the change's tree. Bounded (a few commands), so it is not paginated.
	checks, err := g.q.Checks(id)
	if err != nil {
		return nil, gerr(codeUnavailable, "cannot read checks: %v", err)
	}

	out := map[string]any{
		"change":    view.Hash,
		"intent":    view.Intent,
		"evidence":  evidenceSummary(view.Evidence),
		"checks":    checks,
		"author":    view.Author,
		"signed":    view.Signed,
		"timestamp": view.Timestamp,
		"parents":   view.Parents,
		"changed":   page,
	}
	addPage(out, next, truncated, "changed")
	return out, nil
}

func toolReadLog(g *Gate, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Ref    string `json:"ref"`
		Path   string `json:"path"`
		Cursor string `json:"cursor"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	cur, err := decodeCursor(a.Cursor)
	if err != nil {
		return nil, err
	}
	start, err := g.resolveChange(a.Ref)
	if err != nil {
		return nil, err
	}
	const maxLogScan = 1000
	entries, err := g.rl.Log(start, maxLogScan)
	if err != nil {
		return nil, gerr(codeNotFound, "cannot read log from %q", a.Ref)
	}
	// Optional path filter within scope: keep changes that touch the path.
	if p := strings.Trim(a.Path, "/"); p != "" {
		rp, err := g.resolvePath(p)
		if err != nil {
			return nil, err
		}
		kept := make([]readapi.LogEntryView, 0, len(entries))
		for _, e := range entries {
			id, perr := multihash.ParseHex(e.Hash)
			if perr != nil {
				continue
			}
			cv, cerr := g.rl.Change(id)
			if cerr != nil {
				continue
			}
			if changeTouches(cv, rp) {
				kept = append(kept, e)
			}
		}
		entries = kept
	}
	page, next, truncated := paginate(entries, cur.Offset, logPageSize)
	out := map[string]any{"start": start.Hex(), "entries": page}
	addPage(out, next, truncated, "entries")
	return out, nil
}

// toolReadTicket reads the repository's intent records. Tickets are
// unmaterialized changes (intent, no tree), so they are not file-subtree-scoped
// — reading one cannot leak file content outside the task's scope — and there is
// no governance surface here: an agent can read intent, never approve or veto it.
func toolReadTicket(g *Gate, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Ticket string `json:"ticket"`
		Cursor string `json:"cursor"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	cur, err := decodeCursor(a.Cursor)
	if err != nil {
		return nil, err
	}

	// No id: list the tickets so an agent can discover the one it needs.
	if strings.TrimSpace(a.Ticket) == "" {
		tickets, err := g.rl.Tickets()
		if err != nil {
			return nil, gerr(codeUnavailable, "cannot list tickets: %v", err)
		}
		page, next, truncated := paginate(tickets, cur.Offset, treePageSize)
		out := map[string]any{"tickets": page}
		addPage(out, next, truncated, "tickets")
		return out, nil
	}

	// An id: return the ticket's intent and discussion.
	id, err := ticketID(a.Ticket)
	if err != nil {
		return nil, err
	}
	detail, err := g.rl.Ticket(id)
	if err != nil {
		return nil, gerr(codeNotFound, "no ticket %q", a.Ticket)
	}
	artifacts, err := g.rl.TicketArtifacts(id)
	if err != nil {
		return nil, gerr(codeUnavailable, "cannot read ticket artifacts: %v", err)
	}
	page, next, truncated := paginate(detail.Comments, cur.Offset, treePageSize)
	out := map[string]any{
		"id":   detail.ID,
		"head": detail.Head,
		"spec": detail.Spec,
		// Derived from the ticket→commit link: "open", "stale", or "implemented",
		// with the commits behind that state, so an agent knows whether its intent
		// is already fulfilled and by what.
		"implementation": detail.Implementation,
		"implementers":   detail.Implementers,
		"materialized":   detail.Materialized,
		"artifacts":      artifacts, // external artifacts the ticket names (federation §1)
		"comments":       page,
	}
	addPage(out, next, truncated, "comments")
	return out, nil
}

// ticketID parses a ticket argument — a bare id hex or a
// refs/varvig/tickets/<id> ref — into a ticket id.
func ticketID(arg string) (multihash.Multihash, error) {
	s := strings.TrimPrefix(strings.TrimSpace(arg), reserved.TicketsPrefix)
	id, err := multihash.ParseHex(s)
	if err != nil {
		return nil, gerr(codeNotFound, "invalid ticket id %q", arg)
	}
	return id, nil
}

func toolListProposals(g *Gate, _ json.RawMessage) (map[string]any, error) {
	props, err := g.q.Proposals(g.specTask())
	if err != nil {
		return nil, gerr(codeUnavailable, "cannot list proposals: %v", err)
	}
	for _, p := range props {
		g.record(p.Change)
	}
	return map[string]any{"task": g.specTask(), "proposals": props}, nil
}

// --- propose (write path: proposals, never promotions — §4.2) ---

// proposeFromCheckout is the gate half of the observed-set propose loop (build
// spec P1.1). It scans the task's sparse checkout, diffs it against the in-scope
// portion of the base, and returns the overlaid tree plus the paths it touched.
//
// The checkout is sparse — it materializes only the grant's scope subtrees — so
// the base is first narrowed to what the grant covers. Otherwise every path the
// checkout does not contain would read as a deletion, and the whole proposal
// would be refused as out-of-scope. With both sides scoped the same way, the diff
// is exactly the task's own in-scope edits. A change SelectEdits finds outside
// scope is still a refusal, not a silent truncation.
func (g *Gate) proposeFromCheckout(baseTree multihash.Multihash) (multihash.Multihash, []string, error) {
	base, err := worktree.FlattenStates(g.repo.Objects, baseTree)
	if err != nil {
		return nil, nil, gerr(codeInternal, "cannot flatten base tree: %v", err)
	}
	// The checkout is sparse — it holds only the grant's scope subtrees — so the
	// diff compares against the in-scope slice of the base. Comparing against the
	// whole base would read every unmaterialized path as a deletion. The overlay,
	// though, is applied onto the *full* base, so paths the task never checked out
	// survive its proposal untouched.
	scoped := make(map[string]worktree.FileState, len(base))
	for p, s := range base {
		if g.grant.Covers(p) {
			scoped[p] = s
		}
	}
	// A fresh in-memory index: the gate rehashes the checkout on each proposal and
	// never writes a cache file into the sandboxed working tree.
	idx := worktree.OpenIndex(g.checkout)
	work, err := worktree.Scan(g.repo.Objects, g.checkout, idx)
	if err != nil {
		return nil, nil, gerr(codeInternal, "cannot scan checkout %q: %v", g.checkout, err)
	}
	d := worktree.Compare(scoped, work)
	// Any change the checkout carries outside the covered slice is a boundary hit:
	// record each so a later blocked-on-scope report names the paths the task
	// could not write (build spec P1.2).
	for _, e := range append(append(append(append([]string{}, d.Added...), d.Modified...), d.ModeChanged...), d.Removed...) {
		if !g.grant.Covers(e) {
			g.noteBoundaryHit(e, "changed in the checkout but outside the task scope")
		}
	}
	for _, rn := range d.Renamed {
		if !g.grant.Covers(rn.To) {
			g.noteBoundaryHit(rn.To, "changed in the checkout but outside the task scope")
		}
	}
	edits, err := worktree.SelectEdits(d, g.grant.Covers, g.grant.Scopes.String(), nil)
	if err != nil {
		// SelectEdits refuses out-of-scope changes and an empty observed set with a
		// named message; surface it as invalid_args so the agent reads the reason.
		return nil, nil, gerr(codeInvalidArgs, "%v", err)
	}
	proposed := worktree.Overlay(base, work, edits)
	touched := make([]string, 0, len(edits))
	for _, e := range edits {
		touched = append(touched, e.Path)
		if !e.Del { // a deleted path has no working blob to fold into the read set
			g.record(work[e.Path].Hash.Hex())
		}
	}
	newTree, err := worktree.BuildTree(g.repo.Objects, proposed)
	if err != nil {
		return nil, nil, gerr(codeInternal, "cannot build tree: %v", err)
	}
	return newTree, touched, nil
}

func toolPropose(g *Gate, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Message   string `json:"message"`
		Reasoning string `json:"reasoning"`
		Files     []struct {
			Path       string `json:"path"`
			Content    string `json:"content"`
			Executable bool   `json:"executable"`
		} `json:"files"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Message) == "" {
		return nil, gerr(codeInvalidArgs, "message (the change's intent) is required")
	}
	if len(a.Files) == 0 && g.checkout == "" {
		return nil, gerr(codeInvalidArgs,
			"propose needs either file contents or a working tree to observe — "+
				"start the gate with --checkout to propose the checkout's in-scope changes")
	}

	var baseTree multihash.Multihash
	if g.base != nil {
		bt, err := treeOfChange(g.repo, g.base)
		if err != nil {
			return nil, gerr(codeNotFound, "cannot read base tree: %v", err)
		}
		baseTree = bt
	}

	var (
		newTree multihash.Multihash
		touched []string
	)
	if len(a.Files) == 0 {
		// Observed-set propose (build spec P1.1): no file contents were sent, so
		// reconcile the task's checkout against the base and propose every in-scope
		// change — the same reconciliation `varvig propose` and `diff --name-only`
		// perform, so a forgotten file is never dropped from a hand-listed set.
		t, paths, err := g.proposeFromCheckout(baseTree)
		if err != nil {
			return nil, err
		}
		newTree, touched = t, paths
	} else {
		// Explicit-contents propose: the caller sends each file's new content. This
		// is the path a harness with no sandboxed checkout uses.
		files, err := flattenTree(g.repo.Objects, baseTree)
		if err != nil {
			return nil, gerr(codeInternal, "cannot flatten base tree: %v", err)
		}
		touched = make([]string, 0, len(a.Files))
		for _, f := range a.Files {
			path, err := g.resolvePath(f.Path)
			if err != nil {
				return nil, err // out_of_scope, naming the scope (§4.2)
			}
			if path == "" || strings.HasSuffix(path, "/") {
				return nil, gerr(codeInvalidArgs, "invalid file path %q", f.Path)
			}
			blobID, err := g.repo.Objects.Put(object.NewBlob([]byte(f.Content)))
			if err != nil {
				return nil, gerr(codeInternal, "cannot store blob: %v", err)
			}
			mode := uint32(modeFile)
			if f.Executable {
				mode = 0o100755
			}
			files[path] = fileEnt{id: blobID, mode: mode}
			g.record(blobID.Hex())
			touched = append(touched, path)
		}
		t, err := buildTree(g.repo.Objects, files)
		if err != nil {
			return nil, gerr(codeInternal, "cannot build tree: %v", err)
		}
		newTree = t
	}

	prov := object.NewProvenance(object.Provenance{
		Authority:   g.grant.Fingerprint(),
		TaskSpec:    a.Message,
		ContextRead: strings.Join(g.grant.Reads.Hashes(), " "),
		// Reasoning — the plan the agent followed to produce the change — is the
		// half of the message/reasoning split that makes a varvig proposal more
		// than a tree and a commit message (§1.1). Persisted here, surfaced by
		// read_change, and confirmed back in this response (C1).
		Reasoning: a.Reasoning,
	})
	provID, err := g.repo.Objects.Put(prov)
	if err != nil {
		return nil, gerr(codeInternal, "cannot store provenance: %v", err)
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
		return nil, gerr(codeInternal, "cannot sign change: %v", err)
	}
	changeID, err := g.repo.Objects.Put(change)
	if err != nil {
		return nil, gerr(codeInternal, "cannot store change: %v", err)
	}

	pool := spec.Open(g.repo.GitDir())
	if err := pool.Add(g.specTask(), changeID, g.clock().Unix()); err != nil {
		return nil, gerr(codeInternal, "cannot record proposal: %v", err)
	}

	// Confirm what was stored, not what was sent (C0.4): read the provenance
	// object back straight from the store — not through the read-logging query
	// path, which would fold these hashes into the task's own read set — so the
	// caller can verify reasoning (and the rest) actually landed, not an echo.
	storedProv, err := g.repo.Objects.Get(provID)
	if err != nil {
		return nil, gerr(codeInternal, "cannot read back stored provenance: %v", err)
	}
	pv, err := storedProv.AsProvenance()
	if err != nil {
		return nil, gerr(codeInternal, "stored provenance unreadable: %v", err)
	}
	stored := map[string]any{
		"task_spec":    pv.TaskSpec,
		"context_read": pv.ContextRead,
		"reasoning":    pv.Reasoning,
	}

	return map[string]any{
		"task":       g.specTask(),
		"change":     changeID.Hex(),
		"tree":       newTree.Hex(),
		"provenance": provID.Hex(),
		"parents":    hexes(parents),
		"paths":      touched,
		"read_set":   g.grant.Reads.Hashes(),
		"intent":     stored,
		"promoted":   false, // always: the gate can never promote
	}, nil
}

// toolReportBlocked emits the blocked-on-scope outcome (build spec P1.2): the
// task cannot proceed without something outside its scope, so instead of failing
// or working around the boundary it records one signed report — carrying every
// boundary it has already hit plus the concrete thing it needs — bound to its
// intent revision and routed to whoever can widen scope. It never widens scope
// and never moves a ref.
func toolReportBlocked(g *Gate, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Need  string `json:"need"`
		Why   string `json:"why"`
		Unmet string `json:"unmet"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Need) == "" || strings.TrimSpace(a.Why) == "" {
		return nil, gerr(codeInvalidArgs, "need (what you require) and why (why you require it) are both required")
	}
	if g.base == nil {
		return nil, gerr(codeInvalidArgs, "task has no base intent revision to bind a blocked-on-scope report to")
	}
	rep := blocked.Report{
		Intent:    g.base.Hex(),
		Scope:     g.grant.Scopes.String(),
		Need:      a.Need,
		Why:       a.Why,
		Unmet:     a.Unmet,
		Hits:      append([]blocked.Hit(nil), g.boundary...),
		Author:    g.grant.Fingerprint(),
		Timestamp: g.clock().Unix(),
	}
	noteID, err := blocked.Attach(g.repo, g.grant.Signer(), rep)
	if err != nil {
		return nil, gerr(codeInternal, "cannot record blocked-on-scope report: %v", err)
	}
	return map[string]any{
		"outcome":       "blocked_on_scope",
		"report":        noteID.Hex(),
		"intent":        rep.Intent,
		"scope":         rep.Scope,
		"need":          rep.Need,
		"boundary_hits": len(rep.Hits),
		"routed":        true,
		"widened":       false, // always: the task never widens its own scope
	}, nil
}

// --- helpers ---

// addPage attaches pagination metadata to a tool response: an opaque cursor and
// an explicit truncation marker naming which field was cut (§6, "never
// silently").
func addPage(out map[string]any, next string, truncated bool, field string) {
	out["truncated"] = truncated
	if truncated {
		out["cursor"] = next
		out["truncated_field"] = field
		// Surface the §8 `truncated` code on the (successful) response so an
		// orchestrator can key on "continue with the cursor" uniformly, without
		// treating a capped page as an error.
		out["code"] = codeTruncated
	}
}

// evidenceSummary renders a provenance view compactly; nil when the change
// carries no provenance.
func evidenceSummary(p *readapi.ProvenanceView) map[string]any {
	if p == nil {
		return nil
	}
	return map[string]any{
		"authority":     p.Authority,
		"model":         p.Model,
		"model_version": p.ModelVersion,
		"tool_perms":    p.ToolPerms,
		"intent":        p.TaskSpec,
		"context_read":  p.ContextRead,
		// The persisted plan (C1): a reader judges intent, not only the diff.
		"reasoning": p.Reasoning,
	}
}

// blobAt resolves a repo-relative file path to its blob hash at the base,
// returning not_found when there is no such file within scope.
func (g *Gate) blobAt(path string) (multihash.Multihash, error) {
	dir, file := splitDirFile(path)
	listing, err := g.rl.Tree(g.base, dir)
	if err != nil {
		return nil, gerr(codeNotFound, "no directory %q within scope", dir)
	}
	for _, e := range listing.Entries {
		if e.Name == file && e.Kind == object.TypeBlob.String() {
			return multihash.ParseHex(e.Hash)
		}
	}
	return nil, gerr(codeNotFound, "no file %q within scope %q", path, g.grant.Scopes)
}

// changeTouches reports whether a change view added, modified, or removed a path
// at or under prefix.
func changeTouches(cv readapi.ChangeView, prefix string) bool {
	for _, group := range [][]string{cv.ChangedAdd, cv.ChangedMod, cv.ChangedDel} {
		for _, p := range group {
			if p == prefix || strings.HasPrefix(p, prefix+"/") {
				return true
			}
		}
	}
	return false
}

// resolveChange maps a change argument (hash or ref) to an identity, defaulting
// to the task's base. Change metadata (intent, evidence) is not file content, so
// this is not path-scoped; file reads (list_tree/read_file) are.
func (g *Gate) resolveChange(arg string) (multihash.Multihash, error) {
	if strings.TrimSpace(arg) == "" {
		if g.base == nil {
			return nil, gerr(codeNotFound, "task has no base change; pass an explicit change")
		}
		return g.base, nil
	}
	id, err := g.rl.Resolve(arg)
	if err != nil {
		return nil, gerr(codeNotFound, "cannot resolve change %q", arg)
	}
	return id, nil
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

// toolOK wraps a successful result as MCP tool content, always naming the base
// hash the reads were resolved against (§4.1). The JSON payload is provided both
// as text (universally supported) and as structuredContent.
func (g *Gate) toolOK(v map[string]any) map[string]any {
	if _, ok := v["base"]; !ok {
		v["base"] = g.baseHex()
	}
	b, err := json.Marshal(v)
	if err != nil {
		return g.toolErr(gerr(codeInternal, "cannot marshal result: %v", err))
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(b)}},
		"structuredContent": v,
		"isError":           false,
	}
}

// toolErr reports a tool-execution failure in-band (isError) with a distinct,
// machine-readable code and the current scope, so an orchestrator can tell
// "renew the credential" from "asked for something out of scope" (§8).
func (g *Gate) toolErr(err error) map[string]any {
	sc := map[string]any{
		"code":    codeOf(err),
		"message": err.Error(),
		"scope":   g.grant.Scopes.String(),
		"base":    g.baseHex(),
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": err.Error()}},
		"structuredContent": sc,
		"isError":           true,
	}
}
