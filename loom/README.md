# Loom

A source control system designed for AI agents working in parallel, not for
humans working sequentially. Humans remain a supported consumer, but they are a
rendering target rather than the primary user.

See [`DESIGN.md`](./DESIGN.md) for the full design. This repository is an
implementation of that design, built in Go and shipped as a single portable,
statically-linked binary (design §3).

## Status: Step 1 — the frozen core

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
  FORMAT.md            the frozen format specification
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
id=$(loom hash-object -w note.txt)     # store a blob, print its identity
loom cat-object "$id"                  # read it back
loom update-ref refs/heads/main "$id"  # point a ref at it (create)
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
