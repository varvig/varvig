# Varvig — Builder Instructions: Context Graph

Implements *Design Note: The Context Graph*. Written to be picked up from a fresh checkout
of the varvig repository with no prior context on this thread.

---

## 0. Before writing any code

### 0.1 Read, in this order

1. `Varvig — Design Notes: A Source Control System for Agents` — the base design.
   §1.3 (affected-set index), §1.6 (queryable history), §2 (notes, what to keep from git),
   §3.3 (wasm analyzers), §4.3 (derived layout churns), §4.4 (unknown fields round-trip),
   §5 (graceful degradation) are load-bearing for this work.
2. `Design Note: The Context Graph` — the design this implements. Read §11 carefully; it is
   not a risk list, it is a set of required structural properties.
3. `Design Addendum: Corrections From First Run` (A1–A8) — in particular A3
   (preserve-or-refuse as a general rule) and A4 (bind to content hashes, never mutable
   identities).
4. `Design Addendum: Checkout Scope` (A9–A11) and `Builder Instructions: Core Unification`
   — this work must land on the unified core, not on one shell.

### 0.2 Hard gates

Do not start until both hold:

| Gate | Why |
|---|---|
| **Tier C of the working-loop build spec has landed** — `reasoning` persisted, `read_change` strict, tool surface matching | The graph is a confident-answer machine built on provenance. Edges written while intent is silently dropped carry provenance that cannot be retroactively trusted, and cannot be distinguished later |
| **Tier U of the core unification has landed** — one core, two shells | A gate-only or CLI-only graph reproduces the v0.2.0 `diff` failure exactly |

If either is incomplete, stop and report. Do not build a temporary path around them.

### 0.3 Survey the existing code and report before building

The design was written against the documented architecture, not against the source. Confirm
each of the following in the actual repository and report the answer. Several of them change
the work.

| # | Question | If the answer is "no" |
|---|---|---|
| S1 | Does a notes subsystem exist, and do notes replicate by default on P2P sync? | Blocks G3 and G7. Notes are the only edge storage. Non-replicating notes mean imported edges silently fail to propagate |
| S2 | Are notes GC roots that pin their targets? | Blocks G1 retention classification. Report the current rule rather than assuming |
| S3 | Does an affected-set index exist? Incremental or from-scratch? What produces its facts — textual, build-graph, or analyzers? | Determines whether G4 is exposure work or construction work. This is the largest single unknown |
| S4 | Is there a wasm host for analyzer modules (§3.3)? | If absent, G4 is limited to whatever the existing index computes. Do not build a wasm host as part of this work |
| S5 | Are `refs/varvig/*` namespaces reserved, and does the ticket ref construction exist? | G1's identity nodes reuse it. If tickets are unbuilt, implement the construction generically in G1 and let tickets adopt it later |
| S6 | Do attestation types exist? | Blocks G7 only. G7 is last for this reason |
| S7 | What is the index's on-disk location and lifecycle? Is it already treated as disposable (§4.3)? | Determines the work in G4's self-invalidation |

Report S1–S7 with file references before starting G1.

---

## 1. Phases

Seven phases. G3 and G4 deliver most of the value and require almost no new storage. G7 is
last because it depends on trustworthy provenance.

### G1 — Node identity

**Build.** Four node classes, per design §3.1.

| Class | Identity |
|---|---|
| Object | Content hash |
| Identity | A ref, plus an append-only chain of resolved revisions |
| External | `<system>:<foreign-id>`, the system tag opaque to the core (`tracker:PROJ-123`) |
| Ephemeral | Content hash, retention-governed |

**Constraints.**

- **No node type is a bare string.** A path is not a node. `src/auth.ts` is a resolution of a
  file-identity node under a given tree, and both parts are recorded.
- Identity nodes reuse the ticket construction: identity is a ref moved by compare-and-swap,
  state is an append-only chain. A rename appends a revision; it never retargets an existing
  edge.
- Endpoint class is **computed from node identity type**, never set by a writer.
- **No vendor name enters core source.** An external node's system tag is opaque connector
  data (`bridge.Link.System`). The core's vendor guard fails the build on a tracker name in
  `internal/` or `cmd/` — source, comments, and test fixtures alike — so node formats, edge
  types, and fixtures use `<system>:<foreign-id>`.

**Acceptance.**
- A symbol rename produces a new revision; edges written before it still resolve to what they
  meant at the time.
- A writer cannot construct an edge whose endpoint class disagrees with its node type — make
  this a type error, not a validation error.
- Node identity survives round-trip through an older binary untouched.

### G2 — Edge record schema

**Build.** The stored edge record, as a note. Fields per design §3.2:

- source and target: hash for object nodes; ref plus resolved revision hash for identity
  nodes; namespaced string for external nodes
- edge type: a producer-qualified opaque string
- observed-under: the tree or commit hash the edge held under
- provenance: producing principal, analyzer module hash where applicable, attestation
  strength
- validity: the range of history over which it was observed to hold

**Constraints.**

- **`DerivedEdge` and `StoredEdge` are distinct types with no conversion function between
  them** (design §11.1). `StoredEdge` is the only type the note writer accepts. There must be
  no function taking a `DerivedEdge` and producing a note. This is the single most important
  structural property in this document.
- **Edge type is opaque to the core.** No storage, sync, or retention code path may branch on
  its value.
- Unknown fields round-trip per §4.4.

**Acceptance.**
- Attempting to persist a derived edge fails to compile.
- A synthetic edge type no binary has seen round-trips through write, sync, and read
  byte-identically.
- Static check: no core code path matches on edge-type values. Add to CI (§2 below).

### G3 — Imported edges

**Build.** Edges from connectors: tracker issue ↔ varvig ticket, CI run ↔ commit, deploy ↔
tree. These are the external-identity notes the ticket bridge already needs — build them once,
here, and let the bridge consume them.

**Constraints.**

- Producing principal is the connector, recorded at `weak` strength (ticket design §2.4). A
  bridge cannot mint stronger.
- Edge binds to the content hash of its varvig endpoint, never to a mutable identity (A4).
- Echo suppression: store last-synced state, drop no-op round-trips.
- The connector names its own system tag; the core neither enumerates nor branches on it.

**Acceptance.**
- An imported edge survives P2P sync and is visible to a peer (depends on S1).
- A bridge cannot write an edge at strength above `weak`, under any input.
- Re-import of unchanged foreign state produces no new edge.

### G4 — Derived edges from the existing index

**Build.** Expose what the affected-set index already computes through the graph query
surface. Scope depends on S3.

**Constraints.**

- **One index, two query surfaces.** Do not build a parallel graph index. If the existing
  index cannot answer graph-shaped queries, extend it; do not duplicate it.
- **The index self-invalidates.** Key it by input tree hash plus the set of analyzer module
  hashes that produced it. A changed analyzer invalidates its own output with no manual cache
  clearing.
- Derived edges exist only in query results. They are never written anywhere.

**Acceptance.**
- Delete the index, rebuild, assert every derived edge is byte-identical and none is lost.
- Incremental and from-scratch index agree on every tree in the test corpus.
- Changing an analyzer module hash invalidates exactly the affected entries.

### G5 — Query API with mandatory coverage

**Build.** Traversal and query, with coverage as a structural part of every result.

**Constraints.**

- **There is no result type without coverage.** Not an optional field, not a flag — the type
  does not exist in that shape. The "cleaner" API must be unwritable (design §11.4).
- **Results are three-valued:** present, absent-under-coverage, unknown-outside-coverage. A
  caller doing boolean logic gets a type error rather than a wrong answer.
- **Results are partitioned by provenance class.** No API returns a flat edge list. Merging
  requires explicit caller opt-in and still carries per-edge class (design §11.3).
- Coverage is computed from analyzer module hashes against the tree — derived, never declared.

**Acceptance.**
- A repo containing an unanalyzed language returns unknown-outside-coverage, distinguishable
  from absence.
- Removing an analyzer turns previously-present answers into unknown, not into absent.
- Static check: no code path constructs a result without coverage.

### G6 — CLI and gate access

**Build.** Graph query through both shells, from the shared core.

**Constraints.**

- Verb parity: identical verb sets, generated from the one schema source (U2).
- Capability disparity: the gate's capability set stays a strict subset (U3).
- **Never gate-only.** This is the failure that motivated the core unification.

**Acceptance.**
- Identical results from CLI and gate for the same query in the same checkout.
- The graph verbs appear in `tools/list`, in the CLI verb table, and in `VARVIG-AGENTS.md` —
  all three generated from one source.
- Every documented parameter is accepted **and used**. No field is accepted and dropped (C1).

### G7 — Asserted edges

**Depends on S6. Build last.**

**Build.** Edges contributed by the execution layer: an agent's claim that two modules are
coupled, that a ticket duplicates another, and so on.

**Constraints.**

- **Assertions never gate.** Conflict detection, scheduling, and promotion consume derived
  edges only. An assertion may inform planning; it may never change a merge outcome (design
  §11.3).
- Provenance carries model, version, sampling parameters, and authority under §2.1.
- **No confidence scores.** Provenance answers "how much should I trust this" in a way a float
  cannot, and a float will be Goodharted.
- Promotion from assertion to durable knowledge is an attestation bound to the edge's content
  hash — signed and auditable, never implicit.

**Acceptance.**
- An asserted edge cannot change a conflict-detection outcome. Construct a case where it
  would if the boundary leaked, and assert it does not.
- A consumer requiring derived-class input refuses an unpromoted assertion.
- A promoted assertion carries an attestation naming the edge hash.

---

## 2. Standing CI invariants

These are not phase acceptance tests. They run on every build, forever, and each one guards a
property that will otherwise erode.

| Invariant | Guards |
|---|---|
| Object store contains **zero** notes with derived provenance | §11.1 — no second source of truth |
| No core code path matches on edge-type values | §11.2 — vocabulary cannot ossify |
| No code path constructs a query result without coverage | §11.4 — gaps cannot read as absence |
| From-scratch index rebuild diffs clean against the incremental one | §11.1 — the index stays disposable, and the rebuild path stays exercised |
| An unknown edge type round-trips through the cross-version interop matrix (§4.7) | §11.2 and §4.4 |
| CLI verb set, gate verb set, and `VARVIG-AGENTS.md` agree | C3, U2 |
| Edge count returns to baseline after speculation states are discarded | §11.5 — retention |
| No vendor/tracker name appears in core source | The existing vendor guard, extended to edge code and fixtures |

---

## 3. Do not build

| Item | Reason |
|---|---|
| A separate graph store or graph database | Edges are notes; derived edges are index content. A third store is the second source of truth this design exists to prevent |
| Confidence scores on edges | Unfalsifiable and Goodhartable. Provenance instead |
| A registry or enumeration of edge types | The absence of a list is the mitigation |
| Model inference inside the core | §3.3 keeps inference out of the binary. The execution layer contributes assertions with provenance |
| A wasm analyzer host, if S4 says none exists | Out of scope. Report and limit G4 accordingly |
| Graph replication protocol | Sync the inputs — notes. Each peer rebuilds its own index |

---

## 4. Definition of done

- S1–S7 answered and reported.
- G1–G6 complete; G7 complete or explicitly deferred on S6.
- All seven standing invariants in CI and passing.
- A query for "what does this change affect" returns the same answer through the graph
  surface and the affected-set surface, from one index.
- Deleting the entire index and rebuilding loses nothing.
- An agent in a task checkout can query the graph through the gate, and an operator can run
  the identical query through the CLI, and the results match.

## 5. Report back if any of these appear

Each indicates a design assumption failed rather than an implementation difficulty.

- **The existing index cannot be extended to graph queries without duplicating it.** Do not
  duplicate. Report — this changes G4 from exposure to construction and needs a decision.
- **Coverage cannot be computed without running analyzers on every query.** The design assumes
  coverage is cheap metadata. If it is not, the three-valued result needs rethinking rather
  than dropping.
- **A caller genuinely needs a flat, unpartitioned edge list.** Report the caller. Do not add
  the API; the partition is the mitigation.
- **Pressure to branch on edge type inside the core.** This is the ossification event. Report
  it rather than resolving it locally.
