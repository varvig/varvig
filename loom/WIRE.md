# Loom wire protocol

The peer-to-peer sync protocol. It has two layers with deliberately different
stability guarantees (design §4.2).

## Frozen core

The following are frozen and are the always-available fallback — any two peers,
however far apart in version, complete a sync using only these:

- **Stream magic** `LMW1`, sent first by each peer.
- **Frame format**: `uvarint(msgType) uvarint(len) payload[len]`. Varints are
  minimal; a frame's payload is bounded to guard against hostile peers.
- **Hello** exchange (message type 1): `proto`, `caps[]`, `hashes[]`.
- **Core message set**: `ListRefs`/`Refs`, `GetObjects`, `Object`, `Done`,
  `Push`, `OK`, `Error`.

Payloads use the same minimal-varint / length-prefix discipline as the object
format.

## Negotiated layer — feature bits, never a version number

Optional behavior is advertised as named capability tokens in `Hello.caps`.
Both peers compute the **intersection** of their advertised sets; the
intersection is what the session uses. There is no protocol version number with
implied semantics — only feature tokens — so an old peer and a new peer always
converge on a working subset instead of failing a numeric comparison. A peer
that understands no optional capabilities still interoperates over the frozen
core.

Defined capabilities:

| Token     | Effect                                             |
|-----------|----------------------------------------------------|
| `deflate` | `Object` payloads are zlib-compressed on the wire  |

Object integrity is verified against the object's own multihash *after*
decompression, so a negotiated capability can never weaken the content-address
guarantee.

## Sync algorithm

Sync is a Merkle-DAG reachability transfer. Objects are immutable and
content-addressed, so a peer holding an object holds its entire closure.

**Fetch.** The client sends `GetObjects{want[], have[]}`. The server marks the
closure of everything in `have` (that it holds) as a prune set, then walks each
`want`, streaming every object not pruned or already sent, and finishes with
`Done`. The client verifies each object against its id and confirms `want`'s
closure is complete before updating any ref.

**Push.** The client streams the objects the peer lacks for the new tip's
closure (pruning against what the peer already advertised), then sends
`Push{name, old, new}`. The server refuses unless it holds `new`'s full closure,
then performs `CompareAndSwap(name, old, new)`. `old` is what the client last
observed the remote to be (its remote-tracking ref) — **force-with-lease**: a
peer that moved underneath the client causes a rejection rather than a silent
overwrite. Enforcing that `new` descends from `old` (true fast-forward) is left
to the merge/regeneration step.

## Transport

Any `io.ReadWriter`: the same binary serves an open TCP port and dials one
(design §3.1). Transport-level authentication and TLS layer above this protocol
and are not part of the frozen core.
