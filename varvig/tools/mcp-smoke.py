#!/usr/bin/env python3
"""mcp-smoke.py — smoke-test a varvig binary's MCP gate over stdio.

Usage:
    tools/mcp-smoke.py <varvig-binary> mcp [extra args...]

It drives the newline-delimited JSON-RPC handshake the gate speaks
(initialize -> initialized -> tools/list) and asserts the directory-submission
requirement (varvig-release-automation §7, distribution §6.1): every advertised
tool carries a human-readable `title` and the applicable `readOnlyHint` /
`destructiveHint` behavioral hints. A missing title or hint is a hard failure —
catching it here is far cheaper than catching it in directory review.

The gate needs a repository in its working directory, so the script initializes
a throwaway repo and runs the binary there. It exits non-zero on any failure.
"""

import json
import os
import subprocess
import sys
import tempfile
import threading


def fail(msg):
    print(f"mcp-smoke: FAIL: {msg}", file=sys.stderr)
    sys.exit(1)


def main():
    if len(sys.argv) < 3 or sys.argv[2] != "mcp":
        fail("usage: mcp-smoke.py <varvig-binary> mcp [args...]")
    # Resolve the binary before we change into the throwaway repo, so a relative
    # path like ./dist/varvig still points where the caller meant.
    binary = sys.argv[1]
    if os.path.sep in binary or (os.path.altsep and os.path.altsep in binary):
        binary = os.path.abspath(binary)
    mcp_args = sys.argv[2:]

    workdir = tempfile.mkdtemp(prefix="varvig-smoke-")
    # The gate opens the repo in its cwd; give it a fresh one to read from.
    init = subprocess.run(
        [binary, "init", workdir],
        capture_output=True, text=True,
    )
    if init.returncode != 0:
        fail(f"`varvig init` failed: {init.stderr.strip()}")

    proc = subprocess.Popen(
        [binary, *mcp_args],
        cwd=workdir,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        bufsize=1,
    )

    # Drain stderr so the gate's human-facing chatter never blocks the pipe.
    stderr_lines = []

    def drain():
        for line in proc.stderr:
            stderr_lines.append(line)

    threading.Thread(target=drain, daemon=True).start()

    def send(obj):
        proc.stdin.write(json.dumps(obj) + "\n")
        proc.stdin.flush()

    def recv():
        line = proc.stdout.readline()
        if not line:
            fail("gate closed the stream before replying "
                 f"(stderr: {''.join(stderr_lines).strip()})")
        return json.loads(line)

    try:
        send({"jsonrpc": "2.0", "id": 1, "method": "initialize",
              "params": {"protocolVersion": "2025-06-18", "capabilities": {},
                         "clientInfo": {"name": "mcp-smoke", "version": "0"}}})
        init_resp = recv()
        if init_resp.get("error"):
            fail(f"initialize errored: {init_resp['error']}")
        server_info = init_resp.get("result", {}).get("serverInfo", {})
        print(f"mcp-smoke: connected to {server_info.get('name')} "
              f"v{server_info.get('version')}")

        send({"jsonrpc": "2.0", "method": "notifications/initialized"})
        send({"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}})
        list_resp = recv()
        if list_resp.get("error"):
            fail(f"tools/list errored: {list_resp['error']}")

        tools = list_resp.get("result", {}).get("tools", [])
        if not tools:
            fail("tools/list returned no tools")

        problems = []
        for t in tools:
            name = t.get("name", "<unnamed>")
            ann = t.get("annotations") or {}
            title = t.get("title") or ann.get("title")
            if not title:
                problems.append(f"{name}: missing title")
            if "readOnlyHint" not in ann:
                problems.append(f"{name}: missing annotations.readOnlyHint")
            if "destructiveHint" not in ann:
                problems.append(f"{name}: missing annotations.destructiveHint")
            # The write path is append-only, so nothing is destructive (§4.2).
            if ann.get("destructiveHint") is True:
                problems.append(f"{name}: destructiveHint must be false "
                                "(the write path is append-only)")
            # There is no promotion tool. Do not add one (§4, §9): assert no
            # advertised tool is a promotion. This is a submission blocker.
            if "promote" in name:
                problems.append(f"{name}: a *promote* tool must not exist — "
                                "the gate can never promote")

        got = sorted(t.get("name", "?") for t in tools)
        # The exact advertised surface — read/propose plus ticket reads and the
        # blocked-on-scope report. Asserting the exact set catches an accidental
        # addition or removal (§9); keep it in step with the gate.
        want = sorted([
            "varvig_task_context", "varvig_resolve", "varvig_list_tree",
            "varvig_read_file", "varvig_find_files", "varvig_search_text",
            "varvig_read_change", "varvig_read_log", "varvig_read_ticket",
            "varvig_list_proposals", "varvig_propose", "varvig_report_blocked",
        ])
        if got != want:
            problems.append(f"tool set is {got}, want exactly {want}")

        if problems:
            fail("tool surface requirements not met:\n  " +
                 "\n  ".join(problems))

        print(f"mcp-smoke: OK — {len(tools)} tools, all carry title + hints, "
              f"no promotion tool ({', '.join(got)})")
    finally:
        try:
            proc.stdin.close()
        except Exception:
            pass
        try:
            proc.wait(timeout=5)
        except Exception:
            proc.kill()


if __name__ == "__main__":
    main()
