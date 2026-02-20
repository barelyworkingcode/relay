#!/usr/bin/env python3
"""Test script that simulates Claude Desktop talking to Relay.

Usage:
  python test_relay.py bridge    # test bridge socket directly
  python test_relay.py mcp       # test full MCP stdio path (default)
  python test_relay.py both      # run both tests
"""

import json
import os
import socket
import subprocess
import sys

TOKEN = os.environ.get("RELAY_TOKEN", "your-token-here")
SOCKET_PATH = os.path.expanduser("~/Library/Application Support/relay/relay.sock")
RELAY_BIN = "/Applications/Relay.app/Contents/MacOS/relay"

GREEN = "\033[32m"
RED = "\033[31m"
YELLOW = "\033[33m"
DIM = "\033[2m"
RESET = "\033[0m"


def pass_msg(label, detail=""):
    print(f"  {GREEN}PASS{RESET}  {label}" + (f"  {DIM}{detail}{RESET}" if detail else ""))


def fail_msg(label, detail=""):
    print(f"  {RED}FAIL{RESET}  {label}" + (f"\n        {detail}" if detail else ""))


def header(title):
    print(f"\n{YELLOW}=== {title} ==={RESET}\n")


# -- Bridge tests ----------------------------------------------------------

def bridge_send(request: dict) -> dict:
    """Open a fresh Unix socket connection, send one request, read one response."""
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.settimeout(10)
    sock.connect(SOCKET_PATH)
    try:
        payload = json.dumps(request) + "\n"
        sock.sendall(payload.encode())
        buf = b""
        while b"\n" not in buf:
            chunk = sock.recv(1024 * 1024)
            if not chunk:
                break
            buf += chunk
        return json.loads(buf.strip())
    finally:
        sock.close()


def test_bridge():
    header("Bridge (direct Unix socket)")

    # 1. ListTools
    print("ListTools...")
    try:
        resp = bridge_send({"type": "ListTools", "token": TOKEN})
        if resp.get("type") == "Error":
            fail_msg("ListTools", f"Bridge error: {resp.get('message')}")
            return
        tools = json.loads(resp["tools"]) if isinstance(resp.get("tools"), str) else resp.get("tools", [])
        names = [t.get("name", "?") for t in tools]
        pass_msg("ListTools", f"{len(tools)} tools: {', '.join(names[:8])}{'...' if len(names) > 8 else ''}")
    except Exception as e:
        fail_msg("ListTools", str(e))
        return

    # 2. CallTool -- list_voices
    print("CallTool(list_voices)...")
    try:
        resp = bridge_send({"type": "CallTool", "name": "list_voices", "arguments": {}, "token": TOKEN})
        if resp.get("type") == "Error":
            fail_msg("CallTool", f"Bridge error (code {resp.get('code')}): {resp.get('message')}")
            return
        result = resp.get("result")
        if isinstance(result, str):
            result = json.loads(result)
        # result is a CallToolResult with "content" array
        text = ""
        if isinstance(result, dict):
            for c in result.get("content", []):
                if c.get("type") == "text":
                    text = c["text"]
                    break
        detail = text[:120] + "..." if len(text) > 120 else text
        pass_msg("CallTool(list_voices)", detail)
    except Exception as e:
        fail_msg("CallTool(list_voices)", str(e))


# -- MCP stdio tests -------------------------------------------------------

def mcp_rpc(proc, method, params=None, req_id=None, expect_response=True):
    """Send a JSON-RPC 2.0 request over stdin, optionally read response from stdout."""
    msg = {"jsonrpc": "2.0", "method": method}
    if req_id is not None:
        msg["id"] = req_id
    if params is not None:
        msg["params"] = params

    line = json.dumps(msg) + "\n"
    proc.stdin.write(line)
    proc.stdin.flush()

    if not expect_response:
        return None

    raw = proc.stdout.readline()
    if not raw:
        raise RuntimeError("No response from relay mcp (stdout closed)")
    return json.loads(raw)


def test_mcp():
    header("MCP stdio (Claude Desktop simulation)")

    if not os.path.exists(RELAY_BIN):
        fail_msg("Binary", f"{RELAY_BIN} not found")
        return

    proc = subprocess.Popen(
        [RELAY_BIN, "mcp", "--token", TOKEN],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        bufsize=1,
    )

    try:
        # 1. initialize
        print("initialize...")
        try:
            resp = mcp_rpc(proc, "initialize", {
                "protocolVersion": "2024-11-05",
                "capabilities": {},
                "clientInfo": {"name": "test_relay.py", "version": "1.0.0"},
            }, req_id=1)
            if resp.get("error"):
                fail_msg("initialize", json.dumps(resp["error"]))
                return
            info = resp.get("result", {}).get("serverInfo", {})
            pass_msg("initialize", f"server={info.get('name')} v{info.get('version')}")
        except Exception as e:
            fail_msg("initialize", str(e))
            return

        # 2. notifications/initialized (no response expected)
        print("notifications/initialized...")
        try:
            mcp_rpc(proc, "notifications/initialized", expect_response=False)
            pass_msg("notifications/initialized", "sent (no response expected)")
        except Exception as e:
            fail_msg("notifications/initialized", str(e))

        # 3. tools/list
        print("tools/list...")
        try:
            resp = mcp_rpc(proc, "tools/list", {}, req_id=2)
            if resp.get("error"):
                fail_msg("tools/list", json.dumps(resp["error"]))
                return
            tools = resp.get("result", {}).get("tools", [])
            names = [t.get("name", "?") for t in tools]
            pass_msg("tools/list", f"{len(tools)} tools: {', '.join(names[:8])}{'...' if len(names) > 8 else ''}")
        except Exception as e:
            fail_msg("tools/list", str(e))
            return

        # 4. tools/call -- list_voices
        print("tools/call(list_voices)...")
        try:
            resp = mcp_rpc(proc, "tools/call", {"name": "list_voices", "arguments": {}}, req_id=3)
            if resp.get("error"):
                fail_msg("tools/call", json.dumps(resp["error"]))
                return
            result = resp.get("result", {})
            text = ""
            for c in result.get("content", []):
                if c.get("type") == "text":
                    text = c["text"]
                    break
            detail = text[:120] + "..." if len(text) > 120 else text
            pass_msg("tools/call(list_voices)", detail)
        except Exception as e:
            fail_msg("tools/call(list_voices)", str(e))

    finally:
        proc.stdin.close()
        proc.wait(timeout=5)
        stderr = proc.stderr.read()
        if stderr.strip():
            print(f"\n  {DIM}relay stderr:{RESET}")
            for line in stderr.strip().splitlines():
                print(f"    {DIM}{line}{RESET}")


# -- Main ------------------------------------------------------------------

def main():
    mode = sys.argv[1] if len(sys.argv) > 1 else "mcp"

    if mode in ("bridge", "both"):
        test_bridge()
    if mode in ("mcp", "both"):
        test_mcp()
    if mode not in ("bridge", "mcp", "both"):
        print(f"Unknown mode: {mode}")
        print("Usage: python test_relay.py [bridge|mcp|both]")
        sys.exit(1)

    print()


if __name__ == "__main__":
    main()
