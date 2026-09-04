// Package mcp is the MCP gate, built into the core binary rather than shipped as
// a client (auth design §8). The gate is the component agents talk to: it is
// mandatory in every sandbox and it writes to core objects (read-logging into
// provenance, §8.2), which is exactly the test the design uses to decide what
// belongs in core versus a client.
//
// The gate holds NO authority of its own (§8, the confused-deputy warning). It
// is bound to one task credential and does no more than that credential allows:
//
//   - The capability is the read set. A grant is one task, one subtree,
//     time-bounded — not "access to the repo" (§8.1). Every path a tool touches
//     is checked against the grant's scope.
//   - Reads are logged into provenance. Every resolved hash is recorded so it can
//     be folded into the change a proposal produces — audit and provenance are
//     one mechanism (§8.2).
//   - Writes are proposals, never promotions (§8.1). A tool can create objects
//     and add a speculative state; it can never move a ref. Promotion is a
//     separate, human-gated step.
//   - Hashes come back in every response, so the agent's reads are pinned and its
//     work is reproducible (§8.1).
//
// It is in-process: it uses the query layer directly and does not force the read
// API contract to stabilize before it is ready (§8, "reduces pressure on the
// read API").
package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/blocked"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/readapi"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/task"
)

const (
	serverName = "varvig-mcp"
	// defaultProtocolVersion is used only when a client's initialize omits one;
	// otherwise the gate echoes the client's requested version.
	defaultProtocolVersion = "2024-11-05"
)

// serverVersion is what the gate advertises in initialize's serverInfo. It
// defaults to a placeholder and is overwritten by the binary's build version
// via SetServerVersion, so a released varvig reports its release tag here
// rather than a number frozen in this package.
var serverVersion = "0.1.0"

// SetServerVersion sets the version advertised in the MCP handshake. The core
// binary calls this once at startup with its build-stamped version, keeping the
// version tag in a single source of truth (release design §2). An empty string
// is ignored so a caller cannot blank out the advertised version.
func SetServerVersion(v string) {
	if v != "" {
		serverVersion = v
	}
}

// Gate is a running MCP gate bound to a single task credential. It is created
// per task and serves that task's connection for the credential's lifetime.
type Gate struct {
	repo  *repo.Repo
	q     *readapi.Query // raw query, for scope/reachability bookkeeping
	rl    *readLog       // logging query wrapper the tools read through (§5)
	grant *task.Grant
	base  multihash.Multihash // the pinned state the task reads and proposes from

	reach map[string]bool // memoized in-scope reachable object set (§9.4); nil until first use

	// boundary accumulates every scope-boundary refusal this task hit, so a
	// blocked-on-scope report carries all of them at once rather than the task
	// emitting one failure per hit (build spec P1.2). Its length is the
	// boundary-hit metric reported by varvig_task_context.
	boundary []blocked.Hit

	// checkout is the task's sparse working tree, if the gate was given one. When
	// set, varvig_propose with no files observes it (build spec P1.1). Empty means
	// the gate never touches a filesystem and a proposal must send file contents.
	checkout string

	// mode and principal describe how this session was resolved (§2.1): "task"
	// or "session", and who the principal is. Reported by varvig_task_context.
	mode      string
	principal string

	// now returns the current time, for grant-expiry checks; nil defaults to
	// time.Now, so tests can pin the clock.
	now func() time.Time
}

// NewGate builds a gate over repo r, bound to grant g, reading and proposing
// against base (the change the task started from; may be nil for an empty repo).
func NewGate(r *repo.Repo, g *task.Grant, base multihash.Multihash) *Gate {
	q := readapi.New(r)
	return &Gate{repo: r, q: q, rl: newReadLog(q, g.Reads), grant: g, base: base}
}

// SetIdentity records the resolved operating mode and principal (§2.1) so
// varvig_task_context can report them. Called once at startup; if never called,
// the gate reports "task" with the grant's fingerprint as principal.
func (g *Gate) SetIdentity(mode, principal string) {
	g.mode, g.principal = mode, principal
}

// resolvedMode returns the operating mode, defaulting to "task" — a gate always
// runs as a scoped task credential even when a mode was not explicitly set.
func (g *Gate) resolvedMode() string {
	if g.mode == "" {
		return "task"
	}
	return g.mode
}

// resolvedPrincipal returns the principal, defaulting to the task key
// fingerprint (the credential every operation actually runs as, §3).
func (g *Gate) resolvedPrincipal() string {
	if g.principal == "" {
		return g.grant.Fingerprint()
	}
	return g.principal
}

// SetClock overrides the gate's clock (tests pin expiry).
func (g *Gate) SetClock(now func() time.Time) { g.now = now }

// SetCheckout binds the gate to a materialized working tree (the task's sparse
// checkout). Once set, varvig_propose called with no file contents observes this
// tree — it reconciles it against the base and proposes every in-scope change,
// the same observed-set reconciliation `varvig propose` performs (build spec
// P1.1). Left unset, the gate never touches a filesystem and a proposal must
// carry its file contents inline.
func (g *Gate) SetCheckout(dir string) { g.checkout = dir }

// baseHex is the base change hash this task resolves reads against, or "" for a
// fresh repo. Every tool response names it (§4.1).
func (g *Gate) baseHex() string {
	if g.base == nil {
		return ""
	}
	return g.base.Hex()
}

func (g *Gate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// Serve runs the JSON-RPC request loop over rw until the peer closes the stream
// (io.EOF) or a transport error occurs. One gate serves one connection.
func (g *Gate) Serve(rw io.ReadWriter) error {
	c := newConn(rw)
	for {
		req, err := c.read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			// A decode failure desyncs the stream; there is no reliable id to
			// answer, so report and stop.
			return fmt.Errorf("mcp: read: %w", err)
		}
		if req.JSONRPC != jsonrpcVersion {
			if !req.isNotification() {
				_ = c.replyError(req.ID, codeInvalidRequest, "jsonrpc version must be \"2.0\"")
			}
			continue
		}
		if err := g.dispatch(c, req); err != nil {
			return err
		}
	}
}

// dispatch routes one request. It returns an error only for transport failures;
// protocol and tool errors are reported in-band to the client.
func (g *Gate) dispatch(c *conn, req *request) error {
	switch req.Method {
	case "initialize":
		return g.handleInitialize(c, req)
	case "notifications/initialized", "initialized":
		return nil // notification: no reply
	case "ping":
		if req.isNotification() {
			return nil
		}
		return c.replyResult(req.ID, struct{}{})
	case "tools/list":
		if req.isNotification() {
			return nil
		}
		return c.replyResult(req.ID, map[string]any{"tools": toolList})
	case "tools/call":
		if req.isNotification() {
			return nil
		}
		return g.handleToolsCall(c, req)
	default:
		if req.isNotification() {
			return nil
		}
		return c.replyError(req.ID, codeMethodNotFound, "unknown method: "+req.Method)
	}
}

// handleInitialize answers the MCP handshake. The gate advertises only the tools
// capability; it echoes the client's protocol version when given one.
func (g *Gate) handleInitialize(c *conn, req *request) error {
	if req.isNotification() {
		return nil
	}
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(req.Params, &params)
	pv := params.ProtocolVersion
	if pv == "" {
		pv = defaultProtocolVersion
	}
	return c.replyResult(req.ID, map[string]any{
		"protocolVersion": pv,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
		"instructions": "varvig read-gate scoped to " + g.grant.Scopes.String() + ". " +
			"Reads are logged into provenance; you may propose changes but never promote.",
	})
}

// --- scope helpers (the capability is the read set, §8.1) ---

// scopePath is the grant's scope as a repo-relative directory ("" for the whole
// repo). A read or proposal defaults to this path.
func (g *Gate) scopePath() string {
	return g.grant.Scopes.Primary()
}

// resolvePath maps a possibly-empty request path to a concrete repo-relative
// path and enforces that it lies within the grant's scope. An empty path means
// the scope root. A ".." segment is rejected outright — path-string traversal is
// the first thing the scope-escape suite tries (§9) — and the coded out_of_scope
// error names the scope so an agent that cannot see why it was blocked does not
// retry the same way (§8).
func (g *Gate) resolvePath(p string) (string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		p = g.scopePath()
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			g.noteBoundaryHit(p, "path escapes the task scope")
			return "", gerr(codeOutOfScope, "path %q escapes the task scope %q", p, g.grant.Scopes)
		}
	}
	if !g.grant.Covers(p) {
		g.noteBoundaryHit(p, "path is outside the task scope")
		return "", gerr(codeOutOfScope, "path %q is outside the task scope %q", p, g.grant.Scopes)
	}
	return p, nil
}

// record folds resolved hashes into the task's read set (§8.2).
func (g *Gate) record(hashes ...string) {
	for _, h := range hashes {
		g.grant.Reads.Record(h)
	}
}

// noteBoundaryHit records that the task reached a path or capability outside its
// scope (build spec P1.2). It is a metric and an accumulator, not a refusal —
// the caller still returns the out_of_scope error. Duplicate paths are collapsed
// so a retried read is one boundary, and the count stays the scope-accuracy
// measure the ticket design wanted.
func (g *Gate) noteBoundaryHit(path, reason string) {
	for _, h := range g.boundary {
		if h.Path == path {
			return
		}
	}
	g.boundary = append(g.boundary, blocked.Hit{Path: path, Reason: reason})
}

// boundaryHits is the number of distinct scope boundaries this task has hit.
func (g *Gate) boundaryHits() int { return len(g.boundary) }
