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
is a versioned file, `.vcs/allowed_keys`, changed like any other file.

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

The file lives at the tracked path `.vcs/allowed_keys`, deliberately **outside**
the untracked `.varvig/` metadata directory (which `write-tree` skips), so the
trust store is versioned and travels with the repository.

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

## 5. Deferred, with the upgrade path open

None of these require changes below them; each is added only when its triggering
condition occurs (auth design §11):

| Deferred | Add when |
|---|---|
| Signed `allowed_keys` | Untrusted peers relay the repo |
| Short-lived certificates for task keys | Agents propose to *remote* peers |
| `SO_PEERCRED` / `getpeereid` peer-uid attestation | Beyond the 0600 socket, kept off now to stay cgo-free/cross-platform |
| DNS-rebinding token → HttpOnly cookie | A client serves HTML to a browser over TCP |
| OS-keychain encryption of the fallback key at rest | Platform integration is available |
| Signed ref updates over the wire protocol | Remote promotion is wired (local promote is implemented) |
| Multi-root quorum, OIDC, revocation lists | The corresponding threat becomes live (§11) |

---

## 6. Known gaps (auth design §12)

- **TOFU peer pinning** is genuine trust-on-first-use, not verification.
- **Prompt injection via repo content** is not solved by scoping.
- **Offboarding lag**: a departed key stays valid on peers that have not yet
  fetched the updated `allowed_keys`, bounded by fetch frequency.
- **Clock skew** is tolerated by a small window on `not_after`; NTP is not
  required.
- **The read API is explicitly unstable at v1.** Declaring a contract you are
  not yet testing is worse than churning openly.
