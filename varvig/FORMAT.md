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

**Tag 1 (tree) is nullable.** A change with no tree is *unmaterialized*: it
carries intent but no materialization — the object underlying a ticket
(tickets §1.1, decision D1). Unmaterialized is encoded as **explicit absence of
the tag**, never as an empty-valued tag and never as the empty-tree hash. This
matters because a change materialized to the *empty tree* (a real repository
state with no files) is a legitimate, different thing: it carries tag 1 set to
the empty tree's identity. The two states therefore differ in their canonical
bytes and hash differently, and the distinction can never be silently
conflated. Checkout of an unmaterialized change is a specific named failure,
not an empty working tree.

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
| 10  | environment id (multihash)           |
| 11  | task scope (UTF-8)                    |

Tag 11 is the task scope the change was produced under, recorded so the
scheduler can re-verify a change's self-described scope against its own task
record at promotion (checkout-scope addendum, F4; AUTH §5). It is optional and
additive — a change made outside a scoped task leaves it unset — so all existing
conformance vectors carry no tag 11 and are unaffected.

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

### Attestation (`objectType = 7`)

A signed governance decision bound to a specific intent revision hash
(tickets §2.1). Status is never a stored field; it is derived from the set of
attestations bound to a revision. Because the signature covers the target hash,
editing the spec — which yields a new revision hash — leaves an approval
attached to the bytes that were actually read and signed, so it does not carry
forward (tickets §2.2).

| Tag | Meaning                                               |
|-----|-------------------------------------------------------|
| 1   | target intent revision id (multihash)                 |
| 2   | decision (uvarint: 1 approve, 2 veto, 3 delegate, 4 request-change) |
| 3   | strength (uvarint: 1 weak, 2 delegated, 3 strong)     |
| 4   | timestamp (uvarint, unix secs)                        |
| 5   | rationale (UTF-8, optional)                           |
| 6   | policy module id in force at signing (multihash, optional) |
| 7   | signature blob                                        |

Strength is recorded at signing time and **never upgraded** (tickets §2.4): a
bridge, which signs on behalf of a keyless principal, can only ever produce a
weak attestation, and no code path raises weak to strong. The signature (tag 7,
excluded from the signed bytes) commits to the target, decision, and strength,
so any mutation invalidates it.

### Principal (`objectType = 8`)

A keyholder (tickets §1.4). Principal records are content-addressed objects, so
the org chart is versioned, hash-pinned, diffable, and auditable — including
retroactively. Agent-specific provenance fields are additive future tags that
older builds preserve untouched.

| Tag | Meaning                                     |
|-----|---------------------------------------------|
| 1   | Ed25519 public key (32 bytes)               |
| 2   | display name (UTF-8)                        |
| 3   | kind (uvarint: 1 human, 2 agent, 3 bridge)  |

### Signatures

The signature blob (change tag 7, attestation tag 7) is `uvarint(scheme)
bytes(pubkey) bytes(sig)`; scheme 1 is Ed25519. A signature covers the object's
canonical bytes **with the signature field omitted**. For a change that still
includes the provenance id (change tag 6), so the signature transitively commits
to the provenance object's content; for an attestation it includes the target,
decision, and strength. The object model treats the blob as opaque; its
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

### Reserved namespaces (tickets §1.3, decision D6)

The governance and ticket layers are built purely on top of the primitives
above, but their **names** are reserved now, before first run — identity is the
one thing that cannot be retrofitted. Reservation changes no frozen format: ref
names and note namespaces already accept arbitrary values. See package
`internal/reserved` for the canonical constants.

| Namespace                       | Purpose                                         |
|---------------------------------|-------------------------------------------------|
| `refs/varvig/tickets/<id>`      | ticket identity (a ref moved by CAS)            |
| `refs/varvig/tickets/<id>/spec` | current intent revision, if separated           |
| note ns `varvig/attest`         | signed approve / veto / delegate decisions      |
| note ns `varvig/external`       | foreign tracker IDs and sync watermarks         |
| note ns `varvig/score`          | computed, cached scoring output                 |

A note namespace `N` lives at `refs/notes/N/<target>`; note namespaces may be
slash-separated, so the hierarchical governance spaces above are addressable. A
build that has never heard of these namespaces still lists, syncs, and preserves
them intact: an empty reserved namespace is simply the absence of any ref, so it
is neither an error nor something garbage collection can reclaim.
