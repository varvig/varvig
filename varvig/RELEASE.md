# Releasing varvig

This is the core half of the release automation described in *Design Notes IV:
Release Automation*. It covers everything that lives in `varvig/varvig`; the
marketplace pin, `marketplace.json`, and the bump PR live in `varvig/plugins`.

The one job the whole system exists to do: **keep the binary the marketplace
serves in sync with what this repo releases.** It fails silently when it breaks,
so treat it as release-blocking infrastructure, not a convenience script.

## What a tag does

Pushing a `v*` tag runs `.github/workflows/release.yml`:

1. **build** — cross-compiles a static, `CGO_ENABLED=0` binary for each of
   `linux-amd64`, `linux-arm64`, `darwin-amd64`, `darwin-arm64`,
   `windows-amd64`. Each is stamped with the tag via `-ldflags -X main.version`,
   so `varvig --version` reports it.
2. **bundle** — packs one MCPB bundle per shipping platform
   (`linux-amd64`, `linux-arm64`, `darwin-universal`, `windows-amd64`),
   embedding the built binary. The bundle is a rebuild, not a string
   substitution, because it carries the binary (§6.1).
3. **publish** — fuses the two macOS slices into `darwin-universal`, writes
   `SHA256SUMS`, attests build provenance, and attaches the four binaries, the
   four bundles, and the checksums to the GitHub Release.
4. **bump-marketplace** — dispatches `core-release` to `varvig/plugins` with the
   **version tag only** (never a hash — the bump job derives hashes from the
   artifacts, §5.2). Skipped until the release-bot App is configured
   (`vars.RELEASE_BOT_APP_ID`); the plugins-side drift check is the backstop.

## The pieces in this repo

| Path | Role |
|---|---|
| `tools/build.sh` | Cross-compile one `goos-goarch` target into `<repo>/dist`. |
| `tools/lipo.sh` | Fuse the darwin slices into `darwin-universal`. |
| `tools/makefat/` | Pure-Go universal-Mach-O writer, so `lipo` isn't needed on the Linux runner. |
| `tools/mcp-smoke.py` | Drive the MCP handshake and assert every tool carries a title + read-only/destructive hints (§7). |
| `mcpb/manifest.json` | MCPB manifest; `version` and the Windows command are patched at pack time. |
| `cmd/varvig/version.go` | `varvig version` / `--version`, the single source of the version tag. |

## Building and smoke-testing locally

```bash
# one target
VERSION=v0.3.1 tools/build.sh linux-amd64      # -> ../dist/varvig-linux-amd64

# the macOS universal binary
VERSION=v0.3.1 tools/build.sh darwin-amd64
VERSION=v0.3.1 tools/build.sh darwin-arm64
tools/lipo.sh                                  # -> ../dist/varvig-darwin-universal

# assert the MCP tool surface is directory-submittable
python3 tools/mcp-smoke.py ../dist/varvig-linux-amd64 mcp
```

## Cross-repo credentials

`GITHUB_TOKEN` cannot write to `varvig/plugins`, so the bump dispatch runs as a
GitHub App (`create-github-app-token`), configured with:

- repo **variable** `RELEASE_BOT_APP_ID` — the App's id (its presence also gates
  the `bump-marketplace` job on),
- repo **secret** `RELEASE_BOT_KEY` — the App's private key,
- optional repo **variable** `PLUGINS_REPO` — the plugins repo, default
  `varvig/plugins`.

## Rollback

Rollback is the same mechanism as roll-forward — re-run the plugins-side
`bump-core` workflow against the older tag (§9). There is deliberately no
separate rollback path here.
