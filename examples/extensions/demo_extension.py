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

For a prompt ("input") the answer can rewrite it instead of refusing it:

    extension -> harness: {"id":4,"result":{"text":"..."}}

Run it directly to try it out:

    echo '{"id":1,"method":"manifest"}' | ./demo_extension.py
"""

import json
import sys
from datetime import date

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
        # The harness asks the extension about these events before it
        # acts on them. An extension that does not name an event is
        # never asked, so its silence is not a refusal.
        "consults": [
            "tool_call", "user_bash", "input",
            # Session operations: the harness asks before it writes or
            # moves anything, so a veto leaves the session untouched.
            "session_before_switch", "session_before_fork", "session_before_tree",
        ],
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
    before it happens. Returning nothing means "no objection".

    For a gate, silence is not permission: when no answer arrives the
    harness blocks the operation, so a broken policy extension cannot
    become an open door. The prompt ("input") is the exception — a
    rewrite that cannot be computed leaves the user's line alone.
    """
    event = params.get("event", "")
    call = params.get("toolCall") or {}
    args = call.get("arguments") or {}
    command = args.get("command", "") or params.get("bashCommand", "")

    if command and any(bad in command for bad in BLOCKED_COMMANDS):
        return {"block": True, "reason": "the demo extension blocks %r" % command}

    if event == "input":
        # A rewrite, not a gate: answer with the text to use instead.
        # Returning nothing leaves the prompt alone, which is also what
        # happens when an extension is too slow to answer.
        text = params.get("text", "")
        if "{{today}}" in text:
            return {"text": text.replace("{{today}}", date.today().isoformat())}
        return {}

    if event == "session_before_compact":
        # Let the harness compact on its own.
        return {}

    if event in ("session_before_switch", "session_before_fork", "session_before_tree"):
        # Veto a new session, a fork or a /tree move with
        # {"cancel": True, "reason": "..."} — the harness asks before it
        # writes or moves anything, so nothing is left half-done. This
        # demo allows them.
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
