# Varvig

A source control system designed for **AI agents working in parallel**, not for
humans working sequentially. Humans remain a supported consumer, but they are a
rendering target rather than the primary user.

Its core promise: **nothing an agent or the tool does is destructive.** Agents
hold scoped, propose-only credentials — they read within a scope and propose
signed, speculative changes; a human (or a gate) decides what lands. History is
append-only, refs move only by compare-and-swap, and every change carries
provenance recording who or what produced it.

## What's here

| Path | What it is |
|------|------------|
| [`varvig/`](./varvig) | The implementation: a single portable, statically-linked Go binary that is client, server, and tooling. Start at [`varvig/README.md`](./varvig/README.md). |
| [`varvig/DESIGN.md`](./varvig/DESIGN.md) | The full design and build order. |
| [`varvig/AUTH.md`](./varvig/AUTH.md) | Identity, authorization, task credentials, and the read API. |
| [`varvig/FORMAT.md`](./varvig/FORMAT.md) · [`WIRE.md`](./varvig/WIRE.md) · [`CONFORMANCE.md`](./varvig/CONFORMANCE.md) | The frozen object format, the sync wire protocol, and the conformance suite. |
| [`varvig/RELEASE.md`](./varvig/RELEASE.md) | How releases are built and published. |
| [`mcpb/`](./varvig/mcpb) · [`tools/`](./varvig/tools) | The MCP bundle manifest and the release/build tooling. |
| [`.github/workflows/`](./.github/workflows) | CI, the release pipeline, and the marketplace drift check. |
| [`plugins-repo-instructions.md`](./plugins-repo-instructions.md) | Handoff notes for the companion `varvig/plugins` marketplace repo. |

## Quick start

```sh
cd varvig
go build -o varvig ./cmd/varvig     # single static binary; CGO not required

./varvig init myrepo                # initialize a repo (also writes agent rules — see below)
cd myrepo
echo hello > a.txt
../varvig commit -m "first change"  # signed, with provenance
../varvig log                       # walk the change DAG
../varvig verify                    # check provenance and signatures
```

Run `varvig` with no arguments for the full command list, or `varvig --version`.

## For agents

`varvig init` writes agent-rules files so a coding agent drives varvig correctly:

- **`VARVIG-AGENTS.md`** — generated from varvig's own feature surface and
  overwritten in full on every init/upgrade, so it never describes a flag that
  no longer exists.
- **`AGENTS.md`** (and, if present, `CLAUDE.md`, Cursor/Windsurf/Copilot config)
  — a small pointer block aimed at the one canonical generated file, so multiple
  tools converge on a single source of truth.

Agents connect through the **MCP gate** (`varvig mcp`): a scoped, propose-only
interface where reads are logged into provenance and proposals can never move a
ref. Regenerate or check the rules with `varvig init --agent-rules`
(`--check` is the CI entrypoint). See the generated `VARVIG-AGENTS.md` for the
current rules.

## License

Varvig is free software, licensed under the **GNU General Public License v3.0**.
See [`LICENSE`](./LICENSE) for the full text.

```
Copyright (C) 2026 varvig contributors

This program is free software: you can redistribute it and/or modify it under
the terms of the GNU General Public License as published by the Free Software
Foundation, either version 3 of the License, or (at your option) any later
version. This program is distributed in the hope that it will be useful, but
WITHOUT ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
FITNESS FOR A PARTICULAR PURPOSE. See the GNU General Public License for more
details.
```
