# Building `varvig/plugins` — instructions for the next session

You are picking up the **plugins-repo half** of the release automation described
in *Design Notes IV: Release Automation*. The **core half already exists** in
`varvig/varvig` (see `varvig/RELEASE.md`): the release workflow, the build
tooling, the MCPB manifest source, `varvig --version`, and the annotated MCP
tools are all built and tested. Your job is the repo that holds the pin and the
plugin.

This document is the **contract** between the two repos plus a build order. Where
it says *contract*, the value is fixed by what core already emits — matching it
is not optional, or the two halves won't fit.

---

## 0. The one job

Keep the binary the marketplace serves in sync with what `varvig/varvig`
releases. It fails **silently** — nothing errors, users just get a stale binary
— so treat every piece here as release-blocking infrastructure. Read §10 (the
failure-mode table) of the design before you start; each row is a thing you are
building a defense for.

---

## 1. Repo facts

- `varvig/plugins` **must be public** (Claude Code fetches marketplaces straight
  from the git host, regardless of whether `varvig/varvig` is private).
- **Text only — no binaries.** No `.mcpb` bundles, no executables, ever. Bundles
  are core's release assets; committing them would put binary blobs in a git
  history every user clones (§6).
- The marketplace name is `varvig-tools`.

Target layout (§2):

```
varvig/plugins
├── varvig.lock.json              ← THE pin. Nothing else is authoritative.
├── .claude-plugin/
│   └── marketplace.json          ← generated
├── plugins/varvig/
│   ├── .claude-plugin/plugin.json  ← generated
│   ├── .mcp.json                   ← generated
│   └── README.md
└── tools/
    ├── generate.sh               ← lock → derived files
    ├── smoke.sh                  ← downloads pinned binary, verifies, deletes it
    ├── fetch-binary.sh           ← download one release asset + checksum-verify
    └── mcp-smoke.py              ← copy from varvig/varvig/tools/mcp-smoke.py
```

Note: `mcpb/` and `manifest.json` do **not** live here — the MCPB manifest and
bundles are built in core (§6.1). §5.1 step 5 mentions regenerating a MCPB
manifest; §6.1 is the explicit correction — follow §6.1.

---

## 2. The cross-repo contract (fixed by core — match exactly)

### 2.1 The dispatch

Core's release workflow, after publishing, sends:

```
POST repos/varvig/plugins/dispatches
{ "event_type": "core-release",
  "client_payload": { "version": "v0.3.1" } }   # tag WITH leading "v"
```

So your `bump-core` workflow triggers on `repository_dispatch: types: [core-release]`
and reads the tag from `github.event.client_payload.version`. **The payload
carries only the version.** Never read a hash from it (§5.2) — anyone who can
fire a dispatch could otherwise pin an arbitrary binary.

### 2.2 Release asset names (exact)

For tag `v0.3.1`, `github.com/varvig/varvig/releases/download/v0.3.1/` holds:

| Platform key (lock) | Binary asset | Bundle asset |
|---|---|---|
| `linux-amd64` | `varvig-linux-amd64` | `varvig-linux-amd64.mcpb` |
| `linux-arm64` | `varvig-linux-arm64` | `varvig-linux-arm64.mcpb` |
| `darwin-universal` | `varvig-darwin-universal` | `varvig-darwin-universal.mcpb` |
| `windows-amd64` | `varvig-windows-amd64.exe` | `varvig-windows-amd64.mcpb` |

Plus `SHA256SUMS` — the output of `sha256sum varvig-*` run in the asset
directory, so each line is `<hex>␠␠varvig-<name>` (bare filenames, including the
`.mcpb` bundles). Note the Windows **binary** carries a `.exe` suffix; its lock
key does not.

### 2.3 Provenance

Every binary and bundle is attested with `actions/attest-build-provenance`.
Verify in your bump job:

```bash
gh attestation verify varvig-linux-amd64 --repo varvig/varvig
```

### 2.4 The binary's own surface (already implemented in core)

- `varvig --version` prints `varvig v0.3.1 <os>/<arch>`. Smoke-assert with
  `varvig --version | grep -q "${VERSION#v}"`.
- `varvig mcp` speaks the MCP gate over stdio (newline-delimited JSON-RPC). Every
  tool already carries a `title` and `readOnlyHint`/`destructiveHint`. Reuse
  core's `tools/mcp-smoke.py` verbatim to assert it (§7); do not reimplement.

### 2.5 Cross-repo auth (core's expectations)

Core mints an installation token for a **GitHub App** and dispatches as that App.
You need to (§3, §11 step 4):

1. Create a `varvig-release-bot` App; install it on `varvig/plugins` with
   **Contents: write** and **Pull requests: write**.
2. In `varvig/varvig` settings, set repo **variable** `RELEASE_BOT_APP_ID` and
   repo **secret** `RELEASE_BOT_KEY` (the App's private key). Their presence also
   flips core's `bump-marketplace` job on (it is gated until then, §11 step 7).
3. Optional: repo variable `PLUGINS_REPO` in core if the plugins repo is not
   `varvig/plugins`.

`GITHUB_TOKEN` cannot write across repos — do not try to use it for the dispatch
or the bump (§3).

---

## 3. `varvig.lock.json` — the single source of truth (§2.1)

```json
{
  "version": "v0.3.1",
  "released_at": "2026-08-09T10:14:02Z",
  "source_repo": "varvig/varvig",
  "artifacts": {
    "linux-amd64":      { "sha256": "…", "url": "https://github.com/varvig/varvig/releases/download/v0.3.1/varvig-linux-amd64" },
    "linux-arm64":      { "sha256": "…", "url": "…/varvig-linux-arm64" },
    "darwin-universal": { "sha256": "…", "url": "…/varvig-darwin-universal" },
    "windows-amd64":    { "sha256": "…", "url": "…/varvig-windows-amd64.exe" }
  }
}
```

The hashes here are **derived by your bump job from the downloaded artifacts**,
cross-checked against `SHA256SUMS`. The version is a *pointer*; the hash is
always *computed from the artifact itself* (§5.2). Nothing else in the repo is
allowed to be an authoritative copy of the pin.

---

## 4. `tools/generate.sh` — lock → derived files (§2.2)

`generate.sh` reads `varvig.lock.json` and writes the derived files
deterministically. Then the files are **checked into git**, and CI regenerates
and fails on any diff — the same discipline as a formatting check:

```bash
tools/generate.sh
git diff --exit-code || { echo "generated files stale — run tools/generate.sh"; exit 1; }
```

This makes a partial bump — lock updated but derived files not — impossible to
merge (§10, "Partial bump").

What it generates:

- **`.claude-plugin/marketplace.json`** — the `varvig-tools` marketplace listing
  pointing at `plugins/varvig`.
- **`plugins/varvig/.claude-plugin/plugin.json`** — plugin metadata; set its
  version from the lock, and declare a **minimum core version**.
- **`plugins/varvig/.mcp.json`** — take the §6.3 simplification:

  ```json
  { "mcpServers": { "varvig": { "command": "varvig", "args": ["mcp"] } } }
  ```

  The plugin assumes `varvig` is already on `PATH`, so **the plugin itself needs
  no hash pin at all** (§6.3). Instead it declares the minimum core version and
  should fail with a clear message — `varvig >= 0.3.0 required, found 0.2.4` —
  not obscurely at the first tool call. **Do not** add a download-and-bootstrap
  step to "make install easier": it reintroduces the pin, adds a network
  dependency to plugin load, and duplicates the MCPB bundle.

So the lock ends up binding only two things: the **MCPB bundles** and the
**published install instructions** (README). Both still need it — keep the lock —
but the Claude Code path no longer depends on the cross-repo automation at all.

---

## 5. `bump-core` workflow (§5)

Triggers (§5): `repository_dispatch: [core-release]` **and** `workflow_dispatch`
with a `version` input (build it dispatch-free first — §11 step 5 — and run it
by hand a few times before wiring the dispatch in core).

```yaml
concurrency:
  group: bump-core
  cancel-in-progress: true      # a newer release supersedes an older bump
permissions:
  contents: write
  pull-requests: write
```

Steps (§5.1):

1. Resolve `VERSION` from `client_payload.version` or the manual input.
2. **Download every release artifact and compute sha256 locally.**
3. Verify computed hashes against the release's `SHA256SUMS`, **and** verify
   provenance: `gh attestation verify <file> --repo varvig/varvig` for each.
4. Write `varvig.lock.json` (hashes from step 2, never from the payload).
5. Run `tools/generate.sh`.
6. Smoke test (§6 below).
7. Open a **PR** (not a direct push, §5.3) titled `chore: pin core v0.3.1`,
   label `automated`, enable auto-merge on green.

Why a PR and not a push (§5.3): it costs nothing extra and buys a review surface,
CI gates, and a revert button at the exact moment you least want to skip them.

---

## 6. CI in `varvig/plugins` — validate, never produce (§6.2)

The plugins repo's CI is lint and gate only:

```yaml
- run: tools/generate.sh && git diff --exit-code    # generated files current (§2.2)
- run: jq -e . .claude-plugin/marketplace.json plugins/varvig/.mcp.json
- run: claude plugin validate plugins/varvig
- run: tools/smoke.sh                               # downloads pinned binary, §7
```

`tools/smoke.sh` (§7) downloads the pinned binary into a scratch dir, proves it
works **through the plugin path**, and deletes it — it never commits one:

```bash
tools/fetch-binary.sh "$VERSION" linux-amd64        # download + checksum-verify
./dist/varvig --version | grep -q "${VERSION#v}"
tools/mcp-smoke.py ./dist/varvig mcp                 # copied from core; inits its own scratch repo
claude plugin validate plugins/varvig
```

`mcp-smoke.py` asserts the directory-submission blocker (distribution §6.1): every
MCP tool carries a `title` and the applicable `readOnlyHint`/`destructiveHint`.
Core already satisfies this and ships the script — copy
`varvig/varvig/tools/mcp-smoke.py` in and call it. Catching a missing hint here
is far cheaper than catching it in directory review.

The bump PR must not merge until this passes — otherwise the automation
faithfully ships a broken pin.

---

## 7. Drift detection (§8)

A scheduled check converts silent rot into a visible alert. This is the
**authoritative** one (it reads the local lock):

```yaml
name: drift-check
on:
  schedule: [{ cron: '0 7 * * *' }]
  workflow_dispatch:
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - env: { GH_TOKEN: '${{ secrets.GITHUB_TOKEN }}' }
        run: |
          PINNED=$(jq -r .version varvig.lock.json)
          LATEST=$(gh release view --repo varvig/varvig --json tagName -q .tagName)
          if [ "$PINNED" != "$LATEST" ]; then
            gh issue create \
              --title "Marketplace pin is stale: $PINNED vs $LATEST" \
              --label "release,urgent" \
              --body "Run the bump-core workflow, or investigate why the dispatch was lost."
          fi
```

Note: `varvig/varvig` **already has a release-side complement**
(`.github/workflows/drift-check.yml`) that watches from the other end and opens
an issue *in core* if the pin lags. The two are intentionally redundant —
different repo, token, and schedule — so one point of failure can't silence the
alarm. Build this plugins-side one anyway; it is the authoritative check.

---

## 8. Rollback (§9)

Yanking a core release means the pin must move too. Because the pin is one file
with a manual `workflow_dispatch` path, rollback is the **same mechanism** as
roll-forward:

```bash
gh workflow run bump-core --repo varvig/plugins -f version=v0.3.0
```

**Do not build a separate rollback path** — a rarely-exercised one will be broken
when you need it. Exercise this one deliberately once in a quiet week (§11 step 9).

---

## 9. Build order (plugins-side, from §11)

1. `varvig.lock.json` schema + `tools/generate.sh`; check the generated files in.
2. CI check that generated files are current (§2.2).
3. GitHub App + installation token for cross-repo writes (§2.5 / design §3) —
   coordinate with whoever owns `varvig/varvig` secrets.
4. `bump-core` with `workflow_dispatch` **only**; run it by hand a few times
   against a real published tag before trusting it.
5. `tools/smoke.sh` + copy in `mcp-smoke.py`; wire into CI.
6. `drift-check` schedule.
7. Ask the core owner to set `RELEASE_BOT_APP_ID` / `RELEASE_BOT_KEY` — this
   flips core's `bump-marketplace` dispatch on. Confirm an end-to-end release
   drives a green bump PR.
8. Rehearse a rollback.

---

## 10. Quick reference — what core guarantees you

- Assets named per §2.2, plus `SHA256SUMS`, all attested (`--repo varvig/varvig`).
- Dispatch `core-release` with `client_payload.version = "vX.Y.Z"` (once the App
  is configured; gated off until then).
- `varvig --version` → `varvig vX.Y.Z os/arch`; `varvig mcp` gate with fully
  annotated tools; `tools/mcp-smoke.py` to assert it.
- Config knobs core reads: `vars.RELEASE_BOT_APP_ID`, `secrets.RELEASE_BOT_KEY`,
  optional `vars.PLUGINS_REPO`.

If any of those stops matching what you observe, check `varvig/RELEASE.md` and the
`varvig/varvig` release workflow (`.github/workflows/release.yml`) — that is the
source of truth for the producing side.
