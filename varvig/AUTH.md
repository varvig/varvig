# Identity, Auth, and the Read API

This document describes Varvig's identity, authorization, and read-API layer as
implemented. It is the companion to [`DESIGN.md`](./DESIGN.md) (the storage model
and agent-facing concepts); the design notes it realizes are cited as "auth
design §N" throughout.

Everything here lives **above the frozen core**. The object store has no
permission model and no notion of users — it is handed an already-resolved
principal and is indifferent to how that happened. This is deliberate: auth will
be redesigned several times, and the storage format must not move when it does.
No object type, wire frame, or on-disk layout in the frozen core changed to add
any of this (the conformance suite is untouched).

---

## 1. Identity (`internal/sshkey`, `internal/identity`)

**Reuse the user's SSH key.** Users already have `~/.ssh/id_ed25519`; that is
their identity (auth design §2.1). There is no enrollment, no CA, and nothing to
register — **the fingerprint *is* the identity** (§2.2).

- **Ed25519 only.** No negotiation, no alternatives at v1 (§2.1).
- **Resolution order:** ssh-agent (`SSH_AUTH_SOCK`) → `~/.ssh/id_ed25519` read
  directly → `~/.varvig/keys/` fallback. The agent is preferred because the key
  can be hardware-backed and never leaves it; agent forwarding then works for
  free.
- **Fingerprints** are rendered in the standard OpenSSH `SHA256:base64` form —
  what users paste from `ssh-keygen -lf` and `ssh-add -l`.
- **Encrypted on-disk keys** are not decrypted at rest. Varvig still reports the
  identity (from `~/.ssh/id_ed25519.pub`) but defers signing to ssh-agent, which
  is the intended path for a protected key.

The SSH wire formats (public-key parsing, the OpenSSH private-key container, and
the ssh-agent list/sign protocol) are hand-rolled and cgo-free, so this adds no
dependency and no toolchain requirement to the one portable binary (design §3).

```
varvig whoami                    # → jan  SHA256:aXk9Lm4Qr…  (source: ssh-agent)
varvig key init --name jan       # fallback only; refuses to overwrite; 0600
```

---

## 2. The trust store (`internal/trust`)

**The repo is the trust store** (auth design §3): the list of allowed principals
is a versioned file, `.varvig.d/allowed_keys`, changed like any other file.

```
# fingerprint       name    scope        rights
SHA256:aXk9Lm4Qr…   jan     /            promote
SHA256:cW3nEf8Zx…   ci-01   /            propose
SHA256:dK1oIu5Vb…   sam     src/web/     promote
```

- **`scope`** is a path prefix; `/` is the whole repo. Prefix matching respects
  component boundaries — `src/web/` does not cover `src/webapp/`.
- **`rights`** are `read` < `propose` < `promote`; a higher right implies the
  lower ones.
- **Round-trip discipline** (§3.1, design §4.4): comments, blank lines, unknown
  trailing columns, and lines with unrecognized rights are all preserved
  byte-for-byte. A future version may add fields; an old client must never
  silently delete them.
- Onboarding is "append a line and push"; offboarding is deleting the line.
  You cannot authorize yourself — a principal already holding `promote` must
  grant access (§3.2).

The file lives at the tracked path `.varvig.d/allowed_keys` (the Unix `.d`
config-directory convention, on brand with the tool). It is deliberately **not**
the untracked `.varvig/` metadata directory — `write-tree` skips that one by
exact name — so the trust store is versioned and travels with the repository.

```
varvig trust list                # show principals
varvig trust check [scope]       # what may the active identity do here?
```

---

## 3. Signed ref updates (`internal/refupdate`)

The single most important mechanism (auth design §5). **Authority travels with
the change, not the connection:** a ref update is a *signed compare-and-swap
assertion* whose proof lives in its payload, so it can be relayed through peers
nobody trusts and still be verified at its destination (§5.3).

The payload — `ref`, `expected_old`, `new`, `scope`, the signer's public key, a
16-byte `nonce`, `not_after`, and optional `evidence` — is encoded canonically
(minimal varints, sorted unique fields, no trailing bytes), so the exact bytes
that were signed are reconstructible byte-for-byte by any implementation (§5.1).
Field tags below `CriticalMax` are critical (an unknown one is rejected); tags
at or above it are extensions (an unknown one is preserved and, being inside the
signed bytes, cannot be stripped).

> The signer's full Ed25519 **public key** rides in the payload, and its SSH
> fingerprint is derived from it. The design writes `signer: <multihash of
> pubkey>`, but verification needs the key itself, and the trust store is keyed
> by fingerprint — carrying the key means the key that verifies the signature is
> exactly the one the trust store authorizes, with no separate field that can
> disagree.

**Verification pipeline** (§5.2), as implemented:

1. **Signature** over the canonical bytes — verified *first*. An update whose
   signature does not verify is not an authenticated request and is rejected
   without any side effect.
2. **Expiry** (`not_after`) with a small clock-skew tolerance (no NTP required,
   §12).
3. **Authority** — the signer must hold `promote` at a scope covering the
   update's declared scope, per the trust store.
4. **Referenced objects present** locally (the new tip and any evidence).
5. **Replay** — the nonce, keyed by `(signer, ref)`, is consumed; a repeat is
   rejected. Nonces are remembered only until `not_after` passes.
6. **Atomic compare-and-swap** against `expected_old`; on conflict the caller is
   handed the current head to rebase onto and retry.
7. **Audit** — every *authenticated* outcome (accept or reject) is appended to
   the ref's reflog.

> **Order note.** The design lists nonce and trust checks ahead of signature
> verification. This implementation verifies the signature first and defers the
> two side-effecting steps (recording the nonce, the CAS) to the end, so an
> unauthenticated blob never consumes a nonce or writes to the audit log. The
> pure checks are independent, so reordering them is safe.

> **Scope semantics (v1).** Authorization is enforced over the update's declared
> `scope`. Mapping a specific ref name to a path scope is a policy convention
> left to a higher layer; the mechanism enforces the trust-store scope rigorously.

```
varvig promote <ref> <new> [--scope S] [--ttl SECONDS]
```

signs with the active identity and drives the pipeline locally. The lease is the
ref's current value, so a concurrent move is reported as a conflict with the new
head — regeneration-merge retry in embryo (auth design §10.6).

---

## 4. The read API (`internal/readapi`)

**One query layer, two transports** (auth design §7.1). There is exactly one
implementation of "read a tree"; the HTTP server and the CLI plumbing are thin
wrappers over it, so the UI can never show something the CLI does not. **No
client reads the on-disk layout directly** (§7.2) — all access goes through the
object and ref stores, keeping the layout the disposable cache the storage design
promises (design §4.3).

- **Hash-addressed.** Every view names an immutable state, so every page is a
  permalink and infinitely cacheable; branch names resolve to a hash and
  redirect (§7.3).
- **Content negotiation.** The same routes serve JSON (the default — the machine
  API is the primary consumer) and HTML (for browsers).
- **Intent-first.** `/change/{hash}` leads with the change's intent, then its
  provenance evidence, then the diff — deliberately, not cosmetically (§7.3): a
  diff-first view quietly rebuilds GitHub.

```
GET /o/{hash}                 object metadata
GET /tree/{hash}/{path…}      directory listing
GET /blob/{hash}              file content
GET /change/{hash}            intent, then evidence, then diff
GET /log/{ref}?limit=N        change list
GET /refs                     ref → hash
GET /proposals?scope=…        unpromoted speculative states
```

**Local auth** (§7.4): the read-only server binds a Unix socket at mode `0600` —
filesystem permissions *are* the authentication. TCP is an explicit opt-in.
Blobs are served `nosniff` as `application/octet-stream`, and a cross-origin
browser request (an `Origin` whose host is not ours) is refused — the browser-hop
defense of §7.5.

```
varvig serve --read-only [--socket PATH | --tcp ADDR]
varvig read <object|tree|blob|change|log|refs|proposals> [args]   # JSON plumbing
```

---

## 5. Task credentials and the MCP gate (`internal/task`, `internal/mcp`)

Agents are the primary user, so the gate they talk to is built into the **core
binary**, not shipped as a client (auth design §8): a sandbox needs MCP on every
task, and read-logging into provenance is a core write concern. Two pieces.

**Task credentials** (`internal/task`, auth design §6). Per task, an ephemeral
Ed25519 keypair is minted *in the sandbox*; the private key is held only in
memory and never touches disk. It is granted a scope, a propose-only right, and
an expiry:

- **Scope is the read set.** The grant's scope is simultaneously the sparse
  checkout, the gate's visibility, and the capability boundary — one thing
  expressed three ways (design §1.4).
- **Propose-only.** A task key can create objects and propose a speculative
  state; it can never move a ref. A non-propose-only grant is refused at mint.
- **Expiry does the revocation work.** A short TTL means the common case needs
  no revocation infrastructure; an expired grant can do nothing. At v1 the grant
  registry is an in-memory table the local daemon holds (§6.1); the short-lived
  certificate for *remote* propose is additive and changes nothing below it.

```
varvig task start [--scope S] [--ttl DUR] [--base REF] [dir]
```

mints a grant and carves out the scoped sparse checkout of its read set.

**The MCP gate** (`internal/mcp`, auth design §8) is a JSON-RPC 2.0 server over
stdio, bound to one task credential and holding **no authority of its own** — if
it carried a broad credential, every agent that connected would inherit it, and
you would have built a confused deputy. It is in-process and reads through the
query layer (§4) directly, so it does not force the read API to stabilize before
it is ready. Its rules:

- **The capability is the read set.** Every path a tool touches is checked
  against the grant's scope; out-of-scope reads and proposals are refused.
- **Coarse, domain-shaped tools** rather than one wrapper per endpoint, because
  context window is the scarce resource: `fetch_tree`, `fetch_blob`,
  `fetch_change_with_intent`, `fetch_evidence`, `list_proposals`, `propose`.
- **Hashes in every response**, so the agent's reads are pinned and its work is
  reproducible.
- **Reads are logged into provenance.** Every resolved hash is folded into the
  task's read set and written into the `ContextRead` of the provenance a
  proposal produces — audit and provenance become one mechanism (§8.2).
- **Writes are proposals, never promotions.** `propose` overlays file contents
  onto the base tree, signs the change with the ephemeral key, and records it in
  the speculation pool. It never moves a ref; promotion stays a separate,
  human-gated step.

```
varvig mcp [--scope S] [--ttl DUR] [--base REF]      # serve the gate over stdio
```

**The daemon** (`internal/daemon`, auth design §6.1, §7.4) is the long-running
local process that makes `task start` and the gate two halves of one flow. One
daemon per repository keeps the repo open (warm indices, §7.1) and holds the
in-memory grant table. `task start` asks it to mint a grant: the daemon generates
the ephemeral key, records it, and opens a **per-task Unix socket** (0600 — file
permissions are the authentication, §7.4), backed by an **`SO_PEERCRED` check**
(`internal/peercred`) so only the daemon's own uid may connect — kernel-attested,
unforgeable, nothing on the wire to leak. The read-only server's socket carries
the same check; both fall back to the 0600 mode alone on platforms without
`SO_PEERCRED`. The key then lives only in the
daemon's memory for the task's life — never on disk, never on the wire — and is
used there to sign the task's proposals. A background reaper prunes expired
grants and closes their sockets; expiry is the revocation mechanism (§6.2), so
`task stop` (early revocation) is a convenience, not a requirement.

The gate speaks JSON-RPC over any stream, so the daemon serves the same gate
over each connection. **`varvig mcp` is the stdio entry point a harness spawns**,
and by default it *relays through the daemon*: it asks the daemon to mint an
ephemeral task, bridges stdio to that per-task socket (drain-correct, so the
final replies are never truncated), and stops the task when the client
disconnects. The credential and the warm repo stay in the daemon; the spawned
process is a thin relay. `mcp --connect` bridges to a specific socket, and `mcp
--standalone` forces an in-process gate when no daemon is wanted.

```
varvig daemon [--socket PATH]                        # run the local daemon
varvig daemon status                                 # pid, uptime, live task count
varvig daemon stop                                   # ask it to exit
varvig task start --scope S --ttl DUR [dir]          # mint (in the daemon, if up)
varvig task list                                     # the daemon's live tasks
varvig task stop <id>                                # revoke early
varvig mcp --scope S --ttl DUR                       # stdio gate; relays via daemon if up
varvig mcp --connect <task.sock>                     # bridge to a specific task socket
```

Without a daemon, `task start` still produces the scoped sparse checkout and
`varvig mcp` serves a standalone gate that mints its own key — the same
capability model, just without a shared table across processes. Sockets live
under a short per-uid runtime dir (the `sun_path` length cap rules out a deep
`.varvig/` path), keyed by a hash of the repo root so daemon and client agree.

> **Residual risk (auth design §8.3).** Repository content reaching an agent is
> untrusted input: a comment in a source file can attempt to redirect the
> agent's behavior. Scoping limits the blast radius; it does not eliminate the
> class. This is the argument for keeping read sets narrow and for making
> promotion genuinely independent of the agent that proposed the change.

---

## 6. Deferred, with the upgrade path open

None of these require changes below them; each is added only when its triggering
condition occurs (auth design §11):

| Deferred | Add when |
|---|---|
| Signed `allowed_keys` | Untrusted peers relay the repo |
| Short-lived certificates for task keys | Agents propose to *remote* peers |
| `getpeereid` peer-uid attestation on macOS/BSD | `SO_PEERCRED` is implemented for Linux (`internal/peercred`, cgo-free via the stdlib); the `getpeereid`/`LOCAL_PEERCRED` equivalent for other Unixes is still to come — they fall back to the 0600 mode until then |
| DNS-rebinding token → HttpOnly cookie | A client serves HTML to a browser over TCP |
| OS-keychain encryption of the fallback key at rest | Platform integration is available |
| Signed ref updates over the wire protocol | Remote promotion is wired (local promote is implemented) |
| Multi-root quorum, OIDC, revocation lists | The corresponding threat becomes live (§11) |

---

## 7. Known gaps (auth design §12)

- **TOFU peer pinning** is genuine trust-on-first-use, not verification.
- **Prompt injection via repo content** is not solved by scoping.
- **Offboarding lag**: a departed key stays valid on peers that have not yet
  fetched the updated `allowed_keys`, bounded by fetch frequency.
- **Clock skew** is tolerated by a small window on `not_after`; NTP is not
  required.
- **The read API is explicitly unstable at v1.** Declaring a contract you are
  not yet testing is worse than churning openly.
