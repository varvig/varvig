# Loom

A source control system designed for AI agents working in parallel, not for
humans working sequentially. Humans remain a supported consumer, but they are a
rendering target rather than the primary user.

See [`DESIGN.md`](./DESIGN.md) for the full design. This repository is an
implementation of that design, built in Go and shipped as a single portable,
statically-linked binary (design §3).

## Status: Steps 1–5 complete

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
Packfile reading is a later addition; loose objects are supported now.

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
  FORMAT.md            the frozen object-format specification
  WIRE.md              the wire-protocol specification
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
