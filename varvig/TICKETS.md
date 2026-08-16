# Varvig Tickets — Design Notes: Governance and Intent Intake

Companion to `DESIGN.md` (*A Source Control System for Agents*). Section references in
the form §N.N refer to that document.

Status: no instance of varvig has ever run. Every on-disk format, ref namespace, and
host ABI is still free. This document therefore separates **decisions that must be made
before first run** from **work that can layer on afterwards** — because after first run
the first set becomes impossible and the second stays cheap.

---

## 0. Summary

A ticket is not a new kind of thing. A ticket is a change record whose intent exists but
whose materialization does not yet. The ticket system is therefore not a second product
bolted to the side of the VCS; it is the same object at an earlier point in its life,
plus three additions:

1. **Attestations** — signed approve / veto / delegate decisions, attached as notes to a
   specific intent version hash.
2. **A policy checkpoint** in the promotion path, evaluated by a content-addressed wasm
   module.
3. **Projections** — Jira, GitHub, and whatever comes next, as §1.7 rendering targets
   that happen to accept input.

Everything else — identity, immutability, replication, offline operation, undo, audit —
is inherited from the core and is not reimplemented.

Two governing principles:

- **Authority flows one way.** varvig is source of truth. External trackers are
  projections. Data flows both directions; authority never does.
- **Humans supply the objective, not the ordering.** At agent throughput a product owner
  cannot rank four hundred tickets. They can express constraints, and they can compare
  two tickets. The system harvests both and does the sort.

---

## 0.1 Implementation status in this repository

This document is the full design. Only a subset is built so far — deliberately, the
before-first-run parts (§6.2), because those are the ones that become impossible once an
object is written under the frozen format.

| Item | Status | Where |
|---|---|---|
| **D1** — nullable tree / explicit unmaterialized discriminator | **implemented** | `internal/object/change.go` (`Change.Materialized`, `ErrUnmaterialized`); frozen vector `change/unmaterialized` in the conformance suite; checkout guard in `cmd/varvig` |
| **D2** — unknown *object kinds* round-trip | **already held** | `object.Decode`/`Encode` preserve any type tag; `TestOpaqueUnknownType`, conformance `opaque/unknown-type` |
| **D3** — notes replicate by default; a peer that cannot fetch them fails loudly | **already held** | notes are refs under `refs/notes/`; `p2p` lists them and `hasClosure` refuses an incomplete closure |
| **D4** — a note is a GC root that pins its target | **already held** | `object.Links` for a note includes its target; note refs are GC roots (`internal/gc`) |
| **D5** — wasm host ABI feature-bit negotiated, never version-numbered | **pending** | wire capability bits exist (`internal/wire`); host-ABI negotiation for policy modules is future work |
| **D6** — reserve the ref and note namespaces | **implemented** | `internal/reserved`; notes now accept the hierarchical `varvig/attest` form |

Build-on-top governance layers landed so far:

| Layer | Status | Where |
|---|---|---|
| **Attestations** (§2.1–§2.4) — signed decisions bound to a version, strength typing, derived status | **implemented** | `object.Attestation`/`object.Principal` (types 7/8); `internal/attest` (sign/verify, `Derive`, `PromotionBlocked`); frozen vectors `attestation/approve`, `principal/human` |
| **Veto blocks descendants** (§2.3) — the veto half of the promotion checkpoint | **implemented** | `attest.PromotionBlocked` walks ancestors for a veto |
| **Promotion checkpoint** (M1, §4) — policy consulted before scoring in the promote path | **implemented** | `spec.PromoteWithPolicy` + `spec.PromotionPolicy`; `attest.VetoGate` / `attest.ApprovalGate` / `attest.AllOf`; wired into `varvig spec promote` by default |
| **Policy as a wasm module** (§2.5) — content-addressed, sandboxed policy | **implemented** (context-passing form) | `attest.WasmPolicy` runs a module in the WASI sandbox against a host-computed `PolicyInput`; `refs/varvig/policy`; `varvig attest policy set/show/clear`. Live host functions (M3/M4) are the pending refinement |
| **Principals / org chart** (§1.4) — content-addressed keyholder records | **partial** | `object.Principal` + `attest.PrincipalSet`; a versioned org-chart ref is future work |
| **Scoring / bridge** (§3, §5) | **pending** | build-on-top work (§6.4) |

Everything else in §1–§5 above the object model (the wasm policy module, scoring stages,
the Jira/GitHub bridge) is **build-on-top** work (§6.4) and is not yet present. The design
below is the target; the tables above are the current truth.

---

## 1. Object Model

### 1.1 An unmaterialized change record

A ticket is a change record (§1.1) carrying task specification, context, provenance, and
authorship — with no tree.

**Decision (D1, implemented):** "no tree" is encoded as an explicit absence — the tree
tag is omitted from the change object entirely — *not* as the empty-tree hash. A change
that materializes to an empty tree is a legitimate and meaningful state (a repository
with no files); a change that has not been materialized at all is a different thing.
Conflating them would be a one-line convenience now and an unfixable ambiguity later,
because both would encode to the same bytes and the distinction could not be recovered.

`Change.Tree` is nullable; `Change.Materialized()` tells the two states apart. Checkout of
an unmaterialized change fails with the named error `object.ErrUnmaterialized` — not an
empty working tree.

### 1.2 Identity is a ref; state is an append-only chain

Same construction as refs over commits.

| Concern | Mechanism |
|---|---|
| Stable ticket identity | A ref, moved by compare-and-swap (§2, atomic CAS) |
| Mutation (rescope, rewrite, retitle, reprioritize) | Append a new immutable intent revision; move the ref |
| Undo | Reflog. A director's bad Friday reprioritization is recoverable by construction |
| Replication | Ordinary P2P ref sync. No new protocol |

Nothing here is new machinery. It is the existing machinery pointed at a new namespace.

### 1.3 Reserved namespaces

Reserved now, populated later (D6, implemented in `internal/reserved`). Retrofitting
identity is the one thing that cannot be done after first run — the same argument that put
multihash in step 1.

```
refs/varvig/tickets/<id>          ticket identity
refs/varvig/tickets/<id>/spec     current intent revision (if separated)
notes/varvig/attest/              signed decisions        (note namespace varvig/attest)
notes/varvig/external/            foreign tracker IDs and sync watermarks
notes/varvig/score/               computed and cached scoring output
```

Namespace reservation is free and lives in the object-store milestone, not the ticket
milestone.

### 1.4 Principals

A principal is a keyholder. The system does not distinguish human from agent anywhere in
the object format — a director may be either, and switching one for the other is a
personnel change, not a schema change.

A principal record carries: public key, display name, kind (`human` / `agent` /
`bridge`), and, for agents, the provenance fields from §2.1 (model, version, sampling
parameters, tool permissions, authority delegated from whom).

Principal records are content-addressed objects. The org chart is versioned, hash-pinned,
diffable, and auditable — including retroactively: "who was allowed to approve billing
changes in March" is a query, not an interview.

---

## 2. Authority

### 2.1 Attestations, not status fields

Do not store `status: approved`. Store an approval as a signed decision object attached
to a **specific intent revision hash**, in the same shape as the test evidence of §1.3.

An attestation carries: target hash, decision (`approve` / `veto` / `delegate` /
`request-change`), signing principal, timestamp, strength (§2.4), optional rationale, and
the hash of the policy module in force at signing time.

Status is then *derived*: a function of attestations plus scheduler state plus evidence.
It is computed, cached in the `varvig/score` note namespace, and never authored.

### 2.2 Approval binds to a version, and does not carry forward

Editing the spec produces a new intent revision, which no attestation covers. The
approval does not follow the ticket; it stays attached to the thing that was actually
read and signed.

This is §4.4 applied to governance: preserve or refuse, never silently degrade. An
approval that silently survived a spec rewrite would be the governance equivalent of an
old client dropping unknown fields — the audit chain still looks intact and no longer
means anything.

Re-approval cost is managed by the material-change predicate (§6.1), not by weakening the
binding.

### 2.3 Veto

A veto is a signed refusal against an intent revision. Effects:

- Every state descending from that revision becomes **unpromotable**.
- Nothing is deleted. Speculation output simply becomes GC-eligible (§1.5).
- A veto can therefore land *after* the work has run, which is exactly what you want when
  running the work was cheap.

Veto is not the same as delete, and there is deliberately no delete. A withdrawn ticket
is a vetoed ticket with a rationale.

### 2.4 Attestation strength

Not all signatures mean the same thing, and the difference must be visible rather than
smoothed away.

| Strength | Meaning | Typical source |
|---|---|---|
| `strong` | The signing principal's own key over the exact target hash | Human or agent director acting directly |
| `delegated` | Signed under authority explicitly granted by another principal, recorded and bounded | Agent acting for a director within scope |
| `weak` | Signed by a bridge on behalf of a principal who holds no key | Jira workflow transition, GitHub review |

`weak` exists because Jira has no keys. A Jira workflow transition is not a signature; it
is a bridge asserting that a transition occurred. Typing it honestly lets promotion policy
decide what it is worth — probably sufficient for a docs change, never sufficient for
billing.

**Rule: strength is recorded at signing time and never upgraded.** A bridge cannot mint
strong attestations, and no later process promotes weak to strong.

### 2.5 Policy as a wasm module

Who may sign what, and what suffices to promote, is a wasm module (§3.2) that is itself a
content-addressed object in the repo. It is versioned alongside the code it guards,
sandboxed, and portable.

*Implemented (context-passing form):* `attest.WasmPolicy` runs a content-addressed module
in the same closed WASI sandbox as hooks — no filesystem, network, environment, or
unbounded clock. The host computes a `PolicyInput` (the change's metadata, whether its
ancestry is vetoed, and every signature-verified attestation with its decision, strength,
and signer) and passes it on stdin; the module exits 0 to admit, nonzero to refuse. The
module is stored as a blob and named by `refs/varvig/policy` (`varvig attest policy set`),
and it plugs into the promotion checkpoint as one more `PromotionPolicy` composed with the
built-in constraints via `AllOf`. Exposing *live host functions* to the module (verify a
signature, query the affected-set index, read scheduler state) — so a policy can pull
facts rather than receive a pre-computed context — is the M3/M4 refinement (D5), and
layers on without changing this shape.

The attestation records the policy hash in force at signing. Policy changes therefore do
not rewrite history, and "was this approved under the rules that applied at the time" is
answerable.

---

## 3. Scheduling and Priority

### 3.1 Scoping is what makes a ticket schedulable

A ticket without a declared read set and write set (§1.4) is **unschedulable** by
definition. Refinement — turning a vague human request into a declared transaction — is
agent-performed work, and it is the single highest-value thing the system does for
adoption, because it is legible to the human before anything runs.

The declared read set doubles as checkout scope and capability boundary, exactly as in
§1.4. A ticket's scope *is* its sandbox.

### 3.2 Dependencies are derived, not linked

Two tickets block each other if their write sets overlap. That is computed from the
affected-set index, not declared by a human dragging arrows in a UI.

Epics are parent intents whose materialization is a set of child intents. The hierarchy
is the DAG; there is no separate hierarchy feature.

### 3.3 Priority, in three stages

Do not attempt to write a scoring function up front. Nobody can specify those weights,
and a wrong one is worse than none because it launders arbitrary ordering as objectivity.

**Stage 1 — manual priority and a queue.** P0–P3, then age. Exactly Jira. Ship this. The
parallelism win comes from the transaction scheduler, not from clever ranking, so this
costs nothing real and can run for months.

**Stage 2 — hard constraints.** This is the part worth hand-writing, and it is easy
because it is a list of rules rather than a function:

- nothing touching payments promotes without a `strong` human attestation
- nothing exceeding N tokens or N minutes runs without approval
- nothing merges without passing evidence (§1.3)
- nothing promotes whose ancestor carries a veto

Constraints carry the safety. Score is only throughput optimization. Keep them in
separate modules so this stays obvious.

**Stage 2.5 — model-judged score (optional).** An LLM scores each ticket and emits a
one-line rationale. Model version pinned in provenance like any other agent output. At
ticket volumes the cost is negligible, and the rationale is the affordance that lets a
human notice the scorer is wrong.

**Stage 3 — learn the ordering from decisions already made.** Every override, veto, and
"do this one first" is a labelled pairwise comparison. Features are already computed:
estimated cost, blast radius from the affected-set index, count of tickets unblocked,
age, component, requester. Fit weights to the comparisons.

Because the scorer is a content-addressed wasm module and the full DAG is retained,
**backtesting is native**: replay last quarter, rank with the candidate scorer, and show
the director every case where it disagreed with them. Promote the scorer only if that
review passes. A scorer is code and is governed as code.

### 3.4 Overrides are signed

Manual override stays available forever, as an explicit pinned constraint — but recorded
as a signed decision with provenance. The point is not to discourage overrides. The point
is that six months later you can ask how often the scheduler was overruled, by whom, and
whether they were right.

---

## 4. Lifecycle

```
request → scoped → approved → scheduled → speculation fan-out → promotion → merged
                ↘ vetoed (terminal, non-destructive)
```

| Transition | Gate |
|---|---|
| request → scoped | Agent refinement produces declared read/write sets |
| scoped → approved | Attestation of sufficient strength per policy module |
| approved → scheduled | Transaction scheduler admits it; no write-set conflict held |
| scheduled → fan-out | Speculation store; thousands of ephemeral attempts (§1.5) |
| fan-out → promotion | Scoring **and** policy checkpoint **and** evidence (§1.3) |
| promotion → merged | Ref CAS, as any other change |

Every state above is derived from attestations, evidence, and scheduler state. None of it
is a stored field that anyone can set.

---

## 5. Mirroring to Jira, GitHub, and Others

### 5.1 Shape

A wasm trigger on the ticket ref namespace, plus a mapping from ticket ref to foreign ID
stored in the `varvig/external` note namespace. External IDs accrete onto immutable
objects without touching any hash — precisely the primitive §2 kept notes for.

The bridge is a peer like any other (§3.1, multicall). There is no "integration server."

### 5.2 Field mapping enforces the asymmetry

| Direction | Fields |
|---|---|
| **Write-only into the tracker** (derived; local edits are overwritten on next sync) | status, priority rank, blocked-by links, estimated cost, affected set, evidence summary |
| **Editable, flows back** | title, description / spec, labels |
| **Deliberate exception** | a priority *nudge* field — an input to scoring, never the output |

The nudge field is what makes the asymmetry tolerable to humans. They keep a lever; it
just feeds the objective instead of overwriting the ordering.

### 5.3 Inbound edits

An inbound tracker edit becomes a new intent revision authored by principal
`jira:alice@corp`, with provenance recording arrival through the bridge. Approvals do not
survive it (§2.2). A workflow transition becomes a `weak` attestation (§2.4).

### 5.4 Echo suppression

Store the last-synced hash in both directions in the external note and drop no-op
round-trips. Boring, and the thing that breaks first in every bridge ever written. Test
it explicitly (§7.5).

### 5.5 The adoption story

A human writes a vague ticket in the tool they already use. An agent refines it into a
declared read/write set and writes the refinement back as a comment. The human sees
exactly what the system committed to *before* it runs, in Jira, without learning anything
new.

That single loop is the adoption route. Everything else in this document can be invisible
to them.

---

## 6. Changes to Existing Code

### 6.1 Nothing in the frozen core changes semantically

Object encoding, DAG semantics, and ref CAS (§4.1) are untouched by the ticket system as
such. The items below are core-level only because they must be *decided* before first run.

### 6.2 Decide before first run

| # | Decision | Why it cannot wait | Status |
|---|---|---|---|
| **D1** | Nullable tree / explicit unmaterialized discriminator (§1.1) | Empty-tree ambiguity is unrecoverable once written | implemented |
| **D2** | Unknown *object kinds* round-trip, not just unknown fields | §4.4 was written about fields. An old binary meeting a ticket-shaped object must preserve-or-refuse, never silently drop it | already held |
| **D3** | Notes replicate by default; a peer that cannot fetch notes must fail loudly | Git's notes do not replicate by default. Inherit that and approvals silently fail to propagate. Most dangerous default in the design | already held |
| **D4** | A note is a GC root that pins its target (§1.3) | An approval must pin the exact intent revision it signed, forever, or retention eats the audit chain | already held |
| **D5** | Wasm host ABI extension is feature-bit negotiated, never version-numbered | Host functions are a compatibility surface: new modules meet old binaries. Same argument as §4.2 | pending |
| **D6** | Reserve the ref and note namespaces (§1.3) | Same argument as multihash: identity cannot be retrofitted | implemented |

D1–D6 are cheap today and impossible later. They belong in build-order steps 1–5, ahead
of any ticket work. D2, D3, and D4 are satisfied by the core as it already stands (see
§0.1); D1 and D6 are landed by the governance slice; D5 remains.

### 6.3 Modify above the freeze line

**M1 — Policy checkpoint in the promotion path** (step 9). *Implemented as a module
boundary.* `spec.Promote` now delegates to `spec.PromoteWithPolicy`, which consults an
injected `spec.PromotionPolicy` before scoring selects a winner: a candidate the policy
refuses is disqualified regardless of score, so a refusal can never be outranked. The
speculation store stays policy-agnostic (the policy is injected, exactly like the Scorer);
governance supplies the gate. `attest.VetoGate` disqualifies any change whose ancestry
carries a veto, and `attest.ApprovalGate{Required}` additionally requires an approval of a
given strength. `varvig spec promote` applies the veto gate by default. The wasm policy
module (§2.5) is a future `PromotionPolicy` implementation that slots into the same hook.

**M2 — Pluggable ordering in the transaction scheduler** (step 7). If ordering is
hardcoded, replace it with a module boundary. Not frozen, but load-bearing code with
concurrency semantics. Budget the most time here and the most testing.

**M3 — Wasm host functions.** A policy module needs to: verify a signature, resolve
principal identity, read notes on a target, query the affected-set index, and read
scheduler state. Additive, feature-bit gated per D5. *Not yet: the wasm policy module
(§2.5) currently receives a host-computed `PolicyInput` on stdin rather than calling back
into the host. Host functions are the refinement that lets a module pull facts live.*

**M4 — Signature verification surfaced as a host capability.** Signing already exists as
structural provenance (§2.1). What is new is exposing verification *to policy modules*
with a principal-resolution path, without letting a module reach outside its sandbox.

### 6.4 Build purely on top

Tickets as unmaterialized change records, intent revision chains, attestations,
scoring, the Jira/GitHub bridge, and every human projection. No core involvement beyond
the primitives already listed.

---

## 7. What to Test

Extend the §4.7 conformance suite, which is itself a content-addressed object and runs
from step 1. New cases below are grouped by what failure they are designed to catch.
Cases already covered are marked.

### 7.1 Object and format

- Unmaterialized change round-trips through an old binary with the discriminator intact
  (D1, D2). *(covered: `TestUnmaterializedChangeRoundTrip`, conformance `change/unmaterialized`)*
- An unmaterialized change is **not** equal to, and does not hash as, a change with an
  empty tree. *(covered: `TestUnmaterializedNotEqualEmptyTree`)*
- Checkout of an unmaterialized change fails with the specific named error, not an empty
  working tree. *(covered: `object.ErrUnmaterialized`, checkout guard in `cmd/varvig`)*
- An old binary encountering a reserved-but-unpopulated namespace neither errors nor
  garbage-collects it (D6). *(covered by construction: a namespace is refs; an empty one
  is no refs — see `internal/reserved` and its tests)*
- Fuzz: arbitrary unknown fields and unknown kinds survive read-modify-write by an old
  binary, byte-identically (§4.4). *(covered: `TestUnknownFieldsRoundTrip`,
  `TestOpaqueUnknownType`)*

### 7.2 Attestation and authority

- **Approval does not survive a spec edit.** Edit intent, assert promotion is refused.
  This is the single most important test in the suite; if it ever passes silently, the
  audit chain is theatre. *(covered: `TestApprovalDoesNotSurviveSpecEdit`)*
- Veto on an ancestor revision blocks promotion of every descendant, including
  descendants created after the veto. *(covered: `TestVetoBlocksDescendants`,
  `attest.PromotionBlocked`)*
- Veto is non-destructive: vetoed speculation is GC-eligible, and the attestation and its
  target survive GC (D4). *(covered: `TestGCRetainsAttestationAndTarget`; the
  vetoed-speculation-is-GC-eligible half arrives with the speculation/promotion slice)*
- A `weak` attestation cannot satisfy a policy requiring `strong`, and no code path
  upgrades strength. *(covered: `TestWeakDoesNotSatisfyStrong`, `Strength.Satisfies`)*
- A bridge key cannot mint a `strong` attestation, under any inbound payload.
  *(covered: `TestBridgeCannotMintStrong`, `attest.VerifyWithPrincipal`)*
- Delegated authority is bounded: an agent acting for a director cannot approve outside
  the delegated scope. *(pending: needs delegation records; strength `delegated` is
  represented but scope-bounding is future work)*
- Signature over a mutated target fails verification (tamper test).
  *(covered: `TestTamperedTargetFailsVerification`)*
- Policy hash recorded at signing time is preserved, and evaluating "was this approved
  under the rules then in force" returns the right answer after a policy change.
  *(partial: the policy hash is stored in the attestation and round-trips; the policy
  module and its evaluation arrive with the policy-checkpoint slice)*

### 7.3 Replication and GC

- Approvals and vetoes propagate on ordinary P2P sync, with no extra flags (D3). *(holds
  by construction: notes are refs, listed and synced like any other)*
- A peer that cannot fetch notes **fails loudly** and refuses promotion rather than
  promoting blind. Test the negative path explicitly. *(sync refuses an incomplete
  closure via `hasClosure`; the promotion-blind negative path is future work)*
- Aggressive GC with thousands of speculation states retains every attestation and every
  attested intent revision (D4). *(note-target pinning holds; the volume test is future
  work)*
- Reflog recovers a ticket after an erroneous ref move, including a bad bulk
  reprioritization.

### 7.4 Scheduler and promotion

- Two tickets with overlapping write sets are serialized; non-overlapping ones run in
  parallel (§1.4).
- Derived blocking matches the affected-set index; no hand-declared links exist anywhere.
- Promotion consults policy **before** scoring, and a policy refusal cannot be outranked
  by a high score (M1). *(covered: `TestPromoteWithPolicyRefusalNotOutranked`,
  `TestPromoteWithPolicyAllRefused`, `TestVetoGateAdmit`, `TestApprovalGateAdmit`)*
- A pluggable scorer swap changes ordering and changes nothing else (M2).
- Deterministic replay: the same ticket set, scorer hash, and policy hash produce the same
  admission order.
- Scorer backtest harness reproduces a historical quarter and reports disagreements
  (§3.3).

### 7.5 Bridge

- Round-trip: varvig → Jira → varvig produces no new intent revision (echo suppression,
  §5.4).
- A derived field edited in Jira is overwritten on next sync; an editable field is not.
- A Jira workflow transition produces exactly a `weak` attestation.
- Bridge outage: edits queue and reconcile without duplicate tickets and without lost
  attestations.
- Concurrent edit on both sides resolves deterministically, with the tracker losing.
- Bridge compromise (adversarial): a malicious bridge cannot promote anything that
  requires `strong`, cannot forge a principal, and cannot delete a veto.

### 7.6 Cross-version interop matrix

Per §4.7, every release passes every prior version's suite. Add:

- New client / old server and old client / new server, with ticket objects present.
- A note-carrying replication path in the matrix, so a version that cannot read a newer
  attestation still round-trips it untouched rather than dropping it.
