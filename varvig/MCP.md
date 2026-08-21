# The MCP Gate and the Claude Code Plugin

This document describes Varvig's MCP gate — the `varvig mcp` subcommand — as
implemented, and the Claude Code plugin that ships it. It is the companion to
[`AUTH.md`](./AUTH.md) (identity, authorization, the read API, and the task
credential the gate runs as, cited as "auth §N") and to
[`DESIGN.md`](./DESIGN.md) (the storage model, "design §N"). Release and
distribution concerns are in [`RELEASE.md`](./RELEASE.md) ("release §N").

The gate is built into the core binary rather than shipped as a client
(auth §8): it is mandatory in every sandbox, and it writes to core objects
(read-logging into provenance), which is the test the design uses to decide what
belongs in core versus a client.

The governing constraint: **all distribution channels expose an identical tool
surface.** Transport and authentication differ; tools do not. If a channel
appears to need a channel-specific tool, that is a design error — add it to the
shared surface behind a capability flag.

---

## 1. Transport and lifecycle

`varvig mcp` speaks MCP over stdio (`cmd/varvig/mcp.go`). A client spawns it as a
subprocess; there is no daemon to manage and no port to bind. Human-facing
output goes to stderr so it cannot corrupt the protocol channel on stdout.

It reads through the local query layer (`internal/readapi`) rather than the
object store directly, for the same reason clients do: the on-disk layout is a
disposable cache. When a daemon is running for the repo, `mcp` relays through it
(the credential and the warm repo stay in the daemon); with no daemon it falls
back to an in-process gate that mints its own key — a sandbox should need one
command. Both paths construct the same `mcp.Gate` and dispatch the same tools,
so a tool added once is available on every path.

### 1.1 Two operating modes

Determined at startup, never negotiated at runtime:

| Mode | Trigger | Principal | Scope |
|---|---|---|---|
| Task | `VARVIG_TASK` env, or a `.varvig/task` marker in cwd | ephemeral task key | the task's declared read set |
| Session | neither present; interactive human | socket peer uid / local uid | whole repo |

Promotion is exposed in **neither** mode. A human running Claude Code in their
own repo still cannot move a ref through MCP; they use the CLI and sign it. This
keeps "an agent proposed this" and "a human authorized this" as separate acts
even when the same person is behind both.

The resolved mode, principal, and scope are logged to stderr at startup —
silent scope confusion is the most likely support burden.

## 2. Authentication

No ambient authority. The server holds no credential of its own; every operation
runs as the resolved task grant (`internal/task`, auth §5, auth §8). Scope is
enforced at the query-layer call path inside the gate, not merely announced by
the adapter.

- **Task credentials expire.** On expiry, tool calls return the distinct code
  `credential_expired` so an orchestrator can renew rather than treat it as
  failure. Expiry is checked on every call.
- **The capability is the read set.** A grant is one task, one subtree,
  time-bounded — not "access to the repo" (auth §8.1).

## 3. Tool surface

**Small and domain-shaped.** Not one wrapper per read-API endpoint, because the
agent's context window is the scarce resource (auth §8.1). The surface is the ten
read/propose tools plus read-only ticket access. Every tool declares a `title`
and the applicable annotations — a directory-submission requirement asserted by
the release smoke test (release §7) — and the write path is append-only, so no
tool is destructive and **there is no promotion tool**.

| Tool | Purpose | readOnly | destructive |
|---|---|---|---|
| `varvig_task_context` | Who am I, what is my scope, what is my base | ✓ | — |
| `varvig_resolve` | Ref or partial hash → full hash | ✓ | — |
| `varvig_list_tree` | Directory listing at the base + path | ✓ | — |
| `varvig_read_file` | File content, with line ranges | ✓ | — |
| `varvig_find_files` | Glob within scope | ✓ | — |
| `varvig_search_text` | Literal or regex search within scope | ✓ | — |
| `varvig_read_change` | Intent, then evidence, then changed paths | ✓ | — |
| `varvig_read_log` | Change list for a ref or path | ✓ | — |
| `varvig_read_ticket` | Read intent records (tickets) + discussion; list or detail | ✓ | — |
| `varvig_list_proposals` | Unpromoted speculative states | ✓ | — |
| `varvig_propose` | Create objects, propose a state | ✗ | false |

`varvig_read_ticket` is read-only intent intake (tickets §1.2): a ticket is an
unmaterialized change — intent with no tree — so reading one carries no file
content and is not subtree-scoped. Governance over tickets (attestations,
approve / veto) is a **human** decision surface and is deliberately not exposed
to the gate — the same separation that keeps promotion out (§2.1).

There is no promotion tool. Do not add one. Because the write path is
append-only, Varvig has no destructive agent-facing tool at all — a real
differentiator, and one the CI annotation assertion protects (§7).

### 3.1 Every response names a hash

Each response includes the `base` hash it was resolved against. This pins the
agent's reads, makes its work reproducible, and is what lets the scheduler detect
that an attempt was built on a stale base.

### 3.2 `varvig_propose` — the only write

Overlays file contents onto the base tree, signs a speculative change with the
task's ephemeral key, and records it in the speculation pool (`internal/spec`)
under the task. It returns the proposal's change, tree, and provenance hashes and
its read set. It rejects any path outside scope with `out_of_scope`, naming the
scope in the message — an agent that cannot see why it was blocked will retry the
same way.

`destructiveHint: false` is accurate even for a delete: the proposal is a new
immutable state, and nothing existing is modified or removed until a human
promotes it (via the CLI, `varvig promote`).

## 4. Read logging is provenance

The gate is the only component that knows what a task actually read. Every
resolved hash returned by any tool is recorded into the task's read log
(`internal/task` `ReadSet`), which becomes the `ContextRead` field of the change
object on `varvig_propose` (design §1.1, auth §8.2).

- **The hash is recorded, not the path** — paths are ambiguous across bases;
  hashes are not.
- **Recorded on every call, including ones that error after resolving.**
- **Implemented in the query-layer call path** (`internal/mcp/readlog.go`), a
  recording wrapper around the query, not in each tool handler — or the tenth
  tool added would forget.
- **Never transmitted.** It is written to the local object store as provenance
  and stays there.

## 5. Context discipline

The most common way an MCP server becomes useless is flooding the context
window. Every response is capped (`internal/mcp/caps.go`, ~50 KB); truncation is
always explicit — a marker plus an opaque cursor, never silent.

- `varvig_list_tree`, `varvig_read_log`, `varvig_search_text`, and
  `varvig_find_files` paginate with opaque cursors over a deterministic,
  content-addressed ordering, so the same cursor always resumes at the same
  place.
- `varvig_read_file` takes a line range; without one it returns the whole file
  under the cap, else the head plus a cursor.
- `varvig_read_change` is intent-first: intent, then an evidence summary, then
  the changed paths — and the changed-paths section truncates first when the cap
  binds. A diff-first response quietly rebuilds GitHub and loses the premise
  (auth §7.3). The change view carries a path-level breakdown
  (added/modified/removed), not a textual patch.
- `varvig_search_text` returns matches with surrounding lines, capped per file,
  so one pathological file cannot consume the budget.

## 6. Scope on reachability

`trust.Scope.Covers` is a path-string prefix test. That is not enough on its
own: a hash fetched directly could belong to an out-of-scope subtree. The gate
closes this (`internal/mcp/reach.go`) by computing the set of tree and blob
hashes reachable from the task's scope subtree of its base, and checking
direct-hash reads (e.g. `varvig_resolve`) against it. Scope is enforced on
object reachability, not only on path strings — the easy miss. Path-string reads
additionally reject `..` traversal and out-of-scope paths outright.

## 7. Error semantics

Distinct, machine-readable codes (`internal/mcp/errors.go`), carried in the
tool-error `structuredContent` alongside the human message. An orchestrator must
be able to tell "renew the credential" from "the agent asked for something it may
not have."

| Code | Meaning | Caller action |
|---|---|---|
| `out_of_scope` | Path or object outside the task's read set | Do not retry; message names the scope |
| `credential_expired` | Task TTL elapsed | Renew and retry |
| `stale_base` | Proposal built on a superseded base | Re-fetch base; scheduler regenerates |
| `not_found` | No such hash, ref, or path | Do not retry |
| `truncated` | Response hit the cap | Continue with the cursor |
| `unavailable` | Query layer unreachable | Retry with backoff |
| `invalid_args` | Malformed tool arguments | Fix the call |

Every error message states what was attempted and what the current scope is.
Agents recover from specific errors and loop on vague ones.

## 8. Minimum version

`varvig mcp` verifies its build version against an optional floor
(`--min-version` flag or `VARVIG_MIN_VERSION` env) and exits with a clear message
rather than failing obscurely at the first tool call. The floor is what the
plugin declares (§9); an unstamped dev build is not blocked.

## 9. The Claude Code plugin (`varvig/plugins`)

The plugin package is text only and lives in the `varvig/plugins` repository
(release §1). It bundles the MCP-server config and the Varvig skill, and ships
the same tool surface every other channel exposes.

- **`.mcp.json`** runs `varvig mcp`, assuming `varvig` is on `PATH`. No
  download-and-bootstrap step — that reintroduces a hash pin, adds a network
  dependency to plugin load, and duplicates what the MCPB bundle already does
  (release §6).
- **A minimum core version** is declared so `varvig mcp` fails with a clear
  message (§8) rather than at the first tool call.
- **`README.md`** must carry a Privacy Policy section — missing or incomplete
  policies are grounds for rejection. Varvig's honest policy is short: the local
  gate transmits nothing off-machine; repository content reaches the model only
  as part of the user's own conversation; the read log is written to the local
  object store as provenance and is never transmitted to Varvig or anyone else.
- **`plugin.json`** names the plugin `varvig` in the `varvig-tools` marketplace,
  so the install line is `/plugin install varvig@varvig-tools`. Both names are
  permanent.

The MCPB desktop bundle (same tool surface, different transport) is built in this
repo under [`mcpb/`](./mcpb) and covered by release §6.

## 10. Testing (`internal/mcp`, `tools/mcp-smoke.py`)

- **Unit** — scope enforcement, cursor stability, read-log completeness.
- **Annotation assertion** — list the tools, assert every one has a title and the
  applicable hint, assert the surface is exactly the advertised `varvig_*` set,
  and assert no tool named `*promote*` exists. A submission blocker; it runs both as
  a Go test and, against the real binary, in `tools/mcp-smoke.py` on every CI run
  (release §7).
- **Scope-escape suite** — `..` traversal, absolute paths, symlinks pointing
  outside scope (returned as their literal target, never followed), and a hash
  fetched directly that belongs to an out-of-scope subtree — the reachability
  case of §6.
- **Round-trip** — propose via MCP, promote via the CLI, verify the promoted
  change object carries the full read log in its provenance.
