# Varvig

A source control system designed for AI agents working in parallel, not for
humans working sequentially. Humans remain a supported consumer, but they are a
rendering target rather than the primary user.

See [`DESIGN.md`](./DESIGN.md) for the full design. This repository is an
implementation of that design, built in Go and shipped as a single portable,
statically-linked binary (design §3).

## Status: core build order (steps 1–10) complete, plus the identity/auth/read-API slice and the MCP gate

The ten steps below implement [`DESIGN.md`](./DESIGN.md)'s build order (§6). A
second slice — identity, authorization, and the read API — implements the
[`AUTH.md`](./AUTH.md) build order and is summarized [further down](#identity-auth-and-the-read-api-design-notes-ii).

### Step 1 — the frozen core

Per the design's build order (§6), step 1 is the substrate everything else
layers on:

- **Content-addressed object store** — immutable objects named by the multihash
  of their canonical bytes; atomic writes, idempotent puts, integrity verified
  on read.
- **Merkle DAG** — blob → tree → change objects, where every reference is by
  content identity.
- **Ref compare-and-swap** — named pointers updated only against an expected
  prior value; the concurrency primitive under §1.4.
- **Append-only reflog** — every ref move is recorded; nothing is ever silently
  lost (§2, universal undo).
- **Multihash from day one** — self-describing digests, so hash agility never
  requires a format change (§4.5).

The frozen object format is specified in [`FORMAT.md`](./FORMAT.md).

### Step 2 — packaging, plain working tree, lossless Git export

- **Busybox-style multicall** — one binary is every tool; it dispatches on the
  first argument or on `argv[0]` when invoked under a name like `varvig-commit`
  (§3.1).
- **Plain working tree** — `write-tree` / `commit` capture a real directory of
  real files into objects; `checkout` materializes them back. Regular files,
  executable bits, symlinks, and nested directories are preserved (§2).
- **Lossless bidirectional Git export** — `git-export` writes a repository that
  plain `git` reads (it passes `git fsck`); `git-import` reads one back.
  **Git → Varvig → Git reproduces byte-identical git objects**, including commit
  SHAs, by retaining each imported commit's exact git body as an interop field.
  Verified in CI against the real `git` binary.

Modes are stored in tree entries using Git's own vocabulary
(`100644`/`100755`/`120000`/`40000`), so export is a straight translation.
Import reads both loose objects and **packfiles** (with ofs-/ref-delta
resolution) plus `packed-refs`, so a normally-cloned repository imports cleanly
and re-exports to the original commit SHAs bit for bit.

### Step 3 — P2P sync with a capability-negotiated wire protocol

- **Any peer is a full replica** — the same binary `serve`s an open port and
  `clone`/`fetch`/`push`es against one; there is no separate server (§3.1).
- **Capability negotiation by feature bits, never a version number** — peers
  exchange a `Hello`, intersect their advertised capability tokens, and use the
  frozen core as the always-available fallback (§4.2). See
  [`WIRE.md`](./WIRE.md).
- **Reachability transfer** — sync streams only the objects the receiver lacks,
  pruning the Merkle-DAG walk at everything it already has. Each object is
  verified against its multihash on arrival.
- **Force-with-lease push** — a push advances the peer's ref by
  compare-and-swap against the client's last-observed remote value, so a peer
  that moved underneath you is rejected rather than overwritten (§2).

### Step 4 — provenance and signing, required on change objects

- **Provenance is a first-class object** — a change references a `provenance`
  object recording the acting authority, model + version, sampling parameters,
  tool permissions, the intent (task / context read / reasoning), and the hash
  of the tool binary itself (§1.1, §2.1). It dedups across changes and syncs as
  part of the change's closure.
- **Every native change is signed** — `commit` signs with a per-repo Ed25519
  identity (pure Go, no cgo). The signature covers everything but itself,
  including the provenance id, so tampering with either the change or its
  provenance is detected.
- **`verify` walks the audit chain** — it checks each change's signature and
  provenance, reports the signer, and fails on any broken or missing one.
  Git-imported changes are reported as foreign rather than failed.

### Step 5 — notes and a sandboxed wasm hook/trigger runtime

- **Notes attach metadata without changing an object's hash** (§2) — test
  results, review verdicts, deploy outcomes accrete onto a change as immutable,
  content-addressed note objects chained under `refs/notes/`. The target is
  never touched. `note add` / `note list`.
- **Hooks are sandboxed wasm modules** (§3.2) — a hook is a WASI command
  (reads the event on stdin, exit code = allow/deny) run under
  [wazero](https://wazero.io) with **no filesystem, network, or environment**
  and a memory + time bound. Modules are content-addressed blobs bound to
  events by a manifest at `refs/hooks`, so triggers are portable and versioned
  alongside the code. No bash, no `dlopen`. `commit` fires `pre-commit` (can
  veto) and `post-commit`. `hook set` / `hook list` / `hook run`.

The wasm runtime is the sole third-party dependency
([wazero](https://github.com/tetratelabs/wazero), pure Go, no cgo), keeping the
single-static-binary guarantee intact.

Ref-change triggers extend the same runtime: a `ref-update` hook runs before a
ref moves and can veto it — enforced **server-side on push**, so a policy
(protected branches, deploy gates) holds regardless of the client (§2).

### Step 6 — the affected-set index

- **"What does this change actually affect?"** (§1.3) — `affected` diffs two
  trees to the directly-changed files, then walks a file-dependency graph to
  their transitive dependents. Foundation for semantic conflict detection and
  guided bisect.
- **Merkle-pruned tree diff** — equal subtree ids are skipped whole, so
  unchanged regions cost nothing.
- **Build-graph from import/include directives** — JS/TS, C/C++, and Python
  relative imports resolve to repo paths, and **Go package imports** resolve
  via `go.mod` to the files of the imported package. Edges form only when the
  target actually exists, so there are no false edges.
- **Pluggable wasm analyzers** (§3.3) — a new language is learned without
  recompiling Varvig: register a wasm module against a file extension
  (`varvig hook set analyze:.rb ruby.wasm`); it runs in the same sandbox as
  hooks, receives `{path, content}`, and emits dependency specifiers. Analysis
  caches by content *and* analyzer module, so it stays sound.
- **Degrades to textual** (§5) — a file whose language has no analyzer
  contributes only itself when changed; it never claims safety it can't prove.
- **Content-addressed and incremental** — per-file analysis is cached by blob
  id (a derived, rebuildable index, §4.3), so unchanged files are never
  re-analyzed across commits.

### Step 7 — read/write-set declaration and the transaction scheduler

With hundreds of parallel agents, conflicts are the common case, so
branch-and-merge is replaced by a database-like model (§1.4). This is a library
(`internal/txn`) — the runtime substrate an agent-orchestration layer drives,
not a human command:

- **Declared sets** — each transaction states the path prefixes it reads and
  writes up front. A lock manager admits non-conflicting transactions
  concurrently and blocks conflicting ones (write/write, write/read, read/write
  overlap; read/read never conflicts).
- **The read set is a capability boundary** (§1.4, §2) — a transaction may read
  only within its read+write sets and write only within its write set; a
  violation fails the transaction and never touches the ref.
- **Automatic retry** — a transaction whose commit loses the ref
  compare-and-swap re-derives from the new base and re-runs its logic, so
  disjoint work converges with no human in the loop. All correctness rests on
  the same ref CAS primitive (§2).
- **Pluggable admission ordering** (`txn.Ordering`, tickets M2) — which of two
  *conflicting* transactions commits first is a module boundary, not a goroutine
  race. The scheduler ranks the batch (`InputOrder`, `PriorityOrder`, or a
  `ScoreOrder` the Stage 2.5/3 scorers plug into) and admits conflicting work in
  rank order via per-transaction predecessor gating, while disjoint work stays
  parallel. `Scheduler.Plan` exposes the ranking as a pure function — the
  deterministic-replay artifact: same transactions + same ordering ⇒ same
  admission order. Swapping the ordering changes only the sequence, never what
  runs (that is the promotion policy's job, kept separate).

### Step 8 — the regeneration-based merge driver

Merge-by-regeneration (§1.2): when two changes conflict, rather than forcing a
textual diff3, re-run the losing change's *intent* against the new base.

- **Three-way merge with a recursive base** — finds the *best common
  ancestors* (all maximal common ancestors, not just the first). A linear
  history has one; a criss-cross history has several, which are recursively
  merged into a synthesized virtual base so the merge measures change against a
  base that already reconciles the ancestors (the two-ancestor case is exact).
  Files changed on one side, or identically on both, resolve trivially; the
  Merkle tree makes untouched subtrees free.
- **Textual fast path** — a line-level three-way merge (`diff3`) resolves
  non-overlapping edits to the same file cleanly.
- **Regeneration on conflict** — an overlapping conflict hands the incoming
  change's recorded intent (its provenance: task, context, model — from step 4)
  plus the three file versions to a `Regenerator`, which re-derives the change
  against the new base. The model is out-of-process behind that interface
  (§3.3); the driver never embeds one.
- **Graceful fallback** — with no regenerator (or one that declines), the file
  keeps standard diff3 conflict markers and the merge reports it unresolved
  rather than committing. `varvig merge <ref|id>` uses the textual path (no live
  model wired in this environment) and leaves markers in the working tree on
  conflict.

### Step 9 — speculation store, scoring, promotion, and garbage collection

Branching becomes search (§1.5): produce many ephemeral attempts, score them,
promote the winner, and reclaim the rest — with retention designed in, since at
speculation volume it is a first-order problem (§1.5, §5).

- **Speculation pool** (`internal/spec`) — candidates are grouped under a task,
  one file per candidate (`.varvig/spec/<task>/<change>`), so concurrent agents
  add attempts with independent atomic writes and no branch-name ceremony.
- **Scoring against an objective** — a `Scorer` interface ranks candidates; the
  objective (a test suite, a wasm scorer) is injected, never embedded (§3.3).
- **Promotion** — `spec promote` advances a real ref to the best candidate by
  compare-and-swap; the winner becomes permanent, losers stay in the pool.
- **Retention** — `spec prune` keeps the top-K by score and drops the rest from
  the pool; dropping a candidate stops protecting it, it does not delete it.
- **Garbage collection** (`internal/gc`) — mark-and-sweep from a root set of
  **all refs, every reflog id, and every live pool candidate**. Because reflog
  ids are roots, anything recoverable through the reflog survives GC —
  **universal undo is preserved** (§2). `gc [--dry-run]`.
- **Reflog retention** — because the reflog otherwise pins every attempt
  forever, `gc --prune-reflog <dur> [--keep N]` expires reflog entries older
  than `<dur>` (always keeping each ref's last N) before sweeping. This is the
  opt-in escape hatch that lets GC reclaim speculation volume, trading undo
  depth beyond the retained window for space (the §1.5-vs-§2 tension, resolved
  by making expiry explicit).

### Step 10 — conformance suite and cross-version interop matrix

Compatibility that isn't tested is just a promise (§4.7). See
[`CONFORMANCE.md`](./CONFORMANCE.md).

- **The suite is a content-addressed artifact** — `internal/conformance/
  vectors.json` pins the frozen surfaces (object encodings + identities,
  multihash framing, wire frame format, and unknown-field / unknown-type
  round-trips), and its own multihash is the suite's stable identity.
- **A hard gate** — `varvig conform` and `go test ./internal/conformance/` fail
  loudly on any drift; a `TestSuiteIDStable` check makes an artifact change
  visible in review. Legitimate format evolution is additive-only.
- **The cross-version matrix** — every conformance-bearing release must satisfy
  the same suite; CI builds each tagged release in history and runs its
  `conform`, so the matrix grows as releases accrue. `varvig conform --emit` /
  `--id` are the comparison points.

## Identity, auth, and the read API (Design Notes II)

The second design note — identity, authorization, and how clients read the
store — layers entirely above the frozen core, changing no object type, wire
frame, or on-disk layout (the conformance suite is untouched). See
[`AUTH.md`](./AUTH.md). Cited sections below are from that note.

- **SSH identity, reused** (`internal/sshkey`, `internal/identity`, §2) — a
  user's existing `~/.ssh/id_ed25519` *is* their identity; the fingerprint is
  the identity, and there is nothing to register. Resolution tries ssh-agent
  (`SSH_AUTH_SOCK`), then the key file read directly, then a `~/.varvig/keys`
  fallback. Ed25519 only. The SSH wire formats are hand-rolled and cgo-free, so
  no dependency is added. `whoami`, `key init`.
- **The repo is the trust store** (`internal/trust`, §3) — `.varvig.d/allowed_keys`
  is a versioned table of principals with path-prefix scopes and
  `read`<`propose`<`promote` rights. Comments, blank lines, and unknown columns
  round-trip byte-for-byte (§3.1). `trust list` / `trust check`.
- **Signed ref updates** (`internal/refupdate`, §5) — the load-bearing
  mechanism: a ref update is a *signed compare-and-swap assertion* whose
  authority is in the payload, so it can be relayed through untrusted peers and
  verified at its destination. Canonical encoding with the same critical /
  non-critical unknown-field rule as the object format; a verification pipeline
  of signature → expiry (with skew) → promote-scope authority → object presence
  → nonce replay guard → atomic CAS → reflog audit. `promote --scope --ttl`.
- **The read API** (`internal/readapi`, §7) — one query layer behind both an
  HTTP/JSON server and the CLI plumbing, so the two can never drift; nothing
  reads the on-disk layout directly. Hash-addressed routes with content
  negotiation and branch→hash redirects; `/change` leads with intent, not the
  diff. `serve --read-only` binds a 0600 Unix socket (filesystem permissions are
  the authentication); `read {object,tree,blob,change,log,refs,proposals}` is
  the JSON plumbing.
- **Task credentials and the MCP gate** (`internal/task`, `internal/mcp`, §5, §8)
  — the gate agents talk to, built into the core binary rather than shipped as a
  client. A task is an ephemeral in-sandbox Ed25519 key, granted a scope (which
  *is* the read set), propose-only, and an expiry; `task start` mints one and a
  scoped sparse checkout. `mcp` serves a JSON-RPC gate over stdio bound to that
  credential, holding no authority of its own: coarse domain tools
  (`fetch_tree`, `fetch_blob`, `fetch_change_with_intent`, `fetch_evidence`,
  `list_proposals`, `propose`), scope enforced on every path, a hash in every
  response, every resolved hash logged into the change's provenance, and writes
  that are proposals into the speculation pool — never ref promotions.
- **The daemon** (`internal/daemon`, §6.1) — the long-running local process that
  holds the grant table in memory and keeps the repo warm. `task start` mints a
  grant *in the daemon* and opens a per-task 0600 Unix socket; the ephemeral key
  then lives only in the daemon (never on disk, never on the wire) and a reaper
  revokes it at expiry. `varvig mcp` is the stdio entry point a harness spawns:
  by default it *relays through the daemon* — minting an ephemeral task and
  bridging stdio to its per-task socket, stopping the task on disconnect — so
  the credential and warm repo stay in the daemon. `daemon status` / `daemon
  stop` and `task list` / `task stop` manage it; `mcp --connect` bridges a
  specific socket and `mcp --standalone` forces an in-process gate. Without a
  daemon the same commands still work standalone — one process, its own key.
- **Socket auth** (`internal/peercred`, §7.4) — every daemon and read-API Unix
  socket is 0600 *and* gated by a kernel peer-uid check, so the kernel — not just
  the file mode — confirms the connecting process's uid. Cgo-free on Linux
  (`SO_PEERCRED`), macOS and FreeBSD (`LOCAL_PEERCRED`); other platforms fall back
  to the 0600 mode.

## Governance and tickets (Design Notes III)

The third design note — governance, attestations, and intent intake (tickets as
unmaterialized change records) — layers almost entirely above the frozen core.
See [`TICKETS.md`](./TICKETS.md). It is mostly build-on-top work, but it turns on
a handful of decisions that are cheap now and impossible after first run, so
those land first:

- **Unmaterialized changes** (`internal/object`, decision D1) — a ticket is a
  change with intent but no tree. "No tree" is encoded as the explicit absence
  of the tree field, never as the empty-tree hash, so an unmaterialized change
  and a change materialized to the empty tree stay two distinct states that hash
  differently and can never be silently conflated. `Change.Materialized`
  reports the difference; checkout of an unmaterialized change fails with the
  named `object.ErrUnmaterialized`. Pinned into the frozen suite as the
  `change/unmaterialized` conformance vector.
- **Reserved namespaces** (`internal/reserved`, decision D6) — the ref namespace
  `refs/varvig/tickets/` and the note namespaces `varvig/attest`,
  `varvig/external`, and `varvig/score` are fixed now, in the object-store
  milestone, because identity cannot be retrofitted. Note namespaces may be
  slash-separated so the hierarchical governance spaces are addressable. An old
  binary lists, syncs, and preserves an empty reserved namespace intact.
- **Already held by the core** — unknown object *kinds* round-trip (D2), notes
  replicate like any other ref and an incomplete closure fails loudly (D3), and
  a note pins its target as a GC root (D4). These needed no new code; see
  §0.1 of the note for where each is exercised.

On top of those primitives, the governance and ticket layers are built:

- **Ticket identity & revision chain** (`internal/ticket`, §1.2) — a ticket is a
  *ref* (`refs/varvig/tickets/<id>`), not a raw hash: `tickets new` mints a signed,
  unmaterialized genesis revision and points the ref at it; `tickets revise`
  appends an immutable intent revision and moves the ref by compare-and-swap, so
  a bad edit is recoverable from the reflog. The id is the genesis revision's
  hash — stable for life — while the ref value tracks the head. Because approvals
  and scope bind to the revision hash, revising a ticket drops them to the new
  revision (§2.2): `varvig tickets show` reads `pending`/`unschedulable` right
  after a `revise`. `tickets new|revise|list|show|scope|blockers|graph|rank` is
  the lifecycle surface that ties intake, governance, dependencies, and scoring
  together.

- **Attestations** (`internal/attest`, object types `attestation` and
  `principal`, §2) — a governance decision is a *signed decision object bound to
  a specific intent revision hash*, never a `status: approved` field. Status is
  derived from the attestations, not authored. Because the signature covers the
  target hash, an approval **does not survive a spec edit**: a rewrite yields a
  new revision hash that no attestation covers, so it derives back to pending —
  the single most important property in the design, and the one that makes the
  audit chain mean something. Strength is typed `weak` < `delegated` < `strong`,
  recorded at signing and never upgraded: a compromised bridge cannot mint a
  strong approval, because `VerifyWithPrincipal` checks the asserted strength
  against the signer's principal kind. A veto on any ancestor revision makes
  every descendant unpromotable (`PromotionBlocked`), even descendants created
  after the veto. Attestations attach as notes in the reserved `varvig/attest`
  namespace, keyed by the intent hash, so they list by intent, pin the revision
  as a GC root, and sync like any object. The `attestation` and `principal`
  encodings are pinned into the frozen conformance suite. `varvig attest
  approve|veto|list|status` signs and inspects decisions with the active SSH
  identity.
- **Principal registry / org chart** (`internal/principal`, §1.4) — the set of
  keyholders and their kind, stored as a tree at `refs/varvig/principals` and
  moved by compare-and-swap, so the chart is versioned, hash-pinned, diffable,
  and auditable through its reflog. A `principal.Registry` implements
  `attest.KindResolver`, so the strength rule resolves kinds **from the repo**:
  registered as a bridge, even the `varvig attest` author path refuses to mint a
  strong decision; re-registered as a human, the same approval succeeds.
  `varvig principal add|list|remove` administers it, like the trust store.
- **Promotion checkpoint** (`spec.PromoteWithPolicy`, `attest.VetoGate`, M1) —
  the promote path consults an injected `PromotionPolicy` *before* scoring picks
  a winner, so a policy refusal is never outranked by a high score: a refused
  candidate is skipped in favor of a lower-scored admissible one. The
  speculation store stays policy-agnostic (the policy is injected like the
  Scorer); governance supplies the gate. `VetoGate` disqualifies any change
  whose ancestry carries a veto, `ApprovalGate{Required}` also demands an
  approval of a given strength, and `varvig spec promote` applies the veto gate
  by default.
- **Policy as a wasm module** (`attest.WasmPolicy`, §2.5) — who may sign what and
  what suffices to promote is a content-addressed wasm module, versioned
  alongside the code it guards. It runs in the same closed WASI sandbox as hooks;
  the host computes a `PolicyInput` (the change's metadata, whether its ancestry
  is vetoed, every signature-verified attestation) and passes it on stdin, and
  the module exits 0 to admit. The module is named by `refs/varvig/policy`
  (`varvig attest policy set`) and composes with the built-in constraints via
  `AllOf`. Live host functions — a module pulling facts rather than receiving a
  pre-computed context — are the pending M3/M4 refinement.
- **Ticket scope & derived dependencies** (`internal/deps`, §3.1–§3.2) — a
  ticket declares the read/write set that makes it schedulable, stored as a note
  in the reserved `varvig/scope` namespace. Blocking between tickets is then
  *derived*, never hand-declared: two tickets block when their scopes conflict,
  computed with `txn.Conflict` — the exact predicate the scheduler serializes on,
  so the dependency graph and the scheduler share one notion of conflict.
  `deps.Graph` is a pure function of the scopes with no API to add an edge by
  hand; `varvig tickets scope|blockers|graph` declares and queries it.
- **Learned scoring & native backtest** (`internal/score`, §3.3 Stage 3) — the
  throughput half of scheduling, strictly separate from the safety half (the
  promotion policy). `score.Fit` learns a linear scorer from a corpus of pairwise
  decisions (override/veto/"do this first") with a deterministic averaged
  perceptron; features are extracted from repository state, not hand-labelled
  (blast radius from the write set, contention from the derived dependency graph,
  age from the timestamp). The same weights feed the scheduler's `txn.ScoreOrder`,
  so the learned order and the admission order are one thing. Backtesting is
  native: `score.Backtest` replays past decisions and reports every one a
  candidate scorer disagrees with, so a scorer is reviewed before it is promoted
  — governed as code. `score.BuildCorpus` reads the corpus from the repository's
  own recorded decisions (approved tickets should rank above vetoed ones), so the
  backtest replays *real* history, not a hand-built list. `varvig tickets rank`
  and `varvig tickets backtest` expose the loop: learn from recorded approve/veto
  decisions, report agreement, save weights, re-rank. (The Stage 2.5 model-judged
  scorer is out-of-binary by design and enters through the same boundary.)

## Layout

```
varvig/
  cmd/varvig/            single multicall binary (§3.1)
  internal/
    multihash/         self-describing digests
    object/            frozen VVG1 object encoding (blob, tree, change)
    store/             content-addressed object store
    refs/              ref CAS + append-only reflog
    repo/              repository layout wiring the above together
    worktree/          working tree <-> objects (checkout / write-tree)
    gitobj/            git object codec + loose-object store
    gitport/           lossless Varvig <-> Git export/import
    wire/              frozen framing + capability-negotiated handshake
    p2p/               reachability sync: serve / fetch / push
    provenance/        Ed25519 signing, identity, provenance gathering
    notes/             attach metadata to an object without changing its hash
    hook/              sandboxed wasm (WASI) hook/trigger runtime + manifest
    affected/          tree diff + dependency graph -> affected-set index
    txn/               read/write-set scheduler: serialize conflicts, retry on CAS,
                       pluggable admission ordering (M2)
    merge/             three-way merge + merge-by-regeneration driver
    spec/              speculation pool: score, promote, retention
    gc/                mark-and-sweep GC rooted at refs + reflog + pool
    conformance/       frozen-format golden vectors + cross-version suite
    sshkey/            hand-rolled, cgo-free SSH key + ssh-agent primitives
    identity/          resolve the active principal (agent / ssh key / fallback)
    trust/             .varvig.d/allowed_keys: principals, scopes, rights
    refupdate/         signed ref updates: canonical payload + verify pipeline
    readapi/           one read query layer: HTTP/JSON + CLI plumbing
    task/              ephemeral, scoped, propose-only task credentials (§6)
    mcp/               in-process MCP gate over the query layer (§8)
    daemon/            long-running local daemon: grant table + per-task sockets
    peercred/          kernel peer-uid attestation for local sockets (§7.4)
    reserved/          reserved ticket/governance ref + note namespaces (D6)
    attest/            signed governance decisions: sign, verify, derive status
    principal/         versioned org chart: keyholders and their kind (§1.4)
    deps/              ticket scope + derived blocking dependencies (§3.2)
    score/             learned ticket scoring + native backtest (§3.3)
    ticket/            ticket identity as a ref + intent-revision chain (§1.2)
  FORMAT.md            the frozen object-format specification
  WIRE.md              the wire-protocol specification
  CONFORMANCE.md       the conformance suite + cross-version matrix protocol
  AUTH.md              identity, auth, and the read API (Design Notes II)
  TICKETS.md           governance, attestations, intent intake (Design Notes III)
```

## Build

The binary is statically linked with **no cgo**, so it cross-compiles to any
target with a single environment change and installs by copying one file:

```sh
CGO_ENABLED=0 go build -o varvig ./cmd/varvig
```

Cross-compiling, e.g. for arm64 macOS:

```sh
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o varvig ./cmd/varvig
```

## Try it

```sh
varvig init .
echo "hello from an agent" > note.txt
varvig commit -m "first change"          # capture the working tree, sign, advance HEAD
varvig verify                            # check provenance + signatures on the DAG
varvig affected                          # files the tip change touched + dependents
varvig log                               # walk the change DAG
varvig git-export ./out main             # write a repo plain git can read
git --git-dir=./out/.git log --oneline # ...and read it with real git

# peer-to-peer (any peer is a full replica):
varvig serve :9418 &                     # serve this repo to peers
varvig clone localhost:9418 ../copy main # replicate a branch into a new repo
varvig push localhost:9418 main          # push (force-with-lease)

# lower-level plumbing:
id=$(varvig hash-object -w note.txt)     # store a blob, print its identity
varvig cat-object "$id"                  # read it back
varvig show-ref                          # list refs
varvig reflog refs/heads/main            # inspect the append-only log

# identity, trust, and signed promotion (see AUTH.md):
varvig whoami                            # the active principal + fingerprint
mkdir -p .varvig.d                       # the trust store is a versioned file
echo "$(varvig whoami | awk '{print $2}') me / promote" > .varvig.d/allowed_keys
varvig trust check /                     # what may I do here?
varvig promote refs/heads/main "$id" --ttl 3600   # move a ref via a signed update

# read-only query API (one layer, two transports):
varvig read change main                  # intent-first change view, as JSON
varvig read tree main src                # a directory listing
varvig serve --read-only &               # HTTP/JSON over a 0600 unix socket
curl --unix-socket .varvig/read.sock http://localhost/refs

# task credentials + the MCP gate (agents; see AUTH.md §5, §8):
varvig task start --scope src --ttl 1h ./task-a   # ephemeral key + scoped sparse checkout
varvig mcp --scope src --ttl 1h                   # serve the gate over stdio (JSON-RPC 2.0)
# the agent reads within scope, and every propose lands in the speculation pool,
# signed by the task key — never a ref promotion:
varvig read proposals                             # unpromoted speculative states

# ...or run a long-lived daemon so the key persists and many tasks share it:
varvig daemon &                                   # holds grants in memory, serves per-task sockets
varvig daemon status                              # pid, uptime, live task count
varvig mcp --scope src --ttl 1h                   # stdio gate; relays through the daemon if up
varvig task start --scope src --ttl 1h ./task-a   # or mint a persistent task + its socket
varvig task list                                  # the daemon's live tasks
varvig mcp --connect <task.sock>                  # stdio bridge to a specific task socket
```

An identity like `1e20…` reads as: `1e` = blake3, `20` = 32-byte digest length,
followed by the digest — the self-describing multihash envelope.

## Test

```sh
CGO_ENABLED=0 go test ./...
```

The suite covers canonical-form enforcement, byte-exact round-tripping of
unknown fields (§4.4), integrity detection on corruption, and concurrent
compare-and-swap serialization.
