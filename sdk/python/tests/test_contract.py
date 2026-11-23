"""Black-box contract tests for the zero-dependency Python SDK against real harness + mockllm."""

from __future__ import annotations

import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

SDK_ROOT = Path(__file__).resolve().parents[1]
REPO_ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(SDK_ROOT))

from auto_harness_sdk import RPCClient, SCHEMA_VERSION  # noqa: E402

TINY_PNG_B64 = (
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
)

EXT_SCRIPT = f"""#!/usr/bin/env python3
import sys
sys.path.insert(0, {str(SDK_ROOT)!r})
from auto_harness_sdk import ConsultResult, ExtensionServer, ToolResult

srv = ExtensionServer("py-sdk-ext", "Python SDK contract extension")

@srv.tool(
    name="demo_echo",
    description="Echo text via Python SDK",
    parameters={{"type": "object", "properties": {{"text": {{"type": "string"}}}}, "required": ["text"]}},
)
def demo_echo(args, call_id):
    return ToolResult(content="py-sdk-echo:" + args.get("text", ""))

@srv.command(name="pyhello", description="Greet from Python SDK")
def pyhello(args):
    return "hello " + args

@srv.consult("input")
def on_input(params):
    if params.get("text") == "PLEASE_REWRITE":
        return ConsultResult(text="BRANCH rewritten by py-sdk")
    return ConsultResult()

srv.serve()
"""


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


def _wait_tcp(port: int, timeout: float = 5.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                return
        except OSError:
            time.sleep(0.05)
    raise RuntimeError(f"mockllm failed to listen on port {port}")


class PythonSDKContractTests(unittest.TestCase):
    _tmpdir: tempfile.TemporaryDirectory[str]
    _harness_bin: str
    _mockllm_bin: str
    _mockllm_proc: subprocess.Popen[bytes]
    _base_url: str

    @classmethod
    def setUpClass(cls) -> None:
        cls._tmpdir = tempfile.TemporaryDirectory()
        tmp = Path(cls._tmpdir.name)

        cls._harness_bin = os.environ.get("HARNESS_BIN", "")
        cls._mockllm_bin = os.environ.get("MOCKLLM_BIN", "")

        if not cls._harness_bin:
            cls._harness_bin = str(tmp / "harness")
            subprocess.run(
                ["go", "build", "-o", cls._harness_bin, "./cmd/harness"],
                cwd=str(REPO_ROOT),
                check=True,
            )
        if not cls._mockllm_bin:
            cls._mockllm_bin = str(tmp / "mockllm")
            subprocess.run(
                ["go", "build", "-o", cls._mockllm_bin, "./cmd/mockllm"],
                cwd=str(REPO_ROOT),
                check=True,
            )

        port = _free_port()
        cls._mockllm_proc = subprocess.Popen(
            [cls._mockllm_bin, "-addr", f"127.0.0.1:{port}"],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        _wait_tcp(port)
        cls._base_url = f"http://127.0.0.1:{port}/v1"

    @classmethod
    def tearDownClass(cls) -> None:
        if getattr(cls, "_mockllm_proc", None) is not None:
            cls._mockllm_proc.terminate()
            cls._mockllm_proc.wait(timeout=5)
        if getattr(cls, "_tmpdir", None) is not None:
            cls._tmpdir.cleanup()

    def _make_home(self, extension_command: str = "") -> Path:
        home = Path(tempfile.mkdtemp(dir=self._tmpdir.name))
        harness_dir = home / ".harness"
        harness_dir.mkdir(parents=True, exist_ok=True)
        cfg = {
            "provider": "openai",
            "model": "mock-model",
            "base_url": self._base_url,
            "trust_project": True,
            "permissions": {"default": "allow"},
        }
        if extension_command:
            cfg["extension_command"] = extension_command
        (harness_dir / "config.json").write_text(json.dumps(cfg), encoding="utf-8")
        return home

    def test_rpc_client_text_and_multimodal_prompt(self) -> None:
        home = self._make_home()
        workdir = Path(tempfile.mkdtemp(dir=self._tmpdir.name))
        env = {"HOME": str(home), "OPENAI_API_KEY": "test-key"}

        with RPCClient(
            [
                self._harness_bin,
                "-rpc",
                "-no-mcp",
                "-no-git-checkpoints",
                "-no-subagents",
                "-model",
                "mock-model",
                "-base-url",
                self._base_url,
            ],
            cwd=str(workdir),
            env=env,
        ) as client:
            state = client.get_state()
            self.assertEqual(state["schema_version"], SCHEMA_VERSION)
            self.assertEqual(state["model"], "mock-model")

            # Text prompt using BRANCH trigger for deterministic non-tool response.
            res1 = client.prompt("BRANCH hello")
            self.assertEqual(res1.disposition, "started")
            self.assertIn("mockllm branch answer", res1.text)
            self.assertTrue(any(ev.get("type") == "agent_settled" for ev in res1.events))
            self.assertTrue(all(ev.get("schema_version") == SCHEMA_VERSION for ev in res1.events))

            # Multimodal prompt carrying base64 PNG image.
            res2 = client.prompt(
                "describe shot",
                images=[{"data": TINY_PNG_B64, "media_type": "image/png", "label": "dot.png"}],
            )
            self.assertIn("mockllm saw 1 image(s)", res2.text)

            msgs = client.get_messages()
            self.assertEqual(len(msgs), 4)

    def test_extension_server_tool_command_and_consult(self) -> None:
        ext_path = Path(self._tmpdir.name) / "ext_contract.py"
        ext_path.write_text(EXT_SCRIPT, encoding="utf-8")
        home = self._make_home(extension_command=f"{shutil.which('python3') or sys.executable} {ext_path}")
        workdir = Path(tempfile.mkdtemp(dir=self._tmpdir.name))
        env = {"HOME": str(home), "OPENAI_API_KEY": "test-key"}

        with RPCClient(
            [
                self._harness_bin,
                "-rpc",
                "-no-mcp",
                "-no-git-checkpoints",
                "-no-subagents",
            ],
            cwd=str(workdir),
            env=env,
        ) as client:
            commands = client.get_commands()
            names = {c["name"] for c in commands}
            self.assertIn("pyhello", names)

            # event.consult input rewrite via Python ExtensionServer (run before tool history accumulates).
            res1 = client.prompt("PLEASE_REWRITE")
            self.assertIn("mockllm branch answer", res1.text)

            # Tool execution via Python ExtensionServer (mockllm calls demo_echo).
            res2 = client.prompt("run ext tool")
            self.assertIn("py-sdk-echo:hello-from-extension", res2.text)


if __name__ == "__main__":
    unittest.main()
