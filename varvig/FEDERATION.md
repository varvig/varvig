# Federation Support

Implementation of *Design Notes VII: Federation Support* — the changes inside
varvig that let multiple autonomous peers coordinate through repository state.
Capability advertisement, delegation policy, tier packaging, liveness, and
artifact replication are Factory-layer concerns and are **not** implemented here
(spec §6); nothing below teaches varvig about CUDA, container runtimes, or model
families.

## What shipped

| # | Change | Where |
|---|--------|-------|
| 1 | `artifact-ref` object + reachability + `gc --report-external` | `internal/object/artifactref.go`, `links.go`, `internal/gc`, `cmd/varvig` |
| 2 | Environment descriptor object + evidence field | `internal/object/environment.go`, `provenance.go` |
| 3 | Notes replicate by default (loud, per-namespace opt-out) | `internal/p2p/notes.go`, `cmd/varvig/notes_sync.go` |
| 4 | Pin protocol + capability bits | `internal/wire`, `internal/p2p/pin.go` |

### 1. External artifact reference (§1)

`TypeArtifactRef` (type 9) records the identity of and reachability to bytes
that live outside the object model — images, APKs, SBOMs, archives.
`content_hash` is identity; `locators` are hints (sorted, deduplicated, so an
equal locator set encodes identically). A change names the artifact-refs it
produced (`Change.Artifacts`, tag 8); `object.Links()` makes them
reachable-through, and an artifact-ref links to its producing change. So the
external bytes are pinned exactly as long as some reachable change refers to
them. `varvig gc --report-external` prints the `content_hash`/`media_type`/
`locators` of every artifact-ref that went unreachable this pass — varvig
**reports**, it never deletes registry bytes (it holds no credentials there).

All additive: a change with no artifacts encodes byte-identically to a
pre-federation change, so an old peer round-trips an `artifact-ref` it does not
understand rather than dropping it (unknown fields/types are retained verbatim,
FORMAT.md §4.4).

### 2. Environment descriptor (§2)

`TypeEnvironment` (type 10) records `platform`, `toolchains`, `flags`, an
optional `container` (an artifact-ref), and an optional inference `model`
`{id, version, params}`. Maps encode in sorted key order, so identical
environments hash identically across peers and process runs — the property
dedup and comparison both need.

`Provenance` — varvig's evidence object (it records what produced a change, and
is surfaced as `ChangeView.Evidence`) — gains an optional `Environment` field
(tag 10). `SameEnvironmentClass` confines comparison to a class: cross-class is
reported rather than silently equal, and evidence with **no** environment is
*unknown class* and never matches. Selection policy that acts on this is
Factory-layer; varvig provides the primitive.

### 3. Notes replicate by default (§4)

`clone`/`fetch`/`push` now replicate `refs/notes/*` by default — the reverse of
git — so cross-peer evidence and governance state travel with the branch.
`p2p.ReplicateNotesFetch`/`Push`, gated on the `notes-sync` capability,
fast-forward local notes refs and refuse to clobber a divergent chain. Failure
to transfer a note between two `notes-sync` peers is a **loud error**, never a
silent partial. Opt-out is per namespace via a tracked
`.varvig.d/notes-sync.optout`; reserved governance namespaces
(`reserved.IsReservedNoteNamespace`) always replicate and cannot be opted out.

### 4. Pin protocol (§3)

Pins are ordinary refs under `refs/pins/<peer-id>/…`, so they are GC roots with
no new primitive. `PIN`/`UNPIN`/`LISTPIN` wire verbs (gated on the `pin`
capability) let one peer ask another to hold an object. `not_after` is
mandatory; expired pins stop being roots and are reclaimed. Pins are quota'd per
peer and refusal is a normal, visible response (`quota`/`unknown_object`), so a
peer can never exhaust another's disk and a requester learns it must hold the
state itself. A PIN only ever writes under `refs/pins/<peer>/` — it can never
move a head, so it grants disk, not promotion.

### Wire capability bits (§3.4)

Three negotiated tokens, never a version integer: `artifact-ref`, `pin`,
`notes-sync`. A peer refuses to push an `artifact-ref` object to a partner that
does not advertise the `artifact-ref` bit (§1.4 write gate), so a mixed-version
federation never GCs away state a newer peer considers pinned.

## §5 — verified before building

Two behaviours the spec asked us to confirm rather than assume:

- **Partial sync works peer-to-peer.** The sync protocol transfers exactly the
  closure of the objects a peer names in `want`, pruned by `have`
  (`internal/p2p` `Fetch`). A peer joining an attempt fetches that attempt's (or
  a subtree's) closure by naming its root — it does not have to replicate the
  whole repo. Demonstrated by `TestPartialSyncFetchesScopedClosureOnly`
  (fetching one change delivers its closure and nothing of a sibling change) and
  `TestFetchPrunesHaves`. Federation does **not** degrade to full replication.
  Note: the protocol does not *enforce* a scope server-side (a client may name
  any object); scope enforcement remains the read-gate/Factory concern.

- **The model identifier is structured.** Provenance stores `Model`,
  `ModelVersion`, and `Sampling` as three separate fields, and the environment
  descriptor carries `model {id, version, params}`. Regeneration routing can
  match on these field-by-field; no free-text parsing is required.

## Testing

Reachability through `artifact-ref`, mixed-version write gate, environment hash
determinism, evidence class comparison, pin lifecycle + quota, pin
non-escalation, loud notes-sync failure, and partition semantics are covered
across `internal/object`, `internal/gc`, and `internal/p2p` tests. See spec §7
for the intent behind each.
