// PRD #1156 M3a — a localhost fake Responses provider for the credential-free
// lifecycle/isolation suites.
//
// It is the m3 analogue of the frozen M0 fixture's `/v1/responses` server
// (`e2e/codex-m0/harness.mjs`), rebuilt here (NOT imported — only the frozen PURE
// protocol helpers `message`/`callOutput` are reused) so these suites never
// instantiate `Probe`/`SupervisorProbe` or inject `config:{bypass_hook_trust:true}`.
// It binds ONLY 127.0.0.1 (loopback works under `--network none`), authenticates a
// RUNTIME-ASSEMBLED dummy bearer token (no baked/real secret), and streams a fixed,
// deterministic SSE response the caller shapes via `respond`. No model decides
// anything: the fixture emits harmless tools and the test checks their real effects.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { randomBytes } from "node:crypto";

import { message } from "../codex-m0/harness.mjs";

const DEADLINE_MS = 15_000;
const MAX_BYTES = 4 * 1024 * 1024;

/** A single Responses output item (custom_tool_call cell, message, …). Kept `unknown`
 *  at the boundary: the caller builds the exact frozen-shape literals. */
export type ResponseItem = Record<string, unknown>;

/** The request body the app-server POSTs to `/v1/responses` (the parsed JSON). */
export type ResponsesBody = { readonly model?: unknown; readonly input?: readonly unknown[] } & Record<string, unknown>;

export interface FakeProviderOptions {
  /** The exact bearer credential the app-server must present (assembled at runtime). */
  readonly credential: string;
  /** Produce the Responses output items for one request (mirrors M0's `respond`). */
  readonly respond: (body: ResponsesBody, provider: FakeProvider) => ResponseItem[] | Promise<ResponseItem[]>;
}

/** A runtime-assembled dummy bearer token — secret-SHAPED, never a real/baked value. */
export function dummyCredential(): string {
  return `sk-m3a-${randomBytes(18).toString("hex")}`;
}

/** SSE framing for one Responses turn (created → output items → completed). The frozen
 *  `sse` helper is not exported by the M0 harness, so it is reproduced here verbatim. */
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

  /** The base URL the config's `[model_providers.*] base_url` points Codex at. */
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
    const timeout = setTimeout(() => response.destroy(new Error("m3a request deadline")), DEADLINE_MS);
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
      if (Buffer.byteLength(raw) > MAX_BYTES) request.destroy(new Error("m3a request too large"));
    });
    request.on("end", () => {
      void (async () => {
        try {
          if (request.headers.authorization !== `Bearer ${this.credential}`) {
            throw new Error(`unauthenticated request (authorization=${String(request.headers.authorization)})`);
          }
          const body = JSON.parse(raw) as ResponsesBody;
          this.requests.push(body);
          const items = await this.responder(body, this);
          if (response.destroyed) return;
          const data = sse(`m3a-response-${this.requests.length}`, items);
          response.writeHead(200, { "content-type": "text/event-stream", "content-length": Buffer.byteLength(data) });
          response.end(data);
        } catch (error) {
          this.errors.push(error instanceof Error ? error.message : String(error));
          if (!response.destroyed) response.writeHead(500).end(JSON.stringify({ error: { message: "m3a fixture error" } }));
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

/** A trivial always-finish responder (control B's turn: no tool call, just a message). */
export function trivialResponder(): (body: ResponsesBody) => ResponseItem[] {
  return () => [message() as ResponseItem];
}
