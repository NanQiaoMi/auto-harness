import { spawn } from "node:child_process";
import type { ChildProcessWithoutNullStreams } from "node:child_process";
import { SCHEMA_VERSION } from "./types.ts";
import type { ImageInput, PromptResult, RPCEvent, RPCResponse } from "./types.ts";

export class RPCError extends Error {
  readonly command: string;

  constructor(command: string, message: string) {
    super(`rpc ${command} failed: ${message}`);
    this.name = "RPCError";
    this.command = command;
  }
}

export interface RPCClientOptions {
  cwd?: string;
  env?: Record<string, string | undefined>;
}

interface PendingRequest {
  resolve: (resp: RPCResponse) => void;
  reject: (err: Error) => void;
}

/**
 * Drives an `auto-harness` `-rpc` subprocess over strict LF-only JSONL.
 */
export class RPCClient {
  private readonly command: string[];
  private readonly options: RPCClientOptions;
  private proc: ChildProcessWithoutNullStreams | null = null;
  private seq = 0;
  private closed = false;
  private buffer: Buffer = Buffer.alloc(0);
  private readonly pending = new Map<string, PendingRequest>();
  private readonly listeners = new Set<(ev: RPCEvent) => void>();

  constructor(command: string[], options: RPCClientOptions = {}) {
    this.command = [...command];
    this.options = options;
  }

  start(): void {
    if (this.proc) {
      return;
    }
    const [bin, ...args] = this.command;
    const procEnv: NodeJS.ProcessEnv = { ...process.env, ...this.options.env };
    this.proc = spawn(bin, args, {
      cwd: this.options.cwd,
      env: procEnv,
      stdio: ["pipe", "pipe", "pipe"],
    });

    this.proc.stdout.on("data", (chunk: Buffer) => {
      this.buffer = Buffer.concat([this.buffer, chunk]);
      this.drainLines();
    });
    this.proc.on("close", () => {
      const err = new Error("RPC stream closed");
      for (const [id, req] of this.pending.entries()) {
        this.pending.delete(id);
        req.reject(err);
      }
    });
  }

  private drainLines(): void {
    while (true) {
      const idx = this.buffer.indexOf(0x0a);
      if (idx < 0) {
        return;
      }
      let slice = this.buffer.subarray(0, idx);
      this.buffer = this.buffer.subarray(idx + 1);
      if (slice.length > 0 && slice[slice.length - 1] === 0x0d) {
        slice = slice.subarray(0, slice.length - 1);
      }
      const line = slice.toString("utf8").trim();
      if (!line) {
        continue;
      }
      let record: Record<string, unknown>;
      try {
        record = JSON.parse(line) as Record<string, unknown>;
      } catch {
        continue;
      }
      if (record.type === "response") {
        const resp = record as unknown as RPCResponse;
        const req = resp.id ? this.pending.get(resp.id) : undefined;
        if (req && resp.id) {
          this.pending.delete(resp.id);
          req.resolve(resp);
        }
      } else {
        const ev = record as unknown as RPCEvent;
        for (const fn of this.listeners) {
          fn(ev);
        }
      }
    }
  }

  async call<T = Record<string, unknown>>(
    type: string,
    fields: Record<string, unknown> = {},
    timeoutMs = 30_000,
  ): Promise<RPCResponse<T>> {
    if (!this.proc) {
      throw new Error("RPCClient has not been started");
    }
    if (this.closed) {
      throw new Error("RPCClient is closed");
    }
    this.seq += 1;
    const id = `ts-${this.seq}`;
    const payload = {
      schema_version: SCHEMA_VERSION,
      id,
      type,
      ...fields,
    };
    const wire = JSON.stringify(payload) + "\n";

    const resp = await new Promise<RPCResponse>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error(`RPC call ${type} timed out after ${timeoutMs}ms`));
      }, timeoutMs);
      this.pending.set(id, {
        resolve: (r) => {
          clearTimeout(timer);
          resolve(r);
        },
        reject: (err) => {
          clearTimeout(timer);
          reject(err);
        },
      });
      this.proc!.stdin.write(wire, "utf8");
    });

    if (!resp.success) {
      throw new RPCError(type, resp.error ?? "unknown error");
    }
    return resp as RPCResponse<T>;
  }

  async prompt(
    message: string,
    options: {
      images?: ImageInput[];
      onEvent?: (ev: RPCEvent) => void;
      timeoutMs?: number;
    } = {},
  ): Promise<PromptResult> {
    const timeoutMs = options.timeoutMs ?? 60_000;
    const events: RPCEvent[] = [];
    let deltaText = "";
    let finalText = "";

    let settleResolve!: (res: PromptResult) => void;
    let settleReject!: (err: Error) => void;
    const settledPromise = new Promise<PromptResult>((resolve, reject) => {
      settleResolve = resolve;
      settleReject = reject;
    });
    let disposition = "";
    const timer = setTimeout(() => {
      this.listeners.delete(listener);
      settleReject(new Error(`prompt timed out after ${timeoutMs}ms`));
    }, timeoutMs);

    const listener = (ev: RPCEvent): void => {
      events.push(ev);
      if (options.onEvent) {
        options.onEvent(ev);
      }
      const data = ev.data ?? {};
      if (ev.type === "message_update" && !data.thinking && typeof data.text === "string") {
        deltaText += data.text;
      } else if (ev.type === "message_end") {
        const msg = data.message as Record<string, unknown> | undefined;
        if (msg && msg.role === "assistant" && typeof msg.content === "string" && msg.content) {
          finalText = msg.content;
        }
      } else if (ev.type === "agent_settled") {
        clearTimeout(timer);
        this.listeners.delete(listener);
        settleResolve({
          disposition,
          text: finalText || deltaText,
          aborted: Boolean(data.aborted),
          events,
        });
      }
    };

    this.listeners.add(listener);
    try {
      const fields: Record<string, unknown> = { message };
      if (options.images && options.images.length > 0) {
        fields.images = options.images;
      }
      const resp = await this.call<{ disposition?: string }>("prompt", fields, timeoutMs);
      disposition = resp.data?.disposition ?? "";
      return await settledPromise;
    } catch (err) {
      clearTimeout(timer);
      this.listeners.delete(listener);
      throw err;
    }
  }

  async getState(): Promise<Record<string, unknown>> {
    const resp = await this.call<Record<string, unknown>>("get_state");
    return resp.data ?? {};
  }

  async getMessages(): Promise<Array<Record<string, unknown>>> {
    const resp = await this.call<{ messages?: Array<Record<string, unknown>> }>("get_messages");
    return resp.data?.messages ?? [];
  }

  async getCommands(): Promise<Array<Record<string, unknown>>> {
    const resp = await this.call<{ commands?: Array<Record<string, unknown>> }>("get_commands");
    return resp.data?.commands ?? [];
  }

  async abort(): Promise<void> {
    await this.call("abort");
  }

  async close(): Promise<number> {
    if (!this.proc || this.closed) {
      return 0;
    }
    this.closed = true;
    const proc = this.proc;
    return await new Promise<number>((resolve) => {
      proc.once("close", (code) => resolve(code ?? 0));
      proc.stdin.end();
    });
  }
}
