import { SCHEMA_VERSION } from "./types.ts";
import type { ConsultResult, ManifestCommand, ManifestTool, ToolResult } from "./types.ts";

export type ToolHandler = (
  args: Record<string, unknown>,
  callId: string,
) => ToolResult | Promise<ToolResult>;

export type CommandHandler = (args: string) => string | Promise<string>;

export type ConsultHandler = (
  params: Record<string, unknown>,
) => ConsultResult | Promise<ConsultResult>;

function validateSchemaVersion(version: unknown): void {
  if (version === undefined || version === null || version === "") {
    return;
  }
  if (typeof version === "string" && (version === "1" || version.startsWith("1."))) {
    return;
  }
  throw new Error(`unsupported schema_version ${JSON.stringify(version)} (supported major version: 1)`);
}

/**
 * Implements the auto-harness stdio JSON-RPC extension protocol in TypeScript/Node.
 */
export class ExtensionServer {
  private readonly name: string;
  private readonly description: string;
  private readonly tools: ManifestTool[] = [];
  private readonly toolHandlers = new Map<string, ToolHandler>();
  private readonly commands: ManifestCommand[] = [];
  private readonly commandHandlers = new Map<string, CommandHandler>();
  private readonly consults: string[] = [];
  private readonly consultHandlers = new Map<string, ConsultHandler>();
  private out: NodeJS.WritableStream = process.stdout;

  constructor(name: string, description = "") {
    this.name = name;
    this.description = description;
  }

  registerTool(spec: ManifestTool, handler: ToolHandler): this {
    this.tools.push({
      ...spec,
      parameters: spec.parameters ?? { type: "object", properties: {} },
    });
    this.toolHandlers.set(spec.name, handler);
    return this;
  }

  registerCommand(spec: ManifestCommand, handler: CommandHandler): this {
    this.commands.push(spec);
    this.commandHandlers.set(spec.name, handler);
    return this;
  }

  onConsult(event: string, handler: ConsultHandler): this {
    if (!this.consultHandlers.has(event)) {
      this.consults.push(event);
    }
    this.consultHandlers.set(event, handler);
    return this;
  }

  emitEvent(event: string, text = "", data?: Record<string, unknown>): void {
    const params: Record<string, unknown> = { event };
    if (text) {
      params.text = text;
    }
    if (data !== undefined) {
      params.data = data;
    }
    this.write({ schema_version: SCHEMA_VERSION, method: "event", params });
  }

  async serve(
    stdin: NodeJS.ReadableStream = process.stdin,
    stdout: NodeJS.WritableStream = process.stdout,
  ): Promise<void> {
    this.out = stdout;
    let buf = Buffer.alloc(0);

    for await (const chunk of stdin) {
      const b = typeof chunk === "string" ? Buffer.from(chunk, "utf8") : Buffer.from(chunk);
      buf = Buffer.concat([buf, b]);
      while (true) {
        const idx = buf.indexOf(0x0a);
        if (idx < 0) {
          break;
        }
        let slice = buf.subarray(0, idx);
        buf = buf.subarray(idx + 1);
        if (slice.length > 0 && slice[slice.length - 1] === 0x0d) {
          slice = slice.subarray(0, slice.length - 1);
        }
        const line = slice.toString("utf8").trim();
        if (!line) {
          continue;
        }
        const shouldExit = await this.handleLine(line);
        if (shouldExit) {
          return;
        }
      }
    }
  }

  private async handleLine(line: string): Promise<boolean> {
    let reqId: number | undefined;
    try {
      const req = JSON.parse(line) as {
        schema_version?: unknown;
        id?: number;
        method?: string;
        params?: Record<string, unknown>;
      };
      reqId = req.id;
      validateSchemaVersion(req.schema_version);
      const method = req.method ?? "";
      const params = req.params ?? {};

      if (method === "shutdown") {
        return true;
      }
      let result: unknown;
      if (method === "manifest") {
        result = this.manifest();
      } else if (method === "tool.execute") {
        const name = String(params.name ?? "");
        const handler = this.toolHandlers.get(name);
        if (!handler) {
          result = { content: `unknown tool ${name}`, is_error: true };
        } else {
          result = await handler((params.arguments as Record<string, unknown>) ?? {}, String(params.tool_call_id ?? ""));
        }
      } else if (method === "command.execute") {
        const name = String(params.name ?? "");
        const handler = this.commandHandlers.get(name);
        if (!handler) {
          if (reqId !== undefined) {
            this.writeError(reqId, -32601, `unknown command ${name}`);
          }
          return false;
        }
        result = { text: await handler(String(params.args ?? "")) };
      } else if (method === "event.consult") {
        const event = String(params.event ?? "");
        const handler = this.consultHandlers.get(event);
        result = handler ? await handler(params) : {};
      } else {
        if (reqId !== undefined) {
          this.writeError(reqId, -32601, `unknown method ${method}`);
        }
        return false;
      }
      if (reqId !== undefined) {
        this.write({ schema_version: SCHEMA_VERSION, id: reqId, result });
      }
    } catch (err) {
      if (reqId !== undefined) {
        this.writeError(reqId, -32000, err instanceof Error ? err.message : String(err));
      }
    }
    return false;
  }

  private manifest(): Record<string, unknown> {
    const out: Record<string, unknown> = {
      schema_version: SCHEMA_VERSION,
      name: this.name,
    };
    if (this.description) {
      out.description = this.description;
    }
    if (this.tools.length > 0) {
      out.tools = this.tools;
    }
    if (this.commands.length > 0) {
      out.commands = this.commands;
    }
    if (this.consults.length > 0) {
      out.consults = this.consults;
    }
    return out;
  }

  private writeError(id: number, code: number, message: string): void {
    this.write({ schema_version: SCHEMA_VERSION, id, error: { code, message } });
  }

  private write(record: Record<string, unknown>): void {
    this.out.write(JSON.stringify(record) + "\n");
  }
}
