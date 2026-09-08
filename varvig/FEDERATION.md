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
| 5 | Reserved ref namespaces replicate by default (loud, divergence reported) | `internal/p2p/refsync.go`, `cmd/varvig/refs_sync.go`, `internal/reserved` |
| 6 | Head and namespace sync are independent — one failing does not skip the others | `cmd/varvig/main.go` (`pushToPeer`, `fetchFromPeer`, `fetchHead`) |

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

### 5. Reserved ref namespaces replicate by default (§4, tickets D3)

`clone`/`fetch`/`push` now also replicate the reserved ref namespaces —
`refs/varvig/tickets/*` and `refs/factory/*` — for the same reason notes do.

This closed a real gap rather than adding a feature. `LISTREFS` has always
advertised every ref, and `servePush` has always accepted any ref name, so the
transport could carry these refs from the start; the **client never asked for
them**. The observable symptom: a peer holding a ticket got a clone that
contained the branch and no tickets. Since a ticket's *identity* is a ref
(tickets §1.2), that is not a federation with a shared work queue — it is N
private queues that happen to share a branch. The same held for every Factory
lease, envelope and reservation.

Because the gap was client-side, the fix needs **no new wire verb and no new
capability bit**: it works against peers built before it existed, in both
directions. Gating it on a negotiated token would only have broken that.

**The conflict rule is deliberately weak.** A head has parents and a note has a
`Parent`, so both have an ancestry to fast-forward along. These refs mostly do
not — a lease, an envelope and a reservation are canonical blobs with no links —
so there is no ordering available, and inventing one would mean the core
learning what a lease means, which is the thing reserving a namespace exists to
avoid. The rule is therefore structural only:

| Situation | Result |
|---|---|
| the peer has it, we do not | take it |
| both hold the same id | nothing to do |
| one id reaches the other through `object.Links` | the reaching side supersedes |
| anything else | **diverged**: touch neither side, report it |

Reachability generalises the notes `Parent` walk to the links every object
already exposes, so a ticket's intent chain still fast-forwards while a Factory
blob falls through to the last row. Where the core cannot tell, it refuses and
says so.

**Divergence is a report, not a failure.** Notes abort on a divergent chain
because two peers writing one note chain differently is a fault. Here it is
ordinary: two cells racing the same claim ref is the claim mechanism working, and
aborting would let one contested claim stop every lease and ticket from
replicating. So divergence is collected per ref and printed, and the layer that
knows what the ref means resolves it. A failure to *transfer* a ref between two
peers is still a loud error, never a silent partial — the notes discipline
unchanged.

**A ref name from the network is a path.** These names arrive over the wire and
are written into the ref store, so they are validated at that boundary rather
than left to fail deeper down: one malformed advertisement must not abort
replication of everything alongside it. A varvig peer cannot produce one —
`serveListRefs` resolves each name before advertising it and `Resolve`
validates — so a refusal says something about the peer, and is reported as
such.

**Deletion never propagates**, in either direction. The protocol cannot
distinguish "deleted there" from "never existed there", and guessing deletion
would drop a reservation record — whose absence reads as "nothing was ordered",
the one wrong answer for an irreversible action.

**What does not replicate**, and why each is excluded (`internal/reserved`):

- `refs/varvig/policy` and `refs/varvig/principals` — authority-bearing
  singletons. Adopting a peer's promotion-policy module or org chart is a
  governance decision, not a transport one: on a first fetch there is no local
  value to conflict with, so automatic replication would let any peer we dial
  install who may approve.
- `refs/pins/*` — one peer's retention state. Copying it would import their
  retention obligations onto our disk, which is what the per-peer quota exists
  to bound.
- `refs/heads/*` — stays on the existing explicit-branch path with its
  tracking-ref lease.
- `refs/remotes/*` — local bookkeeping about a peer, meaningless to it.

There is no per-namespace opt-out file, unlike notes: everything replicated here
is reserved, and notes already established that a reserved namespace may not be
opted out of replication. A knob the code then refuses to honour is worse than no
knob. A peer decides what it *accepts* with its ref-update hooks, which run on
every pushed ref regardless of namespace.

### 6. Head and namespace sync are independent

`clone`/`fetch`/`push` attempt the branch head, the notes namespaces and the
reserved ref namespaces **separately**, and a failure in one no longer skips the
others. Every failure is still reported — independent does not mean quiet — and
the command still exits non-zero.

This closed a serious bug rather than adding a convenience. The head moves under
a force-with-lease against a single `refs/remotes/origin/<branch>` tracking ref,
so in a mesh of peers that legitimately differ on the branch, a refused
compare-and-swap is the **normal** case, not the exceptional one. The old code
returned there. The consequences:

- **A peer whose branch had moved received no notes and no reserved refs.** A
  settled lease is the only record that money was spent, and it was being
  withheld from a peer over an unrelated disagreement about code. Reproduced
  with three real repositories: the branch CAS is refused and the peer ends up
  with zero `refs/factory/*`.
- **A peer that had never heard of our branch yielded nothing at all.** `refTip`
  fails, so its tickets, evidence and authority state were never even asked for
  — a cell working a feature branch learned nothing from a peer working
  elsewhere.

Both are the same mistake: treating one namespace's compare-and-swap as a
precondition for namespaces that have their own. `pushToPeer` and
`fetchFromPeer` now collect failures and attempt everything, so what a peer
receives no longer depends on whether it happens to agree about the code.

**Still a limitation, deliberately not fixed here.** There is one tracking ref
per branch, not one per peer, so the head's lease is whatever the last-fetched
peer had. With several peers at most one can accept a head push; the rest are
refused. That is safe — a rejection, never an overwrite — and now harmless to
everything except the head, since the reserved namespaces do not use that lease.
Per-peer tracking refs (`refs/remotes/<peer>/<branch>`) are what would make
multi-peer head pushes work directly rather than through relay.

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
non-escalation, loud notes-sync failure, reserved-ref replication (both
directions, catalogue confinement, divergence reported without clobbering,
fast-forward through ancestry, no capability required), head/namespace sync
independence in both directions, and partition semantics are covered
across `internal/object`, `internal/gc`, and `internal/p2p` tests. See spec §7
for the intent behind each.
