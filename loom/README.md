# Loom

A source control system designed for AI agents working in parallel, not for
humans working sequentially. Humans remain a supported consumer, but they are a
rendering target rather than the primary user.

See [`DESIGN.md`](./DESIGN.md) for the full design. This repository is an
implementation of that design, built in Go and shipped as a single portable,
statically-linked binary (design §3).

## Status: Steps 1–10 complete — the full design build order is implemented

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
  first argument or on `argv[0]` when invoked under a name like `loom-commit`
  (§3.1).
- **Plain working tree** — `write-tree` / `commit` capture a real directory of
  real files into objects; `checkout` materializes them back. Regular files,
  executable bits, symlinks, and nested directories are preserved (§2).
- **Lossless bidirectional Git export** — `git-export` writes a repository that
  plain `git` reads (it passes `git fsck`); `git-import` reads one back.
  **Git → Loom → Git reproduces byte-identical git objects**, including commit
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
  recompiling Loom: register a wasm module against a file extension
  (`loom hook set analyze:.rb ruby.wasm`); it runs in the same sandbox as
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

### Step 8 — the regeneration-based merge driver

Merge-by-regeneration (§1.2): when two changes conflict, rather than forcing a
textual diff3, re-run the losing change's *intent* against the new base.

- **Three-way merge** — finds the merge base (DAG ancestor), then merges each
  file. Files changed on one side, or identically on both, resolve trivially;
  the Merkle tree makes untouched subtrees free.
- **Textual fast path** — a line-level three-way merge (`diff3`) resolves
  non-overlapping edits to the same file cleanly.
- **Regeneration on conflict** — an overlapping conflict hands the incoming
  change's recorded intent (its provenance: task, context, model — from step 4)
  plus the three file versions to a `Regenerator`, which re-derives the change
  against the new base. The model is out-of-process behind that interface
  (§3.3); the driver never embeds one.
- **Graceful fallback** — with no regenerator (or one that declines), the file
  keeps standard diff3 conflict markers and the merge reports it unresolved
  rather than committing. `loom merge <ref|id>` uses the textual path (no live
  model wired in this environment) and leaves markers in the working tree on
  conflict.

### Step 9 — speculation store, scoring, promotion, and garbage collection

Branching becomes search (§1.5): produce many ephemeral attempts, score them,
promote the winner, and reclaim the rest — with retention designed in, since at
speculation volume it is a first-order problem (§1.5, §5).

- **Speculation pool** (`internal/spec`) — candidates are grouped under a task,
  one file per candidate (`.loom/spec/<task>/<change>`), so concurrent agents
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
- **A hard gate** — `loom conform` and `go test ./internal/conformance/` fail
  loudly on any drift; a `TestSuiteIDStable` check makes an artifact change
  visible in review. Legitimate format evolution is additive-only.
- **The cross-version matrix** — every conformance-bearing release must satisfy
  the same suite; CI builds each tagged release in history and runs its
  `conform`, so the matrix grows as releases accrue. `loom conform --emit` /
  `--id` are the comparison points.

## Layout

```
loom/
  cmd/loom/            single multicall binary (§3.1)
  internal/
    multihash/         self-describing digests
    object/            frozen LOM1 object encoding (blob, tree, change)
    store/             content-addressed object store
    refs/              ref CAS + append-only reflog
    repo/              repository layout wiring the above together
    worktree/          working tree <-> objects (checkout / write-tree)
    gitobj/            git object codec + loose-object store
    gitport/           lossless Loom <-> Git export/import
    wire/              frozen framing + capability-negotiated handshake
    p2p/               reachability sync: serve / fetch / push
    provenance/        Ed25519 signing, identity, provenance gathering
    notes/             attach metadata to an object without changing its hash
    hook/              sandboxed wasm (WASI) hook/trigger runtime + manifest
    affected/          tree diff + dependency graph -> affected-set index
    txn/               read/write-set scheduler: serialize conflicts, retry on CAS
    merge/             three-way merge + merge-by-regeneration driver
    spec/              speculation pool: score, promote, retention
    gc/                mark-and-sweep GC rooted at refs + reflog + pool
    conformance/       frozen-format golden vectors + cross-version suite
  FORMAT.md            the frozen object-format specification
  WIRE.md              the wire-protocol specification
  CONFORMANCE.md       the conformance suite + cross-version matrix protocol
```

## Build

The binary is statically linked with **no cgo**, so it cross-compiles to any
target with a single environment change and installs by copying one file:

```sh
CGO_ENABLED=0 go build -o loom ./cmd/loom
```

Cross-compiling, e.g. for arm64 macOS:

```sh
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o loom ./cmd/loom
```

## Try it

```sh
loom init .
echo "hello from an agent" > note.txt
loom commit -m "first change"          # capture the working tree, sign, advance HEAD
loom verify                            # check provenance + signatures on the DAG
loom affected                          # files the tip change touched + dependents
loom log                               # walk the change DAG
loom git-export ./out main             # write a repo plain git can read
git --git-dir=./out/.git log --oneline # ...and read it with real git

# peer-to-peer (any peer is a full replica):
loom serve :9418 &                     # serve this repo to peers
loom clone localhost:9418 ../copy main # replicate a branch into a new repo
loom push localhost:9418 main          # push (force-with-lease)

# lower-level plumbing:
id=$(loom hash-object -w note.txt)     # store a blob, print its identity
loom cat-object "$id"                  # read it back
loom show-ref                          # list refs
loom reflog refs/heads/main            # inspect the append-only log
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
