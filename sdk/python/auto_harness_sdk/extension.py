"""Server helper for writing auto-harness process extensions in Python."""

from __future__ import annotations

import json
import sys
import threading
from dataclasses import dataclass
from typing import Any, BinaryIO, Callable, Dict, List, Optional

SCHEMA_VERSION = "1.0"


@dataclass
class ToolResult:
    content: str
    is_error: bool = False

    def to_wire(self) -> Dict[str, Any]:
        out: Dict[str, Any] = {"content": self.content}
        if self.is_error:
            out["is_error"] = True
        return out


@dataclass
class ConsultResult:
    block: bool = False
    reason: str = ""
    cancel: bool = False
    text: Optional[str] = None

    def to_wire(self) -> Dict[str, Any]:
        out: Dict[str, Any] = {}
        if self.block:
            out["block"] = True
        if self.reason:
            out["reason"] = self.reason
        if self.cancel:
            out["cancel"] = True
        if self.text is not None:
            out["text"] = self.text
        return out


def _validate_schema_version(version: Any) -> None:
    if version is None or version == "":
        return
    if isinstance(version, str) and (version == "1" or version.startswith("1.")):
        return
    raise ValueError(f"unsupported schema_version {version!r} (supported major version: 1)")


class ExtensionServer:
    """Speaks the auto-harness stdio JSON-RPC extension protocol."""

    def __init__(self, name: str, description: str = "") -> None:
        self.name = name
        self.description = description
        self._tools: List[Dict[str, Any]] = []
        self._tool_handlers: Dict[str, Callable[[Dict[str, Any], str], Any]] = {}
        self._commands: List[Dict[str, Any]] = []
        self._command_handlers: Dict[str, Callable[[str], str]] = {}
        self._consults: List[str] = []
        self._consult_handlers: Dict[str, Callable[[Dict[str, Any]], Any]] = {}
        self._out_lock = threading.Lock()
        self._out: BinaryIO = sys.stdout.buffer

    def tool(
        self,
        name: str,
        description: str,
        parameters: Optional[Dict[str, Any]] = None,
        label: str = "",
        retryable: bool = False,
    ) -> Callable[[Callable[[Dict[str, Any], str], Any]], Callable[[Dict[str, Any], str], Any]]:
        def decorator(fn: Callable[[Dict[str, Any], str], Any]) -> Callable[[Dict[str, Any], str], Any]:
            spec: Dict[str, Any] = {
                "name": name,
                "description": description,
                "parameters": parameters or {"type": "object", "properties": {}},
            }
            if label:
                spec["label"] = label
            if retryable:
                spec["retryable"] = True
            self._tools.append(spec)
            self._tool_handlers[name] = fn
            return fn

        return decorator

    def command(
        self, name: str, description: str = ""
    ) -> Callable[[Callable[[str], str]], Callable[[str], str]]:
        def decorator(fn: Callable[[str], str]) -> Callable[[str], str]:
            spec: Dict[str, Any] = {"name": name}
            if description:
                spec["description"] = description
            self._commands.append(spec)
            self._command_handlers[name] = fn
            return fn

        return decorator

    def consult(
        self, event: str
    ) -> Callable[[Callable[[Dict[str, Any]], Any]], Callable[[Dict[str, Any]], Any]]:
        def decorator(fn: Callable[[Dict[str, Any]], Any]) -> Callable[[Dict[str, Any]], Any]:
            if event not in self._consult_handlers:
                self._consults.append(event)
            self._consult_handlers[event] = fn
            return fn

        return decorator

    def emit(self, event: str, text: str = "", data: Optional[Dict[str, Any]] = None) -> None:
        params: Dict[str, Any] = {"event": event}
        if text:
            params["text"] = text
        if data is not None:
            params["data"] = data
        self._write({"schema_version": SCHEMA_VERSION, "method": "event", "params": params})

    def serve(
        self,
        stdin: Optional[BinaryIO] = None,
        stdout: Optional[BinaryIO] = None,
    ) -> None:
        in_stream = stdin if stdin is not None else sys.stdin.buffer
        if stdout is not None:
            self._out = stdout

        while True:
            raw = in_stream.readline()
            if not raw:
                break
            line = raw.decode("utf-8").rstrip("\r\n")
            if not line.strip():
                continue

            req_id: Any = None
            try:
                req = json.loads(line)
                req_id = req.get("id")
                _validate_schema_version(req.get("schema_version"))
                method = req.get("method", "")
                params = req.get("params") or {}

                if method == "shutdown":
                    break
                if method == "manifest":
                    result = self._manifest()
                elif method == "tool.execute":
                    result = self._run_tool(params)
                elif method == "command.execute":
                    result = self._run_command(params)
                elif method == "event.consult":
                    result = self._run_consult(params)
                else:
                    if req_id is not None:
                        self._write_error(req_id, -32601, f"unknown method {method!r}")
                    continue

                if req_id is not None:
                    self._write({"schema_version": SCHEMA_VERSION, "id": req_id, "result": result})
            except Exception as exc:  # noqa: BLE001
                if req_id is not None:
                    self._write_error(req_id, -32000, str(exc))

    def _manifest(self) -> Dict[str, Any]:
        out: Dict[str, Any] = {
            "schema_version": SCHEMA_VERSION,
            "name": self.name,
        }
        if self.description:
            out["description"] = self.description
        if self._tools:
            out["tools"] = list(self._tools)
        if self._commands:
            out["commands"] = list(self._commands)
        if self._consults:
            out["consults"] = list(self._consults)
        return out

    def _run_tool(self, params: Dict[str, Any]) -> Dict[str, Any]:
        name = params.get("name", "")
        handler = self._tool_handlers.get(name)
        if handler is None:
            return ToolResult(content=f"unknown tool {name!r}", is_error=True).to_wire()
        res = handler(params.get("arguments") or {}, params.get("tool_call_id", ""))
        if isinstance(res, ToolResult):
            return res.to_wire()
        if isinstance(res, dict):
            return res
        return ToolResult(content=str(res)).to_wire()

    def _run_command(self, params: Dict[str, Any]) -> Dict[str, Any]:
        name = params.get("name", "")
        handler = self._command_handlers.get(name)
        if handler is None:
            raise ValueError(f"unknown command {name!r}")
        text = handler(params.get("args", ""))
        return {"text": "" if text is None else str(text)}

    def _run_consult(self, params: Dict[str, Any]) -> Dict[str, Any]:
        event = params.get("event", "")
        handler = self._consult_handlers.get(event)
        if handler is None:
            return {}
        res = handler(params)
        if res is None:
            return {}
        if isinstance(res, ConsultResult):
            return res.to_wire()
        if isinstance(res, dict):
            return res
        raise TypeError(f"unsupported consult result type: {type(res).__name__}")

    def _write_error(self, req_id: Any, code: int, message: str) -> None:
        self._write(
            {
                "schema_version": SCHEMA_VERSION,
                "id": req_id,
                "error": {"code": code, "message": message},
            }
        )

    def _write(self, record: Dict[str, Any]) -> None:
        raw = (json.dumps(record, separators=(",", ":")) + "\n").encode("utf-8")
        with self._out_lock:
            self._out.write(raw)
            self._out.flush()
