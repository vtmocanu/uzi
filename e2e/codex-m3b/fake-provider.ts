// PRD #1171 m5 — a localhost fake Responses provider for the credential-free packaged
// CodexExecutor lifecycle proof.
//
// It is the m3b analogue of the m3a loopback fake (`e2e/codex-m3a/fake-provider.ts`) and,
// like it, the m3 rebuild of the frozen M0 fixture's `/v1/responses` server (only the frozen
// PURE protocol helpers `message`/`callOutput` are reused, NOT `Probe`/`SupervisorProbe`).
// It binds ONLY 127.0.0.1 (loopback works under `--network none`), authenticates a
// RUNTIME-ASSEMBLED dummy bearer token (no baked/real secret), and streams a fixed,
// deterministic SSE response the caller shapes via `respond`. No model decides anything.
//
// It is used ONLY by lifecycle.test.ts's IMAGE leg (the real supervisor + real Codex talk to
// it). The host node --test leg drives the packaged CodexExecutor through an in-memory
// transport instead, so it never binds this server. What the two legs SHARE is the canary
// set below: `codexCanaries()` assembles the secret-shaped credential/capability the fake
// WorkerClient injects PLUS the secret-shaped tool-argument canaries this provider emits, so
// both legs assert against the SAME shapes.
//
// 🔴 MAINTAINER-VERIFIED: the exact Responses `item.type` / tool-call framing the real Codex
// app-server accepts is UNCONFIRMED in code (agent/src/codex/codex-harness.ts marks the item
// types "provisional … MUST be verified in the packaged integration (m3b:packaged)"). The
// responders below are the current best model; a maintainer confirms them when the docker leg
// first runs. The host leg — which is what CI/in-worker validation exercises — does not depend
// on them.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { randomBytes } from "node:crypto";

import { message, patchTool, tool } from "../codex-m0/harness.mjs";

const DEADLINE_MS = 15_000;
const MAX_BYTES = 4 * 1024 * 1024;

/** A single Responses output item (custom_tool_call cell, message, …). Kept `unknown` at the
 *  boundary: the caller builds the exact frozen-shape literals. */
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
  return `sk-m3b-${randomBytes(18).toString("hex")}`;
}

/**
 * The shared canary set both legs assert against. Every value is assembled at RUNTIME (no
 * secret literal sits in the source) and is comfortably above the run redactor's 8-char floor.
 *
 *  - `credential` / `capability`: the INJECTED credential the fake WorkerClient releases and
 *    the claim binding's capability. These MUST NOT appear in any public message, any
 *    provider-visible request, any log line, or the command-root state.
 *  - `bashArg` / `patchArg` / `spawnArg` / `planArg`: secret-shaped strings the provider puts
 *    inside model-supplied TOOL ARGUMENTS. They model untrusted model output; they flow to
 *    their own effect (a Bash arg reaches the command surface) but are distinct from the
 *    credential/capability, so a canary appearing in command state is only a real leak when it
 *    is the credential/capability, never a tool arg the model itself chose.
 */
export interface CodexCanaries {
  readonly credential: string;
  readonly capability: string;
  readonly bashArg: string;
  readonly patchArg: string;
  readonly spawnArg: string;
  readonly planArg: string;
}

export function codexCanaries(): CodexCanaries {
  const nonce = (): string => randomBytes(12).toString("hex");
  return {
    credential: `sk-codex-cred-canary-${nonce()}`,
    capability: `codex-cap-canary-${nonce()}`,
    bashArg: `codex-bash-arg-canary-${nonce()}`,
    patchArg: `codex-patch-arg-canary-${nonce()}`,
    spawnArg: `codex-spawn-arg-canary-${nonce()}`,
    planArg: `codex-plan-arg-canary-${nonce()}`,
  };
}

/** SSE framing for one Responses turn (created → output items → completed). The frozen `sse`
 *  helper is not exported by the M0 harness, so it is reproduced here verbatim (as m3a does). */
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
    const timeout = setTimeout(() => response.destroy(new Error("m3b request deadline")), DEADLINE_MS);
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
      if (Buffer.byteLength(raw) > MAX_BYTES) request.destroy(new Error("m3b request too large"));
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
          const data = sse(`m3b-response-${this.requests.length}`, items);
          response.writeHead(200, { "content-type": "text/event-stream", "content-length": Buffer.byteLength(data) });
          response.end(data);
        } catch (error) {
          this.errors.push(error instanceof Error ? error.message : String(error));
          if (!response.destroyed) response.writeHead(500).end(JSON.stringify({ error: { message: "m3b fixture error" } }));
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

/**
 * The IMAGE-leg lifecycle responder: a fixed, deterministic tool sequence the real Codex
 * app-server replays as the packaged CodexExecutor drives one implement turn. Each step's
 * argument carries the matching secret-shaped canary from {@link CodexCanaries}. Advances by
 * request count (Codex re-POSTs `/v1/responses` after each tool result, so the count is the
 * step index): Bash → apply_patch → spawn_agent (synchronous subagent) → submit_plan (a ROOT
 * signal, denied-when-non-root by the broker, latched-but-inert in m3) → finish message.
 */
export function lifecycleResponder(canaries: CodexCanaries): (body: ResponsesBody, provider: FakeProvider) => ResponseItem[] {
  return (_body, provider) => {
    const step = provider.requests.length; // 1-based: this request is the Nth
    switch (step) {
      case 1:
        return [tool("cc-bash", "Bash", { command: `echo ${canaries.bashArg}` }) as ResponseItem];
      case 2: {
        // patchTool's FROZEN filename constraint is /^[a-z-]+$/, so map the canary to a
        // filename-safe form (the marker file only proves the patch effect ran; the
        // credential/capability boundary is what the suite asserts). [#1171 m5 review]
        const patchName = canaries.patchArg.replace(/[^a-z-]+/g, "-");
        return [patchTool("cc-patch", patchName) as ResponseItem];
      }
      case 3:
        return [tool("cc-spawn", "spawn_agent", { subagent_type: "coder", prompt: canaries.spawnArg }) as ResponseItem];
      case 4:
        return [tool("cc-plan", "submit_plan", { plan: canaries.planArg }) as ResponseItem];
      default:
        return [message("m3b lifecycle finished") as ResponseItem];
    }
  };
}

/** A trivial always-finish responder (a turn with no tool call, just a message). */
export function trivialResponder(): (body: ResponsesBody) => ResponseItem[] {
  return () => [message() as ResponseItem];
}
