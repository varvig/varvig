# Varvig object format (VVG1) — frozen

This document specifies the parts of Varvig that are **frozen forever** (design
§4.1): the object encoding, object identity, and DAG semantics. Hashes produced
under this format end up in signatures, deploy pins, and other repositories'
dependency records, so once written they must remain readable indefinitely.
Everything not specified here (on-disk layout, indices, wire protocol) is a
cache or a negotiated surface and may change freely.

## Identity: multihash

An object's identity is a [multihash](https://multiformats.io/multihash/) over
its canonical encoded bytes:

```
<uvarint hash-code> <uvarint digest-length> <digest bytes>
```

The format never names a hash algorithm; every identity carries its own code.
This is deliberate (design §4.5): a future dual-hash transition needs a new code
and a translation table, not a format change. Current codes:

| Code   | Name     | Digest length |
|--------|----------|---------------|
| `0x12` | sha2-256 | 32            |
| `0x1e` | blake3   | 32 (default)  |

Varints are **minimal**: non-minimal or overlong encodings are rejected, so
every value has exactly one byte representation.

## Object encoding

Every object is a type tag followed by a canonical set of typed fields:

```
magic       4 bytes   "VVG1"
objectType  uvarint
fieldCount  uvarint
fields      fieldCount records, each:
              tag     uvarint
              length  uvarint
              value   length bytes
```

Canonical form — enforced on both encode and decode:

1. `magic` is exactly `VVG1`.
2. All varints are minimal.
3. Fields are sorted by `tag` ascending, and every `tag` is unique.
4. There are no trailing bytes.

A decoder **refuses** any input that violates these rules. Because canonical
form is unique, `decode ∘ encode` is the identity function on bytes, and thus
on identity.

### Unknown fields round-trip (design §4.4)

An object is represented internally as its raw field list; typed accessors are
views. A field whose `tag` a build does not recognize is preserved verbatim and
re-emitted in order. Reading and rewriting an object without changes reproduces
its exact bytes and identity. This is how provenance and signatures written by
a newer build survive handling by an older one: **preserve-or-refuse, never
silently degrade.** An object of an unknown *type* is likewise preserved intact.

### Reserved tag ranges

| Range                       | Purpose                                            |
|-----------------------------|----------------------------------------------------|
| `1 … 0x0FFF_FFFF`           | Semantic core tags, assigned per object type       |
| `0xF000_0000 … 0xFFFF_FFFF` | Interop metadata (not part of the semantic core)   |

Interop-range tags carry data needed to reproduce foreign formats exactly but
which does not participate in Varvig's own semantics. They round-trip like any
other unknown field. The Git bridge uses `0xF000_0001` to retain an imported
commit's exact git object body so that re-export reproduces a byte-identical
git commit (and thus an identical git SHA-1).

## Object types

### Blob (`objectType = 1`)

| Tag | Meaning        |
|-----|----------------|
| 1   | raw content    |

### Tree (`objectType = 2`)

| Tag | Meaning       |
|-----|---------------|
| 1   | entry list    |

The entry list is serialized into the field value:

```
count     uvarint
entries   count records, sorted by name ascending, names unique:
            nameLen  uvarint
            name     nameLen bytes
            mode     uvarint     (filesystem mode bits)
            kind     uvarint     (1 = blob, 2 = tree)
            idLen    uvarint
            id       idLen bytes (multihash of the referenced object)
```

Because entries reference children by multihash, a tree's identity depends on
its children's content: this is the Merkle DAG.

### Change (`objectType = 3`)

A history node — the analogue of a commit, deliberately thin for step 1.
Intent, provenance, and signing are added later as **new tags**, which older
builds preserve untouched.

| Tag | Meaning                         |
|-----|---------------------------------|
| 1   | tree id (multihash)             |
| 2   | parent list                     |
| 3   | message (UTF-8)                 |
| 4   | timestamp (uvarint, unix secs)  |
| 5   | author (UTF-8)                  |
| 6   | provenance id (multihash)       |
| 7   | signature blob                  |

Tags 6 and 7 are optional at the codec level so that git-imported and legacy
changes remain representable; the commit and verify layers require both on
native changes (design §2.1).

### Provenance (`objectType = 4`)

Records who or what produced a change (design §1.1 intent, §2.1 signed
provenance). All fields optional; emitted only when set.

| Tag | Meaning                              |
|-----|--------------------------------------|
| 1   | authority (UTF-8)                    |
| 2   | model (UTF-8)                        |
| 3   | model version (UTF-8)                |
| 4   | sampling parameters (UTF-8)          |
| 5   | tool permissions (string list)       |
| 6   | tool binary hash (multihash)         |
| 7   | task specification (UTF-8)           |
| 8   | context read (UTF-8)                 |
| 9   | reasoning / plan (UTF-8)             |

### Note (`objectType = 5`)

Metadata attached to another object without changing that object (design §2).

| Tag | Meaning                        |
|-----|--------------------------------|
| 1   | target id (multihash)          |
| 2   | namespace (UTF-8)              |
| 3   | payload (opaque bytes)         |
| 4   | parent note id (multihash)     |
| 5   | timestamp (uvarint, unix secs) |
| 6   | author (UTF-8)                 |

Notes for a `(namespace, target)` pair form a chain via tag 4, whose head is
tracked by a ref under `refs/notes/`. The target object is never modified, so
its identity is stable while metadata accretes.

### Hook config (`objectType = 6`)

The hook manifest (design §3.2), referenced by `refs/hooks`.

| Tag | Meaning        |
|-----|----------------|
| 1   | entry list     |

The entry list is `count` records of `eventLen event moduleLen module`, binding
an event name to a wasm module blob's id. Modules are content-addressed, so
triggers are versioned alongside the code they guard.

### Signatures

The signature blob (change tag 7) is `uvarint(scheme) bytes(pubkey)
bytes(sig)`; scheme 1 is Ed25519. A signature covers the change's canonical
bytes **with the signature field omitted** — which still includes the
provenance id (change tag 6), so the signature transitively commits to the
provenance object's content. The object model treats the blob as opaque; its
interpretation lives above the frozen core, keeping crypto policy out of the
format (design §2 format neutrality).

The parent list is serialized into the field value:

```
count    uvarint
parents  count records, sorted by id bytes ascending, unique:
           idLen  uvarint
           id     idLen bytes
```

## Refs and the reflog

Refs are named pointers to object identities, updated by **atomic
compare-and-swap** — the concurrency primitive under design §1.4 and §2. Every
successful update appends to an **append-only reflog**, the universal-undo
substrate: no ref state is ever silently lost.

These are repository mechanics rather than object-format rules; the on-disk
representation of refs and logs is a cache and may change. Only the object
encoding and identity above are frozen.
