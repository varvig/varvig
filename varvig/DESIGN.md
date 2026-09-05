# Design Notes: A Source Control System for Agents

A version control system designed for AI agents working in parallel, not for humans
working sequentially. Humans remain a supported consumer, but they are a rendering
target rather than the primary user.

---

## 0. Summary

Git's model assumes a small number of slow, serial actors who write prose about their
changes and resolve conflicts by hand. Agents invert every one of those assumptions:
there are many of them, they are fast, their intent is machine-readable, and
recomputing a change is often cheaper than merging one.

The design below keeps Git's actual load-bearing invention — an immutable,
content-addressed Merkle DAG — and rebuilds the layer above it around intent,
verification, and concurrency.

Three constraints shape everything:

1. **A minimal frozen core, forever compatible.** Small enough to stop changing almost
   immediately.
2. **A single portable binary.** Install is copy-a-file. No runtime, no package manager.
3. **All volatility lives above the core**, as content-addressed modules and derived
   indices that old clients can ignore without corrupting anything.

---

## 1. Core Model

### 1.1 Intent is the primary object; code is a materialization

In Git, the diff is ground truth and the commit message is a lossy human comment about
it. Invert this.

A change record stores:

- the task specification the agent was given
- the context it was provided (and what it actually read)
- the reasoning or plan it produced
- the model, model version, sampling parameters, and tool permissions in effect
- the resulting tree

The resulting tree is still content-addressed and still authoritative for checkout — but
it is understood as a *cached materialization of an intent*, and the intent is what gets
reused when the world changes underneath it.

### 1.2 Merge by regeneration, not reconciliation

When two changes conflict, do not diff3 the text. Hand the losing change its original
intent plus the new base, and re-run it.

Rerunning an agent is frequently cheaper than a careful textual merge, and it produces a
change that is actually correct against the new base rather than one that is merely
textually plausible. Conflict resolution becomes recomputation.

Textual merge remains available as a fast path and a fallback.

### 1.3 Conflict is semantic, not positional

Two agents editing adjacent lines with compatible intent is not a conflict. Two agents
editing different files in ways that jointly break an invariant is.

This requires an incremental build / type / test graph fused into the VCS itself,
content-addressed so it can answer *"what does this change actually affect"* in
milliseconds. A commit is not valid until it carries its evidence: what was checked,
against what, with what result.

### 1.4 Transactions, not optimistic branching

Git assumes conflicts are rare. With hundreds of parallel agents they are the common
case. Use a database-like model instead:

- agents declare **read sets and write sets** ahead of time
  (e.g. "I am touching the auth module's public interface")
- a scheduler serializes genuinely conflicting work and parallelizes the rest
- failed transactions retry automatically rather than surfacing to a human

The declared write set is the capability boundary — a proposal is held to it at
proposal time. The declared read set is a *dependency hint*: validated against
what the agent actually read and used for optimistic-concurrency conflict
detection, not to shape the checkout. (As built, the checkout-scope addendum
settled this as "full checkouts as default": a task checkout is the whole base
tree, an ordinary repository, not a sparse slice of the read set. See AUTH §5.)

### 1.5 Speculation is the default unit of work

No branch names, no ceremony. Thousands of ephemeral attempt-states, content-addressed,
plus a selection mechanism that scores them against an objective and promotes winners.

Branching stops being a coordination ritual and becomes search.

Consequence: **retention policy is a core design question, not a background cleanup
job.** Garbage collection must be designed in from the start, not bolted on.

### 1.6 History is a queryable graph with zoom levels

Agents do not scan `git log`; they need retrieval. Two requirements:

- **Semantic blame**: which *task* caused this behavior to change, not which line moved.
- **Multi-resolution history**: raw operations at the bottom, summarized intent at the
  top, like mipmaps — because context windows are finite and the repo should be able to
  serve history at whatever resolution fits.

### 1.7 Humans are a rendering target

Linear tidy history, readable commit messages, and reviewable diffs are *projections
generated on demand* for human review. They are not the storage format, and the storage
format is not obligated to be pretty.

---

## 2. What to Keep From Git

Git's genuinely load-bearing property is one thing: **immutable content-addressed
objects in a Merkle DAG.** That single property is what delivers exact version pinning,
verifiable peer-to-peer sync with no central authority, and stable identities for
external tooling to attach to. Everything above can be layered on it.

Also worth preserving:

| Property | Why it survives the transition |
|---|---|
| **Exact version pinning** | A hash names a state unambiguously. Rollout, rollback, and deploy pins all depend on it. |
| **First-class peer-to-peer** | Most people run a central server; nothing should *require* one. Any peer is a full replica. |
| **External triggers / hooks** | CI, deployment, and automation hang off ref changes. Must be first-class, not an afterthought. |
| **Bisect** | Perfectly suited to agents: fully automated regression search with no human in the loop. Generalizes from binary search on a line to guided search over the speculation DAG, using the affected-set index. |
| **Reflog — nothing is ever really deleted** | Git's best safety property. With autonomous agents doing destructive operations at machine speed, cheap universal undo *is* the containment mechanism. Append-only log of every ref move; recovery always possible. |
| **Atomic compare-and-swap on refs** | The real concurrency primitive underneath `push --force-with-lease`. All of §1.4 ultimately reduces to this. |
| **Full local replica, full offline operation** | An agent in a network-isolated sandbox must be able to do the entire workflow. Also makes forking whole repo states cheap, which speculation depends on. |
| **Partial clone / sparse checkout** | Originally promoted to central mechanism ("a checkout is exactly the declared read set"). The checkout-scope addendum reversed this: a task checkout is a **full** ordinary repo (the whole base tree), because a sparse checkout that hides paths recreates the missing-vs-deleted ambiguity and needs eight cooperating mechanisms to be safe. Confinement is now the *write set*, enforced at proposal; the read set is an observed, validated dependency hint. Sparse is retained only as an explicit opt-in. See AUTH §5. |
| **Cross-repo composition by hash** | Submodules are miserable, but pinning a dependency to an exact immutable version is what makes reproducibility hold across repo boundaries. Keep the primitive, redesign the ergonomics. |
| **Notes: attach data to an immutable object without changing its hash** | Test results, review verdicts, deploy outcomes, incident links accrete onto a commit over time. Git's implementation is obscure; the primitive is exactly right. |
| **Format neutrality** | Git is a data structure, not a policy engine. Permissions, review rules, and branch protection layer above. Preserve this or you bake 2026's agent-orchestration assumptions into the storage format. |
| **Plain working tree on a real filesystem** | Plus a lossless bidirectional export to Git. Non-negotiable for adoption: anything that requires a special client to read is dead on arrival. |

### 2.1 Signed provenance, promoted to load-bearing

Git bolted signing on. Here it is structural. Every object carries who or what produced
it: model and version, sampling parameters, tool permissions, whose authority it acted
under, what it read, and the hash of the tool binary itself.

When most commits come from non-humans, the audit chain is the only basis on which a
deploy can be trusted.

### 2.2 What to drop

- **The staging area.** An interaction affordance for humans deciding what to include in
  a commit. Meaningless when the agent already declared its write set.
- **The assumption that GC is a detail.** See §1.5.

---

## 3. Packaging: One Portable Binary

The whole system compiles to a single self-contained executable per platform. Install is
download-or-copy. Size is irrelevant; storage is cheap.

This matters more for agents than for humans. A human installs once a year. An agent
provisions a fresh sandbox every task — install cost is paid thousands of times a day,
and every package manager in the path is a source of version skew between agents
supposed to be collaborating on one repo.

Beyond convenience: **the binary's own hash becomes part of provenance.** Reproducibility
then covers the tooling, not just the code — which only works if the tool is one
immutable artifact rather than a dynamic composition of whatever happened to be on the
box.

### 3.1 Design consequences

- **Busybox-style multicall.** The same file is client, server, CI runner,
  merge/regeneration driver, and index daemon. This reinforces P2P: there is no separate
  "server product," only a peer with an open port.
- **Self-distributing.** The binary serves other platforms' binaries over the same
  content-addressed protocol it uses for repo data. A peer bootstraps from a peer,
  hash-verified, with no registry in the loop.
- **Static linking has two classic leaks:** glibc's NSS/DNS resolution, and TLS root
  certificates. Use a native resolver and embed a CA bundle with opt-in to the system
  store.
- **No `dlopen` plugin ABI, ever.** That is precisely how single-binary tools stop being
  single-binary.

"One binary for every platform" realistically means one artifact per `(os, arch)` with
identical behavior and a byte-identical on-disk format — plus a universal macOS build and
probably a wasm build for browser and embedded use.

### 3.2 Hooks and triggers must be portable too

Git's real portability failure is not the `git` binary; it is that every hook is a bash
script assuming Python and curl exist on the machine.

Embed a small sandboxed **wasm runtime**. Hook, policy, and trigger logic are wasm
modules that are themselves content-addressed objects in the repo. Triggers become
portable, sandboxed, and versioned alongside the code they guard.

### 3.3 Deliberately outside the binary

- **Model inference.** Out-of-process behind a stable interface. Otherwise you ship a
  runtime that dwarfs everything else and ages in months.
- **Language-specific semantic analyzers.** Wasm modules, fetched and cached, so the
  affected-set index can learn new languages without a recompile.

---

## 4. Compatibility and Stability

Every version must interoperate with every other version, in both directions. But this
splits into three surfaces with genuinely different requirements — conflating them is
the usual mistake.

### 4.1 The object format is forever

Hashes end up in signatures, deploy pins, and other repositories' dependency records.
Once written, they must be readable in thirty years.

Freeze hard, keep tiny: object encoding, DAG semantics, ref compare-and-swap. Nothing
else belongs here.

### 4.2 The wire protocol negotiates

Capability flags intersected at handshake, with the frozen core as the always-available
fallback. **Never a version number with implied semantics** — feature bits, so that an
old peer and a new peer always find a working subset rather than failing on a comparison.

### 4.3 The on-disk layout is a cache and may churn freely

Indices, packfiles, the affected-set graph: all derivable from objects. Rewrite them on
upgrade at will. No identity depends on them.

### 4.4 The rule that actually bites: unknown fields round-trip

The dangerous failure is not an old client refusing to read something new. It is an old
client reading an object, silently discarding the fields it does not understand, and
writing it back. That is how provenance and signatures get destroyed.

**Preserve-or-refuse. Never silently degrade.**

### 4.5 Hash agility is the one thing you cannot retrofit

Freezing the digest means betting on it forever. Git's SHA-1 → SHA-256 migration has
dragged on for over a decade precisely because the hash was assumed rather than declared.

Use self-describing multihash digests from day one, plus a defined dual-hash transition
with translation tables. You will probably never need it. Design it anyway, because it
cannot be added later.

### 4.6 On "feature-complete soon"

Half right. Git's core has been effectively frozen for years and that is a real success —
but the reason it *could* freeze is that the core is small and everything interesting
happens above it.

In this design, the agent-facing parts — regeneration drivers, semantic affected-set
analysis, speculation scoring — are coupled to models that turn over every few months.
Freeze those and they will be wrong within a year.

So the path to stability is: an aggressively minimal core, frozen **immediately**, with
all volatile behavior living in notes, wasm modules, and derived indices that old
binaries can ignore without corrupting anything.

### 4.7 Compatibility that isn't tested is just a promise

Ship a **conformance suite that is itself a content-addressed object in the repo**. Every
release must pass every prior version's suite, and CI runs the full cross-version interop
matrix — old client against new server, new client against old server, and round-trip
preservation of unknown fields.

---

## 5. Known Risks

Stated plainly, because they are structural rather than incidental.

- **Merge-by-verification is only as good as the test suite.** This design quietly makes
  tests the real source of truth, and therefore invites Goodharting against them. Needs
  explicit countermeasures: mutation testing, tests that are themselves reviewed
  artifacts, and provenance on test changes.
- **"Intent is primary" only holds if regeneration is faithful.** With non-deterministic
  models it drifts. Hence pinning model version, sampling parameters, and full context in
  the change object — and accepting that the repository is partly an experiment log.
- **Speculation volume makes retention a first-order problem.** Thousands of ephemeral
  states per task will bury a naive store.
- **Semantic conflict detection is language-specific and incomplete.** It must degrade
  gracefully to textual conflict detection rather than claiming safety it cannot deliver.

---

## 6. Build Order

1. Content-addressed object store, Merkle DAG, ref CAS, reflog. Multihash from day one.
2. Single-binary packaging, multicall dispatch, plain working tree, lossless Git export.
3. P2P sync with capability-negotiated wire protocol.
4. Provenance and signing as required fields on change objects.
5. Notes / attachable metadata; wasm hook and trigger runtime.
6. Affected-set index (start textual + build-graph; add semantic analyzers as wasm later).
7. Read/write-set declaration and the transaction scheduler.
8. Regeneration-based merge driver.
9. Speculation store, scoring, promotion, and retention policy.
10. Conformance suite and cross-version interop matrix — running from step 1, not step 10.
