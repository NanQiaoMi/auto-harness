"""Subprocess RPC client for driving `harness -rpc` over LF-only JSONL."""

from __future__ import annotations

import json
import os
import queue
import subprocess
import threading
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional, Sequence

SCHEMA_VERSION = "1.0"


class RPCError(RuntimeError):
    """Raised when the harness returns an unsuccessful RPC response."""

    def __init__(self, command: str, message: str) -> None:
        super().__init__(f"rpc {command} failed: {message}")
        self.command = command
        self.message = message


@dataclass
class PromptResult:
    """Result of running a prompt until `agent_settled`."""

    disposition: str
    text: str
    aborted: bool = False
    events: List[Dict[str, Any]] = field(default_factory=list)


class RPCClient:
    """Thread-safe client for `harness -rpc` over stdin/stdout JSONL."""

    def __init__(
        self,
        command: Sequence[str],
        cwd: Optional[str] = None,
        env: Optional[Dict[str, str]] = None,
    ) -> None:
        self._command = list(command)
        self._cwd = cwd
        self._env = env
        self._proc: Optional[subprocess.Popen[bytes]] = None
        self._reader_thread: Optional[threading.Thread] = None
        self._lock = threading.Lock()
        self._write_lock = threading.Lock()
        self._seq = 0
        self._pending: Dict[str, queue.Queue[Optional[Dict[str, Any]]]] = {}
        self._subscribers: Dict[int, queue.Queue[Optional[Dict[str, Any]]]] = {}
        self._next_sub = 0
        self._closed = False

    def __enter__(self) -> "RPCClient":
        self.start()
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        self.close()

    def start(self) -> None:
        if self._proc is not None:
            return
        proc_env = os.environ.copy()
        if self._env:
            proc_env.update(self._env)
        self._proc = subprocess.Popen(
            self._command,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            cwd=self._cwd,
            env=proc_env,
        )
        self._reader_thread = threading.Thread(target=self._read_loop, daemon=True)
        self._reader_thread.start()

    def _read_loop(self) -> None:
        assert self._proc is not None and self._proc.stdout is not None
        try:
            # Read raw bytes line-by-line so LF is the only record delimiter;
            # U+2028 and U+2029 inside JSON strings are preserved untouched.
            while True:
                raw = self._proc.stdout.readline()
                if not raw:
                    break
                line = raw.decode("utf-8").rstrip("\r\n")
                if not line.strip():
                    continue
                record = json.loads(line)
                if record.get("type") == "response":
                    req_id = record.get("id", "")
                    with self._lock:
                        q = self._pending.pop(req_id, None)
                    if q is not None:
                        q.put(record)
                else:
                    with self._lock:
                        subs = list(self._subscribers.values())
                    for q in subs:
                        q.put(record)
        finally:
            with self._lock:
                pending = list(self._pending.values())
                self._pending.clear()
                subs = list(self._subscribers.values())
                self._subscribers.clear()
            for q in pending:
                q.put(None)
            for q in subs:
                q.put(None)

    def call(
        self,
        cmd_type: str,
        fields: Optional[Dict[str, Any]] = None,
        timeout: float = 30.0,
    ) -> Dict[str, Any]:
        if self._proc is None or self._proc.stdin is None:
            raise RuntimeError("RPCClient has not been started")

        with self._lock:
            if self._closed:
                raise RuntimeError("RPCClient is closed")
            self._seq += 1
            req_id = f"py-{self._seq}"
            q: queue.Queue[Optional[Dict[str, Any]]] = queue.Queue(maxsize=1)
            self._pending[req_id] = q

        payload: Dict[str, Any] = {
            "schema_version": SCHEMA_VERSION,
            "id": req_id,
            "type": cmd_type,
        }
        if fields:
            payload.update(fields)

        encoded = (json.dumps(payload, separators=(",", ":")) + "\n").encode("utf-8")
        try:
            with self._write_lock:
                self._proc.stdin.write(encoded)
                self._proc.stdin.flush()
            resp = q.get(timeout=timeout)
        except BaseException:
            with self._lock:
                self._pending.pop(req_id, None)
            raise

        if resp is None:
            raise RuntimeError(f"RPC stream closed while waiting for {cmd_type}")
        if not resp.get("success", False):
            raise RPCError(cmd_type, resp.get("error", "unknown error"))
        return resp

    def prompt(
        self,
        message: str,
        images: Optional[List[Dict[str, Any]]] = None,
        on_event: Optional[Callable[[Dict[str, Any]], None]] = None,
        timeout: float = 60.0,
    ) -> PromptResult:
        ev_q: queue.Queue[Optional[Dict[str, Any]]] = queue.Queue()
        with self._lock:
            self._next_sub += 1
            sub_id = self._next_sub
            self._subscribers[sub_id] = ev_q

        try:
            fields: Dict[str, Any] = {"message": message}
            if images:
                fields["images"] = images
            resp = self.call("prompt", fields, timeout=timeout)
            disposition = (resp.get("data") or {}).get("disposition", "")

            events: List[Dict[str, Any]] = []
            deltas: List[str] = []
            final_text = ""

            while True:
                ev = ev_q.get(timeout=timeout)
                if ev is None:
                    raise RuntimeError("RPC stream closed before agent_settled")
                events.append(ev)
                if on_event is not None:
                    on_event(ev)

                ev_type = ev.get("type")
                data = ev.get("data") or {}
                if ev_type == "message_update" and not data.get("thinking"):
                    txt = data.get("text")
                    if isinstance(txt, str) and txt:
                        deltas.append(txt)
                elif ev_type == "message_end":
                    msg = data.get("message") or {}
                    if msg.get("role") == "assistant" and isinstance(msg.get("content"), str):
                        if msg["content"]:
                            final_text = msg["content"]
                elif ev_type == "agent_settled":
                    return PromptResult(
                        disposition=disposition,
                        text=final_text or "".join(deltas),
                        aborted=bool(data.get("aborted", False)),
                        events=events,
                    )
        finally:
            with self._lock:
                self._subscribers.pop(sub_id, None)

    def get_state(self, timeout: float = 10.0) -> Dict[str, Any]:
        return self.call("get_state", timeout=timeout).get("data") or {}

    def get_messages(self, timeout: float = 10.0) -> List[Dict[str, Any]]:
        data = self.call("get_messages", timeout=timeout).get("data") or {}
        return data.get("messages") or []

    def get_commands(self, timeout: float = 10.0) -> List[Dict[str, Any]]:
        data = self.call("get_commands", timeout=timeout).get("data") or {}
        return data.get("commands") or []

    def abort(self, timeout: float = 10.0) -> None:
        self.call("abort", timeout=timeout)

    def close(self, timeout: float = 10.0) -> int:
        with self._lock:
            if self._closed:
                return self._proc.returncode if self._proc and self._proc.returncode is not None else 0
            self._closed = True
        if self._proc is None:
            return 0
        if self._proc.stdin is not None:
            try:
                self._proc.stdin.close()
            except OSError:
                pass
        rc = self._proc.wait(timeout=timeout)
        if self._reader_thread is not None:
            self._reader_thread.join(timeout=timeout)
        for stream in (self._proc.stdout, self._proc.stderr):
            if stream is not None:
                try:
                    stream.close()
                except OSError:
                    pass
        return rc
