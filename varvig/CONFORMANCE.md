# Conformance and cross-version interop

Compatibility that isn't tested is just a promise (design §4.7). Varvig ships a
conformance suite that is **itself a content-addressed object in the repo**, and
every build must satisfy it.

## The suite

`internal/conformance/vectors.json` is the golden artifact. Its multihash is the
suite's identity:

```
1e20759d4f9731ad6cf1dc4db6d6934fb6c90d9bc748e76ff329b076821accdbf220
```

It pins the frozen surfaces (design §4.1):

- **Object encodings** — canonical VVG1 bytes and blake3 identities for blob,
  tree, change (minimal and signed-with-provenance), provenance, note, and
  hook-config objects.
- **Multihash framing** — `(algorithm, input) → multihash` for blake3 and
  sha2-256, including the empty input.
- **Wire frame format** — the handshake byte string and a `Refs` frame (the
  frozen framing; negotiated capabilities are not frozen, §4.2).
- **Unknown-field round-trip** (§4.4) — a known object carrying an unrecognized
  field, and an object of an entirely unknown type, each of which must decode
  and re-encode **byte-for-byte** with a stable identity.

## Running it

```sh
varvig conform          # verify this build against the golden suite
varvig conform --id     # print this build's suite identity
varvig conform --emit   # print this build's vectors (canonical JSON)
```

`go test ./internal/conformance/` runs the same checks: `TestConformance`
(current code satisfies the golden suite), `TestSuiteIDStable` (the golden
artifact's identity is unchanged), and `TestWriteGolden` (regeneration, gated by
`VARVIG_WRITE_GOLDEN=1`).

## Changing the format

The object format is frozen. A change that alters any golden vector is a
compatibility break and fails `TestConformance` loudly. Legitimate evolution is
**additive** — new object types, new field tags, new capabilities — which by
construction leaves existing vectors untouched (old fields keep their bytes;
unknown fields round-trip). When a release genuinely adds a vector, regenerate:

```sh
VARVIG_WRITE_GOLDEN=1 go test ./internal/conformance/ -run TestWriteGolden
```

and update `GoldenSuiteID`. Removing or mutating an existing vector is not
allowed.

## The cross-version matrix

The matrix (design §4.7) asserts that **every conformance-bearing version agrees
on the frozen format**: old client against new server, new client against old
server, and unknown-field preservation in both directions. The mechanism:

1. Each release's binary must pass `varvig conform` against the golden suite.
2. Two releases interoperate iff their `varvig conform --emit` outputs agree on
   every shared vector (a newer release may *add* vectors; it may never change
   an existing one — enforced by rule above).

CI's `interop` job runs this: it builds the current binary and every tagged
release still present in history, runs each one's `conform`, and compares suite
identities. Tags that predate the suite are skipped, so the matrix grows as
releases accrue rather than failing on their absence. Because the frozen rules
forbid mutating a vector, a passing matrix is a proof — not a promise — that a
thirty-year-old identity is still readable.

Cross-*implementation* interop is covered separately: the Git bridge
(`internal/gitport`) proves varvig↔git round-trips reproduce byte-identical git
objects, verified against the real `git` binary in CI.
