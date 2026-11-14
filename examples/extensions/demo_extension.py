#!/usr/bin/env python3
"""Example process extension for auto-harness.

Speaks the extension RPC protocol over stdio: newline-delimited JSON.

    harness -> extension: {"id":1,"method":"manifest","params":null}
    extension -> harness: {"id":1,"result":{"name":"demo","tools":[...]}}
    harness -> extension: {"id":2,"method":"tool.execute","params":{...}}
    extension -> harness: {"id":2,"result":{"content":"...","is_error":false}}
    extension -> harness: {"method":"event","params":{"event":"input","text":"..."}}

And the other direction: the harness asks the extension whether an
operation may happen, and the extension may veto it.

    harness -> extension: {"id":3,"method":"event.consult","params":{"event":"tool_call",...}}
    extension -> harness: {"id":3,"result":{"block":true,"reason":"..."}}

Run it directly to try it out:

    echo '{"id":1,"method":"manifest"}' | ./demo_extension.py
"""

import json
import sys

# Commands the demo refuses to run. A real extension would have a
# policy of its own here; this one keeps it short.
BLOCKED_COMMANDS = ("rm -rf", ":(){", "mkfs")


def read_message():
    """Read one JSON message from stdin, or None at EOF."""
    line = sys.stdin.readline()
    if not line:
        return None
    line = line.strip()
    if not line:
        return read_message()
    return json.loads(line)


def send(message):
    """Write one JSON message to stdout."""
    sys.stdout.write(json.dumps(message) + "\n")
    sys.stdout.flush()


def manifest():
    return {
        "name": "demo",
        "description": "Example extension written in Python",
        "tools": [
            {
                "name": "demo_echo",
                "label": "Echo",
                "description": "Echo the given text back to the model.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "text": {"type": "string", "description": "text to echo"}
                    },
                    "required": ["text"],
                },
            },
            {
                "name": "demo_add",
                "label": "Add",
                "description": "Add two numbers.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "a": {"type": "number"},
                        "b": {"type": "number"},
                    },
                    "required": ["a", "b"],
                },
            },
        ],
    }


def execute(params):
    """Run a tool and return its result payload."""
    name = params.get("name", "")
    args = params.get("arguments") or {}
    if name == "demo_echo":
        return {"content": args.get("text", "")}
    if name == "demo_add":
        try:
            total = float(args.get("a", 0)) + float(args.get("b", 0))
        except (TypeError, ValueError):
            return {"content": "a and b must be numbers", "is_error": True}
        if total == int(total):
            total = int(total)
        return {"content": str(total)}
    return {"content": "unknown tool: " + name, "is_error": True}


def consult(params):
    """Answer the harness's question about an operation.

    The harness asks before it acts, so a veto here stops the call
    before it happens. Returning nothing means "no objection", and an
    extension that never answers is treated the same way: a broken
    extension must not freeze the harness.
    """
    event = params.get("event", "")
    call = params.get("toolCall") or {}
    args = call.get("arguments") or {}
    command = args.get("command", "") or params.get("bashCommand", "")

    if command and any(bad in command for bad in BLOCKED_COMMANDS):
        return {"block": True, "reason": "the demo extension blocks %r" % command}

    if event == "session_before_compact":
        # Let the harness compact on its own.
        return {}
    return {}


def main():
    while True:
        try:
            message = read_message()
        except json.JSONDecodeError as exc:
            send({"error": {"code": -32700, "message": "parse error: %s" % exc}})
            continue
        if message is None:
            return

        method = message.get("method", "")
        message_id = message.get("id")
        params = message.get("params") or {}

        if method == "manifest":
            send({"id": message_id, "result": manifest()})
        elif method == "tool.execute":
            send({"id": message_id, "result": execute(params)})
        elif method == "event.consult":
            send({"id": message_id, "result": consult(params)})
        elif method == "shutdown":
            send({"id": message_id, "result": {}})
            return
        elif method == "event":
            # The harness sends this to push an event to the extension.
            continue
        elif message_id:
            send({"id": message_id, "error": {"code": -32601, "message": "unknown method: " + method}})


if __name__ == "__main__":
    main()
