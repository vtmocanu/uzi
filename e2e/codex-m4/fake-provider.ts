// PRD #1287 C1 — a loopback fake Responses provider for the credential-free real-binary P
// smoke. Adapted from e2e/codex-m3b/fake-provider.ts: it binds ONLY 127.0.0.1 on an EPHEMERAL
// port (so overlapping/parallel gate runs never collide), authenticates an exact runtime dummy
// bearer, and streams fixed deterministic SSE. No model decides anything. Runtime canaries are
// assembled with randomBytes so no literal secret is committed (gitleaks stays green).
//
// The smoke uses the app-server api_key auth mode, so the REAL pinned Codex presents the api key
// as the provider bearer; this fake authenticates that exact value (`credential` mode). The
// request-inspection helpers (countToolCallbacks / toolCallbackTexts) prove the broker executed
// a callback and its RESULT was fed back to the model — not merely that a frame was emitted.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { randomBytes } from "node:crypto";

import { message, tool } from "../codex-m0/harness.mjs";

const DEADLINE_MS = 15_000;
const MAX_BYTES = 4 * 1024 * 1024;

/** A single Responses output item (function_call, message, …). */
export type ResponseItem = Record<string, unknown>;

/** The parsed `/v1/responses` request body the app-server POSTs. */
export type ResponsesBody = { readonly model?: unknown; readonly input?: readonly unknown[] } & Record<string, unknown>;

export interface FakeProviderOptions {
  /** The exact bearer credential the app-server must present (assembled at runtime). */
  readonly credential: string;
  /** Produce the Responses output items for one request (mirrors M0's `respond`). */
  readonly respond: (body: ResponsesBody, provider: FakeProvider) => ResponseItem[] | Promise<ResponseItem[]>;
}

/** A runtime-assembled dummy bearer token — secret-SHAPED, never a real/baked value, and
 *  comfortably above the run redactor's 8-char floor. */
export function dummyCredential(): string {
  return `sk-m4-${randomBytes(18).toString("hex")}`;
}

/** A runtime secret-shaped canary for a model-chosen tool argument (a Bash command echoes it).
 *  It flows to the command surface as the model's own legitimate argument. */
export function bashArgCanary(): string {
  return `codex-m4-bash-arg-${randomBytes(12).toString("hex")}`;
}

/** SSE framing for one Responses turn (created → output items → completed). The frozen `sse`
 *  helper is not exported by the M0 harness, so it is reproduced here (as m3a/m3b do). */
function sse(id: string, items: ResponseItem[]): string {
  const frames = [
    { type: "response.created", response: { id } },
    ...items.map((item) => ({ type: "response.output_item.done", item })),
    {
      type: "response.completed",
      response: {
        id,
        usage: {
          input_tokens: 0, output_tokens: 0, total_tokens: 0,
          input_tokens_details: null, output_tokens_details: null,
        },
      },
    },
  ];
  return frames.map((frame) => `event: ${frame.type}\ndata: ${JSON.stringify(frame)}\n\n`).join("");
}

export class FakeProvider {
  readonly requests: ResponsesBody[] = [];
  readonly errors: string[] = [];
  private closing = false;

  private constructor(
    private readonly server: Server,
    readonly port: number,
    private readonly credential: string,
    private readonly responder: (body: ResponsesBody, provider: FakeProvider) => ResponseItem[] | Promise<ResponseItem[]>,
  ) {}

  /** The base URL the loopback config's `base_url` points Codex at. Satisfies
   *  buildCodexLoopbackTestConfigToml's strict `http://127.0.0.1:<port>/v1` validation. */
  get baseUrl(): string {
    return `http://127.0.0.1:${this.port}/v1`;
  }

  static async start(options: FakeProviderOptions): Promise<FakeProvider> {
    const server = createServer();
    const holder = { provider: undefined as FakeProvider | undefined };
    server.on("request", (request, response) => holder.provider?.handle(request, response));
    server.requestTimeout = DEADLINE_MS;
    server.headersTimeout = DEADLINE_MS;
    await new Promise<void>((resolve, reject) => {
      server.once("error", reject);
      server.listen(0, "127.0.0.1", resolve);
    });
    const address = server.address();
    if (address === null || typeof address === "string" || address.address !== "127.0.0.1") {
      throw new Error("fake provider must bind 127.0.0.1");
    }
    const provider = new FakeProvider(server, address.port, options.credential, options.respond);
    holder.provider = provider;
    return provider;
  }

  private handle(request: IncomingMessage, response: ServerResponse): void {
    const timeout = setTimeout(() => response.destroy(new Error("m4 request deadline")), DEADLINE_MS);
    response.on("close", () => clearTimeout(timeout));
    request.on("error", (error) => { if (!this.closing) this.errors.push(error.message); });
    if (request.method !== "POST" || request.url !== "/v1/responses") {
      request.resume();
      response.writeHead(404).end();
      return;
    }
    let raw = "";
    request.setEncoding("utf8");
    request.on("data", (chunk: string) => {
      raw += chunk;
      if (Buffer.byteLength(raw) > MAX_BYTES) request.destroy(new Error("m4 request too large"));
    });
    request.on("end", () => {
      void (async () => {
        try {
          const auth = request.headers.authorization;
          const bearer = typeof auth === "string" && auth.startsWith("Bearer ") ? auth.slice("Bearer ".length) : undefined;
          if (bearer === undefined || bearer !== this.credential) {
            throw new Error("unauthenticated request (bearer did not match the runtime dummy credential)");
          }
          const body = JSON.parse(raw) as ResponsesBody;
          this.requests.push(body);
          const items = await this.responder(body, this);
          if (response.destroyed) return;
          const data = sse(`m4-response-${this.requests.length}`, items);
          response.writeHead(200, { "content-type": "text/event-stream", "content-length": Buffer.byteLength(data) });
          response.end(data);
        } catch (error) {
          this.errors.push(error instanceof Error ? error.message : String(error));
          if (!response.destroyed) response.writeHead(500).end(JSON.stringify({ error: { message: "m4 fixture error" } }));
        }
      })();
    });
  }

  async close(): Promise<void> {
    this.closing = true;
    this.server.closeAllConnections();
    await new Promise<void>((resolve) => this.server.close(() => resolve()));
  }
}

/** The call id the scripted responder uses for its single allowed Bash callback. */
export const SMOKE_BASH_CALL_ID = "m4-smoke-bash";

/**
 * The smoke responder: the FIRST request emits ONE allowed `uzi_bash` function_call (echoing
 * `bashArg`), and every subsequent request emits a finish message so the turn completes once the
 * tool result comes back. The wire tool name is `uzi_bash` — the collision-free dynamic-tool name
 * the broker maps back to canonical `Bash` (broker.ts CODEX_DYNAMIC_WIRE_NAMES).
 */
export function scriptedAllowedBashResponder(bashArg: string): (body: ResponsesBody) => ResponseItem[] {
  let emittedTool = false;
  return (): ResponseItem[] => {
    if (!emittedTool) {
      emittedTool = true;
      return [tool(SMOKE_BASH_CALL_ID, "uzi_bash", { command: `echo ${bashArg}` }) as ResponseItem];
    }
    return [message("m4 smoke finished") as ResponseItem];
  };
}

/** Count the tool-call REPLIES (function/custom tool-call OUTPUT items) fed back across every
 *  recorded request — provider-visible evidence the broker executed a tool and returned its
 *  result to Codex. */
export function countToolCallbacks(requests: readonly ResponsesBody[]): number {
  let n = 0;
  for (const body of requests) {
    const input = Array.isArray(body.input) ? body.input : [];
    for (const item of input) {
      if (item !== null && typeof item === "object") {
        const t = (item as { type?: unknown }).type;
        if (t === "function_call_output" || t === "custom_tool_call_output") n += 1;
      }
    }
  }
  return n;
}

/** Text returned to one provider-issued dynamic callback, observed in a later Responses
 *  request. Distinguishes "the fixture emitted a call" from "the app-server delivered it to the
 *  worker and fed the worker's result back". */
export function toolCallbackTexts(requests: readonly ResponsesBody[], callId: string): string[] {
  const texts: string[] = [];
  for (const body of requests) {
    const input = Array.isArray(body.input) ? body.input : [];
    for (const item of input) {
      if (item === null || typeof item !== "object") continue;
      const record = item as Record<string, unknown>;
      if (
        (record.type !== "function_call_output" && record.type !== "custom_tool_call_output")
        || record.call_id !== callId
      ) continue;
      if (typeof record.output === "string") {
        texts.push(record.output);
        continue;
      }
      if (!Array.isArray(record.output)) continue;
      for (const content of record.output) {
        if (content === null || typeof content !== "object") continue;
        const text = (content as Record<string, unknown>).text;
        if (typeof text === "string") texts.push(text);
      }
    }
  }
  return texts;
}
