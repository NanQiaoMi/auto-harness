import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import type { ChildProcess } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { RPCClient, SCHEMA_VERSION } from "../src/index.ts";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const SDK_ROOT = path.resolve(__dirname, "..");
const REPO_ROOT = path.resolve(__dirname, "../../..");

const TINY_PNG_B64 =
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=";

async function getFreePort(): Promise<number> {
  return await new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.listen(0, "127.0.0.1", () => {
      const addr = srv.address();
      if (!addr || typeof addr === "string") {
        srv.close(() => reject(new Error("unexpected address")));
        return;
      }
      const { port } = addr;
      srv.close(() => resolve(port));
    });
    srv.on("error", reject);
  });
}

async function waitTcp(port: number, timeoutMs = 5000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      await new Promise<void>((resolve, reject) => {
        const sock = net.createConnection({ host: "127.0.0.1", port }, () => {
          sock.destroy();
          resolve();
        });
        sock.on("error", reject);
      });
      return;
    } catch {
      await new Promise((r) => setTimeout(r, 50));
    }
  }
  throw new Error(`mockllm failed to listen on port ${port}`);
}

let tmpDir: string;
let harnessBin: string;
let mockllmBin: string;
let mockllmProc: ChildProcess;
let baseUrl: string;

test.before(async () => {
  tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "harness-ts-sdk-"));
  harnessBin = process.env.HARNESS_BIN || path.join(tmpDir, "harness");
  mockllmBin = process.env.MOCKLLM_BIN || path.join(tmpDir, "mockllm");

  if (!process.env.HARNESS_BIN) {
    execFileSync("go", ["build", "-o", harnessBin, "./cmd/harness"], { cwd: REPO_ROOT, stdio: "inherit" });
  }
  if (!process.env.MOCKLLM_BIN) {
    execFileSync("go", ["build", "-o", mockllmBin, "./cmd/mockllm"], { cwd: REPO_ROOT, stdio: "inherit" });
  }

  const port = await getFreePort();
  mockllmProc = spawn(mockllmBin, ["-addr", `127.0.0.1:${port}`], { stdio: "ignore" });
  await waitTcp(port);
  baseUrl = `http://127.0.0.1:${port}/v1`;
});

test.after(async () => {
  if (mockllmProc) {
    mockllmProc.kill();
    await new Promise<void>((resolve) => mockllmProc.once("close", () => resolve()));
  }
  if (tmpDir) {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

function makeHome(extensionCommand = ""): string {
  const home = fs.mkdtempSync(path.join(tmpDir, "home-"));
  const harnessDir = path.join(home, ".harness");
  fs.mkdirSync(harnessDir, { recursive: true });
  const cfg: Record<string, unknown> = {
    provider: "openai",
    model: "mock-model",
    base_url: baseUrl,
    trust_project: true,
    permissions: { default: "allow" },
  };
  if (extensionCommand) {
    cfg.extension_command = extensionCommand;
  }
  fs.writeFileSync(path.join(harnessDir, "config.json"), JSON.stringify(cfg));
  return home;
}

test("RPCClient text and multimodal prompt contract", async () => {
  const home = makeHome();
  const workdir = fs.mkdtempSync(path.join(tmpDir, "work-"));
  const client = new RPCClient(
    [
      harnessBin,
      "-rpc",
      "-no-mcp",
      "-no-git-checkpoints",
      "-no-subagents",
      "-model",
      "mock-model",
      "-base-url",
      baseUrl,
    ],
    {
      cwd: workdir,
      env: { HOME: home, OPENAI_API_KEY: "test-key" },
    },
  );
  client.start();
  try {
    const state = await client.getState();
    assert.equal(state.schema_version, SCHEMA_VERSION);
    assert.equal(state.model, "mock-model");

    const res1 = await client.prompt("BRANCH hello");
    assert.equal(res1.disposition, "started");
    assert.match(res1.text, /mockllm branch answer/);
    assert.ok(res1.events.some((ev) => ev.type === "agent_settled"));
    assert.ok(res1.events.every((ev) => ev.schema_version === SCHEMA_VERSION));

    const res2 = await client.prompt("describe shot", {
      images: [{ data: TINY_PNG_B64, media_type: "image/png", label: "dot.png" }],
    });
    assert.match(res2.text, /mockllm saw 1 image\(s\)/);

    const msgs = await client.getMessages();
    assert.equal(msgs.length, 4);
  } finally {
    await client.close();
  }
});

test("ExtensionServer tool, command and consult contract", async () => {
  const extPath = path.join(tmpDir, "ext_contract.ts");
  const sdkIndex = path.join(SDK_ROOT, "src/index.ts");
  const extSource = `import { ExtensionServer } from ${JSON.stringify(sdkIndex)};

const srv = new ExtensionServer("ts-sdk-ext", "TypeScript SDK contract extension");

srv.registerTool(
  {
    name: "demo_echo",
    description: "Echo text via TS SDK",
    parameters: { type: "object", properties: { text: { type: "string" } }, required: ["text"] },
  },
  (args) => ({ content: "ts-sdk-echo:" + String(args.text ?? "") }),
);

srv.registerCommand({ name: "tshello", description: "Greet from TS SDK" }, (args) => "hello " + args);

srv.onConsult("input", (params) => {
  if (params.text === "PLEASE_REWRITE") {
    return { text: "BRANCH rewritten by ts-sdk" };
  }
  return {};
});

await srv.serve();
`;
  fs.writeFileSync(extPath, extSource, "utf8");

  const home = makeHome(`${process.execPath} --experimental-strip-types ${extPath}`);
  const workdir = fs.mkdtempSync(path.join(tmpDir, "work-ext-"));
  const client = new RPCClient(
    [harnessBin, "-rpc", "-no-mcp", "-no-git-checkpoints", "-no-subagents"],
    {
      cwd: workdir,
      env: { HOME: home, OPENAI_API_KEY: "test-key" },
    },
  );
  client.start();
  try {
    const commands = await client.getCommands();
    const names = new Set(commands.map((c) => String(c.name)));
    assert.ok(names.has("tshello"));

    const res1 = await client.prompt("PLEASE_REWRITE");
    assert.match(res1.text, /mockllm branch answer/);

    const res2 = await client.prompt("run ext tool");
    assert.match(res2.text, /ts-sdk-echo:hello-from-extension/);
  } finally {
    await client.close();
  }
});
