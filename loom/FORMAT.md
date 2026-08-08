# Loom object format (LOM1) — frozen

This document specifies the parts of Loom that are **frozen forever** (design
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
magic       4 bytes   "LOM1"
objectType  uvarint
fieldCount  uvarint
fields      fieldCount records, each:
              tag     uvarint
              length  uvarint
              value   length bytes
```

Canonical form — enforced on both encode and decode:

1. `magic` is exactly `LOM1`.
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
