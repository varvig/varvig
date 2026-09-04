# The Governance Plane

Design Notes VI. This note asks a single question: how does an *authorized
principal* — a human keyholder, or an automated system holding explicitly
delegated authority — exercise governance (attest, promote) programmatically,
and specifically whether that should happen over MCP, **without weakening the
agent gate**.

It is the companion to [`TICKETS.md`](./TICKETS.md) (the authority model —
attestations, strength, policy, cited as "tickets §N"), [`MCP.md`](./MCP.md)
(the agent gate, "MCP §N"), and [`AUTH.md`](./AUTH.md) (identity and the
confused-deputy warning, "auth §N").

**Status: proposal.** Nothing here is implemented. This note frames a decision
that touches a trust boundary, so it should be settled deliberately before any
code. The recommendation (§9) is to build the smallest useful version, disabled
by default, and to prefer policy over a plane wherever the automation is
deterministic.

---

## 1. The problem

The agent gate is deliberately powerless. Through `varvig mcp` an agent can read
within its scope and **propose** signed, speculative changes — and nothing more.
There is no promotion tool and no governance tool, and that absence is a hard,
loudly-tested invariant (MCP §3, §4.2): "varvig has no destructive agent-facing
tool at all."

Governance is the other half of the system: the signed decisions that let work
be promoted — `approve` / `veto` / `request-change`, and the promotion policy
(tickets §2). Today these are exercised only by a keyholder at the CLI
(`varvig attest`, `varvig promote`), signing with their own identity.

The question is whether an **authorized non-human principal** — a delegated
reviewer or automation — should be able to exercise governance
*programmatically*, and if so, how to give it a surface without folding any
authority into the agent gate.

The barrier was never the transport. MCP could carry an `attest` tool trivially.
The barrier is **authority**: `attest.Sign` needs a signer whose principal *kind*
permits the strength it issues (`attest.CheckStrengthKind`, tickets §2.4). The
agent gate runs as an ephemeral, propose-only task key — the wrong kind of key —
so an `attest` tool bolted onto it would either be inert or would require handing
the gate a governance key, which recreates exactly the confused deputy the design
exists to prevent (auth §8).

## 2. Non-negotiables

Any governance plane must preserve every one of these. They are the reason the
plane is worth designing carefully rather than reaching for.

1. **Every governance act is a signed object bound to a specific revision hash.**
   An approval binds to the intent revision it approved and does not carry
   forward across a revision (tickets §2.1, §2.2). The plane is transport; it
   never invents authority, and it never mutates a decision after the fact.
2. **Every act is attributable to a principal** whose kind and permitted strength
   are checked (tickets §1.4, §2.4). "Who signed this, and were they allowed to?"
   is always answerable from the object alone.
3. **The signing key is held by the principal, never by the model.** Backed by
   ssh-agent (auth §1), the key never enters an LLM's context; the plane asks the
   agent to sign, it does not carry the secret.
4. **The agent gate is untouched.** Still the read/propose surface, still
   propose-only, still no promotion tool, still asserted in CI (MCP §9). A
   governance plane is a *different consumer*, never a mode of the agent gate.
5. **Policy remains the adjudicator.** The wasm promotion policy and the built-in
   gates (`VetoGate`, `ApprovalGate`, tickets §2.5) decide what suffices to
   promote. A governance plane *submits* attestations and promotion requests; it
   can never bypass a veto or a policy refusal. A veto always wins (tickets §2.3).

## 3. Two kinds of "automated governance" — separate them first

Most of the demand for "let automation govern" is not a plane problem at all.

- **Deterministic governance** — "approve when CI is green, coverage ≥ X, and
  provenance verifies." This is a *rule*, and rules belong in the **wasm
  promotion policy** (tickets §2.5): content-addressed, versioned alongside the
  code it guards, evaluated at the promotion checkpoint with no model in the
  loop. It is already the right home, and it needs no new surface. Prefer it.
- **Judgment governance** — a reviewer that reads a change and exercises
  discretion to approve or request changes, holding *delegated* authority. This
  is the only case that needs a plane, and standing one up is a deliberate
  decision that a model may exercise delegated authority. Do not reach for it
  when a policy rule would do.

The first question for any request to "expose governance" is therefore: *is this
a rule or a judgment?* A rule is a policy module. Only a judgment justifies a
plane.

## 4. The plane

If a judgment plane is warranted, it is a **distinct MCP surface** — a separate
subcommand, `varvig govern-mcp`, not a flag on `varvig mcp` — bound to a
governance principal and speaking a small, governance-shaped tool surface:

| Tool | Purpose | readOnly |
|---|---|---|
| `govern_read` | List the attestations and derived governance status for a change or ticket, and show the active policy | ✓ |
| `govern_attest` | `approve` / `veto` / `request-change` against a specific revision hash, at a strength the principal's kind permits | ✗ |
| `govern_promote` *(optional)* | Request promotion of a change; the policy adjudicates and may refuse | ✗ |

Shape rules, mirroring the agent gate's discipline:

- **Bound to a signer.** The server runs as a governance principal
  (`identity.Signer`, ssh-agent backed). `govern_attest` calls `attest.Sign` with
  that identity; `attest.CheckStrengthKind` rejects a strength the principal's
  kind may not issue. There is no way to sign as anyone else.
- **Every write returns the signed object's hash** (the attestation, or the
  promotion outcome), so the act is immediately pinnable and auditable — the same
  "every response names a hash" discipline as the agent gate (MCP §4.1).
- **`govern_promote` never bypasses policy.** It submits; `VetoGate` /
  `ApprovalGate` / the wasm policy decide. A refusal is a normal, coded result,
  not an error to route around.
- **Reads are provenance too** (MCP §5): the attestations and changes the
  reviewer read are recorded, so "what did this decision see?" is answerable.

Whether `govern_promote` belongs on the plane at all is an open question (§8):
attesting is a discrete signed opinion; promotion is a state move, and there is a
good argument for keeping promotion CLI-only even when attestation is delegated.

## 5. Why a separate surface, not a capability flag on the agent gate

MCP §1 mandates that *all agent distribution channels expose an identical tool
surface* and, where a channel seems to need its own tool, "add it to the shared
surface behind a capability flag." That escape hatch is real, and it tempts a
capability-flagged `govern_*` set on the agent gate, advertised only when a
governance principal is present.

Reject it, for two reasons:

1. **It muddies the differentiator.** "No destructive, no promotion tool, ever"
   is a one-line promise the CI exact-set assertion protects (MCP §9). A
   capability-gated promotion tool forces that assertion to become mode-aware and
   turns a clean invariant into "it depends."
2. **It blurs the two acts.** The whole point (auth §8, MCP §2.1) is that "an
   agent proposed this" and "a principal authorized this" stay distinct even when
   the same operator is behind both. A different surface for a different authority
   keeps that legible. varvig already carves consumers this way — the read API for
   varvig-ui is a separate surface, not a channel of the gate (MCP §1). The
   governance plane is the same kind of separate consumer.

## 6. Principals and delegation

A delegated automation is a **principal** (tickets §1.4) of a *kind* that policy
grants a bounded authority. The strength model already expresses the bound:
`weak < delegated < strong` (tickets §2.4), with `CheckStrengthKind` enforcing
which kinds may issue which strengths. A reviewer bot might be permitted to issue
`delegated` approvals over some scope but never `strong`, and never a veto
override; a human keyholder retains `strong` and the last word (a veto always
wins, tickets §2.3).

Delegation itself is expressed and signed the same way everything else is — there
is no ambient "the bot is trusted" flag. The plane authenticates *which*
principal it is acting as and does no more than that principal's kind allows. It
is a courier for a bounded, signed authority, not a source of authority.

## 7. Guardrails — what must not change

- The **agent gate** stays exactly the read/propose surface: propose-only, no
  promotion tool, the exact-set and no-`*promote*` CI assertions intact (MCP §9).
- The **frozen core** does not move (design §4); the attestation object format is
  unchanged (FORMAT, tickets §2).
- **Policy is still the adjudicator.** The plane cannot promote past a veto or a
  policy refusal. Human veto authority is never delegated away.
- The plane ships **disabled by default**. It does nothing until an operator
  configures a governance principal and a policy that grants it authority.

## 8. Open questions and risks

- **Model-as-approver.** Giving a model delegated authority invites
  rubber-stamping and prompt-injection-into-the-reviewer: content in a change or
  ticket trying to talk the reviewer into approving. Mitigations are structural,
  not vibes — bounded strength, tightly scoped delegation, policy still gates,
  and a human veto that always wins — but the residual risk is real and is the
  reason this is opt-in and narrow.
- **Should `promote` be on the plane at all?** Attestation is a signed opinion;
  promotion is a state transition. Keeping promotion CLI-only, even when
  attestation is delegated, is a defensible smaller starting point.
- **Key custody.** A delegated principal's key is now an automation credential.
  ssh-agent keeps it out of the model's context, but its lifecycle, scope, and
  revocation are an operational surface that did not exist before.
- **Auditability** is already handled — every act is a signed, attributable,
  hash-addressed object — which is precisely what makes even a risky delegation
  *observable* after the fact.

## 9. Recommendation and build order

1. **Default to policy, not a plane.** Route every "automate this governance"
   request through the wasm promotion policy (tickets §2.5) first; only a genuine
   judgment call justifies a plane.
2. If a judgment plane is adopted, build the **minimum**: `govern_read` +
   `govern_attest` on a separate `varvig govern-mcp`, bound to a governance
   principal, **disabled until configured**.
3. **Keep `promote` CLI-only** to start; revisit `govern_promote` only after the
   attestation plane has been exercised.
4. Express the delegated principal's authority in the **existing** kind/strength
   model (tickets §1.4, §2.4) — no new authority primitive.
5. **Never touch the agent gate.** Its invariants are the load-bearing promise;
   the governance plane succeeds only if that promise is still true afterward.
