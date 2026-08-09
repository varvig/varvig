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

        if problems:
            fail("tool annotation requirements not met:\n  " +
                 "\n  ".join(problems))

        names = ", ".join(sorted(t.get("name", "?") for t in tools))
        print(f"mcp-smoke: OK — {len(tools)} tools, all carry title + hints "
              f"({names})")
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
