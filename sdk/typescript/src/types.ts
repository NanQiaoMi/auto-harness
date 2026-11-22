export const SCHEMA_VERSION = "1.0";

export interface ImageInput {
  path?: string;
  data?: string;
  media_type?: "image/png" | "image/jpeg" | "image/webp" | "image/gif";
  label?: string;
}

export interface RPCResponse<T = Record<string, unknown>> {
  schema_version?: string;
  id?: string;
  type: "response";
  command: string;
  success: boolean;
  data?: T;
  error?: string;
}

export interface RPCEvent {
  schema_version?: string;
  type: string;
  data?: Record<string, unknown>;
}

export interface PromptResult {
  disposition: string;
  text: string;
  aborted: boolean;
  events: RPCEvent[];
}

export interface ToolResult {
  content: string;
  is_error?: boolean;
}

export interface ConsultResult {
  block?: boolean;
  reason?: string;
  cancel?: boolean;
  text?: string;
}

export interface ManifestTool {
  name: string;
  label?: string;
  description: string;
  parameters?: Record<string, unknown>;
  retryable?: boolean;
}

export interface ManifestCommand {
  name: string;
  description?: string;
}
