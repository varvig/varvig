# Varvig — Design Note: The Context Graph

Response to *Varvig Context Graph* (proposal). Companion to the ticket design and to
addenda A1–A11.

**Position.** The proposal is right that durable project knowledge belongs to varvig rather
than to an execution layer, and right that an agent inference must not silently become
project truth. It is wrong in one structural respect: most of what it describes is not a new
store. It is a query surface over objects varvig already holds, plus one genuinely new
category of fact.

This document takes the idea and fits it to the existing machinery.

---

## 1. What already exists

| Proposal element | Existing mechanism |
|---|---|
| Queryable project knowledge, zoom levels | §1.6 — history as a queryable graph, semantic blame, multi-resolution |
| `File --defines--> Symbol --depends-on--> Symbol` | §1.3 — the affected-set index. This *is* that graph |
| Edge storage without mutating objects | §2 — notes: attach data to an immutable object without changing its hash |
| External identities (trackers, forges) | `notes/varvig/external/` in the ticket design |
| Deterministic vs agent-asserted | §2.1 provenance, plus attestation strength (`strong` / `delegated` / `weak`) |
| Language-specific analysis | §3.3 — wasm analyzer modules, fetched and cached |

The context graph is therefore a **naming and query layer** over four existing things, not a
fifth thing beside them.

---

## 2. Three kinds of edge, three homes

The single most important structural decision. Edges are not uniform, and treating them
uniformly is what turns this into a second source of truth.

| Kind | Example | Storage | Authority | Syncs? |
|---|---|---|---|---|
| **Derived** | `Commit --changes--> File`, `PR --contains--> Commit`, `File --defines--> Symbol` | Index only (§4.3) | None — the objects are authoritative | No. Rebuilt locally |
| **Imported** | tracker issue ↔ varvig ticket, CI run ↔ commit, deploy ↔ tree | Note | The connector, recorded as a `weak`-class principal | Yes, as notes |
| **Asserted** | An agent's claim that two modules are coupled, or that a ticket duplicates another | Note, with provenance and attestation strength | The asserting principal | Yes, as notes |

**Derived edges are never stored as edges.** Storing them creates a second source of truth
that can disagree with the objects, with no way to adjudicate. They are recomputed from
content-addressed inputs, which is exactly what §4.3 permits: indices are caches and may
churn freely because no identity depends on them.

**Consequence for "synchronizes the graph":** varvig syncs the *inputs* — notes — and each
peer rebuilds its own index. Nothing needs a graph replication protocol.

---

## 3. Node identity

The largest piece of design in this proposal, and the one the proposal does not address.

Everything in varvig is content-addressed. `File` and `Symbol`, as written, are mutable
identities — a path can be renamed, a symbol can be renamed, and an edge bound to either
silently points at a different thing while continuing to read as valid.

This is A4 for the fourth time: an approval bound to a ticket ID, a commit bound to a ticket
ID, evidence bound to a proposal ID. Each was fixed by binding to a content hash. The same
rule applies here, with one addition, because some nodes are genuinely conceptual and persist
across content changes.

### 3.1 Four node classes

| Class | Identity | Examples |
|---|---|---|
| **Object** | Content hash | Commit, tree, blob, intent revision, evidence note, wasm module |
| **Identity** | A ref, with an append-only chain of what it resolved to | Ticket, symbol, file-across-time, principal |
| **External** | `<system>:<foreign-id>` — an opaque system tag, never a name the core knows | `tracker:PROJ-123`, `github:org/repo#45`, `ci:run/8891` |
| **Ephemeral** | Content hash, retention-governed | Speculation states, task executions |

**External node identity names no system the core knows.** The tag before the colon is
opaque data chosen by the connector, exactly as `bridge.Link.System` already is — the core
stores it, syncs it, and never branches on it. This is not a stylistic preference: the core
carries a build-failing guard against vendor names appearing in its source, so a tracker
spelled into a node format, an edge type, or a test fixture is a CI failure. Illustrative
names in prose are fine; normative spellings are `<system>:<foreign-id>`.

**Identity nodes reuse the ticket construction exactly** (ticket design §1.2): identity is a
ref moved by compare-and-swap; state is an append-only chain of immutable revisions. A symbol
rename appends a revision; it does not orphan the node or silently retarget it.

**No node type is a bare string.** A path is not a node. `src/auth.ts` is a *resolution* of a
file-identity node at a given tree, and the edge records both the identity ref and the tree
it resolved under.

### 3.2 Edges bind to hashes and record their resolution

An edge stores:

- source and target — hash for object nodes, ref plus resolved revision hash for identity
  nodes, namespaced string for external nodes
- edge type
- the tree or commit hash the edge was observed under
- provenance: producing principal, analyzer module hash where applicable, attestation
  strength
- validity: the range of history over which it was observed to hold

That last field is what makes "which task caused this behavior to change" (§1.6, semantic
blame) answerable rather than approximate.

---

## 4. No confidence scores

The proposal offers optional confidence metadata. Do not build it.

A float is unfalsifiable and immediately Goodharted — §5 already names this failure mode for
tests, and a scored graph invites it at a larger blast radius. The distinction the proposal
actually wants is *what produced this edge*, which is checkable:

- a deterministic analyzer, identified by module hash → verifiable by re-running it
- a connector import → traceable to the foreign system
- an agent assertion → carries the model, version, sampling parameters, and authority under
  §2.1

Provenance answers "how much should I trust this" better than a number, and it can be
audited. Where ranking is genuinely needed, derive it from provenance class at query time
rather than storing an opinion.

---

## 5. Coverage is part of every answer

§5 requires semantic analysis to degrade gracefully to textual rather than claiming safety it
cannot deliver. A query that returns no dependency edge for an unanalyzed language, in a form
indistinguishable from a genuine absence of dependency, is that failure exactly.

**Every query result carries a coverage descriptor:** which paths were analyzed, by which
analyzer module hashes, and which were not analyzed at all. A caller must be able to
distinguish:

- no such edge exists
- no analyzer covers this language
- an analyzer covers it but has not run against this tree yet

Absence is never returned bare.

---

## 6. What "core" means here

The proposal argues the graph belongs in Core rather than in the execution layer. Agreed, and
the word needs splitting, because §4.1 and §4.3 mean different things by it.

| Layer | Contains | Freeze status |
|---|---|---|
| Frozen object format (§4.1) | Nothing new. Edges are notes; notes already exist | Frozen. Untouched by this design |
| Note schemas | Imported and asserted edge records | Versioned, unknown fields round-trip (§4.4) |
| Index and query surface (§4.3) | Derived edges, traversal, coverage | Churns freely. Rebuildable |
| Analyzer modules (§3.3) | Language-specific extraction | Content-addressed wasm, fetched and cached |

**A graph schema must not enter the frozen core.** Freezing an SDLC ontology in 2026 is
precisely what §2's format-neutrality warning exists to prevent — varvig is a data structure,
not a policy engine, and an edge-type vocabulary is policy.

Edge types are therefore an open namespace. Unknown edge types round-trip untouched by older
binaries (§4.4, and A3's general form).

---

## 7. Boundary with the execution layer

The proposal's operating principle — *varvig remembers, the factory thinks and acts* — is
correct and consistent with format neutrality. One sharpening:

- **Core derives edges with deterministic analyzers.** Wasm modules, content-addressed,
  reproducible. Re-running one over the same tree yields the same edges, which is what makes
  derived edges safe to discard and rebuild.
- **Core does not run models to infer edges.** Model inference is deliberately outside the
  binary (§3.3). An execution layer that discovers a relationship contributes it back as an
  *asserted* edge with full provenance — never as a derived one.

The line is reproducibility, not intelligence. Anything a peer cannot independently recompute
is an assertion and is stored as one.

---

## 8. Retention

Notes pin their targets (ticket design D4), so edges pin nodes. With §1.5's thousands of
ephemeral speculation states, naive edge retention buries the store — the risk §5 already
names for speculation, arriving through a new door.

Rules:

- **Derived edges are never retained.** They are index content and die with the index.
- **Edges whose endpoint is ephemeral are collectable with that endpoint.** A speculation
  state's edges do not outlive it.
- **Edges attesting to something durable pin it,** exactly as approvals pin intent revisions.
  An imported edge linking a tracker issue to a merged commit is durable; the same edge to a
  discarded attempt is not.

Classify at write time by endpoint class (§3.1), not by a later sweep.

---

## 9. Where it slots

**After tier C, alongside build step 6.** Two reasons, one of them non-negotiable.

**It is the same index.** §1.3's affected-set index and the proposal's symbol graph compute
overlapping facts from the same analyzer modules. One index with two query surfaces, not two
stores. Building them separately guarantees they disagree.

**It cannot precede reliable provenance.** The graph is a confident-answer machine built on
provenance. The system currently drops the `reasoning` field silently and returns other
agents' changes with `isError: false`. Building an inference-shaped layer over that
reproduces C2's failure at a larger blast radius: plausible answers with no way to detect
they are wrong. Worse, edges written during the unreliable period carry provenance that
cannot be retroactively trusted, and there is no way to distinguish them later.

Tier C first. The graph gets better inputs for free once intent is actually stored.

---

## 10. Initial scope

The proposal's instinct to start small is right. Concretely:

| Step | Build |
|---|---|
| 1 | Node identity: the four classes, with identity nodes reusing the ticket ref construction |
| 2 | Edge record schema as a note: endpoints, type, observed-under hash, provenance, validity range |
| 3 | Imported edges via connectors — the external-identity notes the ticket bridge already needs |
| 4 | Derived edges from the existing affected-set index, exposed through the graph query surface |
| 5 | Traversal and query API with mandatory coverage descriptors |
| 6 | CLI and gate access — one core, two shells, per the unification instructions. Never gate-only |
| 7 | Asserted edges from the execution layer, with attestation strength |

Steps 3 and 4 deliver most of the value and require almost no new storage. Step 7 is the one
that needs the provenance chain to be trustworthy, and it comes last for that reason.

---

## 11. Risks, and the design that resolves them

Each risk below was stated in an earlier draft as something to watch. A risk mitigated by
vigilance is not mitigated — the whole first run is evidence of that: every finding in it was
a rule someone intended to follow.

**The principle applied throughout this section: make the bad state unrepresentable rather
than forbidden.** Three of the five resolve to type-level properties, which is why they hold
under pressure and a coding standard would not.

### 11.1 The graph becomes a second source of truth

**Failure.** A derived edge gets persisted. It then disagrees with the objects it was derived
from, and nothing can adjudicate which is right.

**Design.**

- **Derived edges have no storable representation.** `DerivedEdge` and `StoredEdge` are
  distinct types with no conversion between them. `DerivedEdge` is produced only by the index
  and appears only in query results. There is no function that accepts one and writes a note.
  The failure is a compile error, not a review catch.
- **The index self-invalidates.** It is keyed by the input tree hash and the set of analyzer
  module hashes that produced it. A changed analyzer invalidates its own output without
  anyone remembering to clear a cache.
- **Rebuild is routine, not exceptional.** CI rebuilds the index from scratch on every run and
  diffs against the incremental one. Drift surfaces the day it is introduced rather than
  after months of accumulation. This also keeps the rebuild path exercised, which is what
  makes "the index is disposable" true rather than aspirational.

**Tests.**
- Delete the index, rebuild, assert every derived edge is byte-identical and none is lost.
- Assert the object store contains **zero** notes carrying derived provenance — a standing
  invariant, checked in CI, not a one-time audit.
- Incremental and from-scratch index agree on every tree in the test corpus.

### 11.2 The edge-type vocabulary ossifies

**Failure.** Edge types become an enumerated set in the core, and the 2026 SDLC ontology is
frozen into the format — §2's format-neutrality warning, realized.

**Design.**

- **The core never branches on edge type.** Type is opaque to storage, sync, and retention.
  Only analyzer modules and query callers interpret it. If core behaviour ever needs to
  switch on a type, that is the ossification event and the change should be refused.
- **There is no registry.** The resistance is not "resist pressure to add types" — it is that
  there is no list to add to. Types are producer-qualified strings: `varvig:changes`,
  `tracker:relates-to`, `agent:couples-with`. Qualification prevents collision and makes
  provenance legible in the type itself.
- **Unknown types round-trip** under §4.4 and A3's general form, like any unknown field.

**Tests.**
- A synthetic edge type no binary has seen survives write, sync, index, and query untouched.
- The same, in the cross-version interop matrix (§4.7): new type written by a new binary,
  read and rewritten by an old one, byte-identical.
- Static assertion that no core code path matches on edge-type values.

### 11.3 Asserted edges silently become truth

**Failure.** An agent's claim is consumed as though it were a derived fact, and ends up
gating something.

**Design.** The operative word in the original proposal is *silently*. There is a path from
assertion to trusted; it is signed and auditable, never implicit.

- **Query results are partitioned by provenance class, always.** No API returns a flat edge
  list. The return type separates derived, imported, and asserted, and merging requires an
  explicit caller opt-in that still carries per-edge class. Treating an assertion as derived
  requires writing code that says so.
- **Assertions never gate.** The affected-set index used for conflict detection, scheduling,
  and promotion consumes **derived edges only**. An assertion can inform an agent's planning;
  it can never change a merge outcome. This is §5's degradation requirement applied
  precisely: the system does not claim safety it cannot deliver, and an unreproducible claim
  cannot deliver safety.
- **Promotion is an attestation.** An assertion becomes durable project knowledge by the same
  machinery as a ticket approval: signed, bound to the edge's content hash (A4), attributable
  later. Reuse the attestation types rather than growing a parallel mechanism.

**Tests.**
- An asserted edge cannot change a conflict-detection outcome. Assert directly, with a case
  constructed so it would if the boundary leaked.
- No API returns edges without their provenance class.
- A promoted assertion carries an attestation naming the edge hash; an unpromoted one is
  refused by any consumer requiring derived-class input.

### 11.4 Coverage gaps read as absence

**Failure.** A query returns no dependency edge for an unanalyzed language, indistinguishable
from a genuine absence of dependency. A caller concludes there is no dependency.

**Design.**

- **There is no query result type without coverage.** Not an optional field, not a flag —
  the type does not exist in that shape. The API that would be "cleaner" cannot be written,
  which is the point: this is the mitigation most likely to be traded away for ergonomics, so
  the trade is made unavailable.
- **Results are three-valued:** present, absent-under-coverage, unknown-outside-coverage. A
  caller doing boolean logic on the result gets a type error rather than a wrong answer.
- **Coverage is derived, not declared.** Computed from analyzer module hashes against the
  tree, so it cannot drift from what actually ran.

**Tests.**
- A repo containing an unanalyzed language returns unknown-outside-coverage, distinguishable
  from absence.
- Static assertion that no code path constructs a result without coverage.
- Removing an analyzer changes previously-present answers to unknown, not to absent.

### 11.5 Retention pressure

**Failure.** Edges pin their endpoints (D4), speculation produces thousands of ephemeral
states (§1.5), and the store silently swells.

**Design.**

- **Endpoint class is computed, not declared.** The edge write path derives it from node
  identity type (§3.1). A writer cannot mark an ephemeral edge durable, correctly or
  otherwise.
- **Ephemeral edges share their endpoint's GC root.** They are not a separate retention rule
  with its own sweep; they are collected with the speculation state they attach to, by
  construction.
- **Per-task edge budget, enforced loudly.** Exceeding it fails the task with a named error.
  The failure mode becomes a task erroring at the moment of excess rather than the store
  degrading invisibly over weeks — the same preference for loud failure that A3 applies
  everywhere else.

**Tests.**
- Create N speculation states with edges, discard them, assert edge count returns to
  baseline exactly.
- An edge to an ephemeral endpoint cannot be written with durable classification.
- Budget exceedance fails the task and names the count.

### 11.6 What this does not resolve

Stated because the residue is real and should not be mistaken for coverage.

- **An agent can still believe an assertion.** 11.3 prevents assertions from gating anything,
  but an agent planning work will read asserted edges and act on them. That is the point of
  having them. The mitigation is provenance visibility at query time, not prevention — and it
  means a wrong assertion costs wasted work, which is acceptable, rather than a bad merge,
  which is not.
- **Convention can harden even where the format does not.** 11.2 keeps the core free of an
  edge-type enumeration, but if every connector settles on the same twenty types, tooling
  will assume them. That is ecosystem gravity and no format decision prevents it. It is
  survivable precisely because the core does not encode it: a later divergence costs tooling
  updates, not a format migration.
- **Coverage is honest about analyzers, not about analyzer quality.** A shipped analyzer that
  parses a language badly reports full coverage and wrong edges. Analyzer correctness is a
  separate problem, addressed by module hashing and reproducibility rather than by the graph
  design.
