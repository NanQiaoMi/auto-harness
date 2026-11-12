#!/usr/bin/env python3
"""Example MCP server for auto-harness.

Speaks the Model Context Protocol over stdio: newline-delimited
JSON-RPC 2.0.

    client -> server: {"jsonrpc":"2.0","id":1,"method":"initialize","params":{...}}
    server -> client: {"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18",...}}

Try it out:

    echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
        | ./demo_mcp_server.py
"""

import json
import sys

PROTOCOL_VERSION = "2025-06-18"

TOOLS = [
    {
        "name": "add",
        "title": "Add numbers",
        "description": "Add two numbers and return the sum.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "a": {"type": "number", "description": "first number"},
                "b": {"type": "number", "description": "second number"},
            },
            "required": ["a", "b"],
        },
    },
    {
        "name": "echo",
        "title": "Echo",
        "description": "Echo the given text back to the caller.",
        "inputSchema": {
            "type": "object",
            "properties": {"text": {"type": "string"}},
            "required": ["text"],
        },
    },
]


def send(message):
    sys.stdout.write(json.dumps(message) + "\n")
    sys.stdout.flush()


def read():
    line = sys.stdin.readline()
    if not line:
        return None
    line = line.strip()
    if not line:
        return read()
    return json.loads(line)


def call_tool(params):
    name = params.get("name", "")
    args = params.get("arguments") or {}
    if name == "add":
        try:
            total = float(args.get("a", 0)) + float(args.get("b", 0))
        except (TypeError, ValueError):
            return {"content": [{"type": "text", "text": "a and b must be numbers"}], "isError": True}
        if total == int(total):
            total = int(total)
        return {"content": [{"type": "text", "text": str(total)}], "isError": False}
    if name == "echo":
        return {"content": [{"type": "text", "text": args.get("text", "")}], "isError": False}
    return {"content": [{"type": "text", "text": "unknown tool: " + name}], "isError": True}


def main():
    while True:
        try:
            message = read()
        except json.JSONDecodeError:
            continue
        if message is None:
            return

        method = message.get("method", "")
        message_id = message.get("id")
        params = message.get("params") or {}

        if method == "initialize":
            send({
                "jsonrpc": "2.0",
                "id": message_id,
                "result": {
                    "protocolVersion": PROTOCOL_VERSION,
                    "capabilities": {"tools": {"listChanged": True}},
                    "serverInfo": {"name": "demo", "title": "Demo MCP server", "version": "1.0.0"},
                    "instructions": "A demo MCP server with arithmetic and echo tools.",
                },
            })
        elif method == "notifications/initialized":
            continue
        elif method == "tools/list":
            send({"jsonrpc": "2.0", "id": message_id, "result": {"tools": TOOLS}})
        elif method == "tools/call":
            send({"jsonrpc": "2.0", "id": message_id, "result": call_tool(params)})
        elif method == "ping":
            send({"jsonrpc": "2.0", "id": message_id, "result": {}})
        elif message_id is not None:
            send({
                "jsonrpc": "2.0",
                "id": message_id,
                "error": {"code": -32601, "message": "method not found: " + method},
            })


if __name__ == "__main__":
    main()
