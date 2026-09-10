// PRD #1171 m5 — a localhost fake Responses provider for the credential-free packaged
// CodexExecutor lifecycle proof.
//
// It is the m3b analogue of the m3a loopback fake (`e2e/codex-m3a/fake-provider.ts`) and,
// like it, the m3 rebuild of the frozen M0 fixture's `/v1/responses` server (only the frozen
// PURE protocol helpers `message`/`callOutput` are reused, NOT `Probe`/`SupervisorProbe`).
// It binds ONLY 127.0.0.1 (loopback works under `--network none`), authenticates either an
// exact runtime dummy credential or a digest recorded from a successful WorkerClient
// release/refresh, and streams a fixed deterministic SSE response. No model decides anything.
//
// It is used ONLY by lifecycle.test.ts's IMAGE leg (the real supervisor + real Codex talk to
// it). The host node --test leg drives the packaged CodexExecutor through an in-memory
// transport instead, so it never binds this server. What the two legs SHARE is the canary
// set below: `codexCanaries()` assembles the secret-shaped credential/capability the fake
// WorkerClient injects PLUS the secret-shaped tool-argument canaries this provider emits, so
// both legs assert against the SAME shapes.
//
// VERIFIED 2026-09-10: the exact response/tool framing and parent/child interleaving below
// completed through pinned Codex 0.153.2 in both native AMD64 worker images. The host leg
// remains independent and uses an in-memory transport.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { createHash, randomBytes } from "node:crypto";

import { message, tool } from "../codex-m0/harness.mjs";

const DEADLINE_MS = 15_000;
const MAX_BYTES = 4 * 1024 * 1024;

/** A single Responses output item (custom_tool_call cell, message, …). Kept `unknown` at the
 *  boundary: the caller builds the exact frozen-shape literals. */
export type ResponseItem = Record<string, unknown>;

/** The request body the app-server POSTs to `/v1/responses` (the parsed JSON). */
export type ResponsesBody = { readonly model?: unknown; readonly input?: readonly unknown[] } & Record<string, unknown>;

export interface FakeProviderOptions {
  /** The exact bearer credential the app-server must present (assembled at runtime). */
  readonly credential?: string;
  /** SHA-256 digests of credentials successfully returned by the WorkerClient. The mutable
   *  set is populated before Codex can present each release/refresh result, so the real-server
   *  path binds provider authentication without putting raw credentials in a contract or log. */
  readonly allowedBearerDigests?: ReadonlySet<string>;
  /** Produce the Responses output items for one request (mirrors M0's `respond`). */
  readonly respond: (body: ResponsesBody, provider: FakeProvider) => ResponseItem[] | Promise<ResponseItem[]>;
}

/** A runtime-assembled dummy bearer token — secret-SHAPED, never a real/baked value. */
export function dummyCredential(): string {
  return `sk-m3b-${randomBytes(18).toString("hex")}`;
}

/** One-way identity used to bind provider requests to WorkerClient release/refresh results. */
export function bearerDigest(credential: string): string {
  return createHash("sha256").update(credential).digest("hex");
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
  /** m6 (item 7): every bearer token presented on a `/v1/responses` request, in order and
   *  stripped of the `Bearer ` prefix. On the real-server path the RELEASED credential
   *  (which the test never knows ahead of time) lands here, so the credential-canary
   *  boundary is asserted against the token the real Codex actually presented. Only an
   *  authenticated bearer is recorded. */
  readonly observedBearers: string[] = [];
  private closing = false;
  private constructor(
    private readonly server: Server,
    readonly port: number,
    private readonly credential: string | undefined,
    private readonly allowedBearerDigests: ReadonlySet<string> | undefined,
    private readonly responder: (body: ResponsesBody, provider: FakeProvider) => ResponseItem[] | Promise<ResponseItem[]>,
  ) {}

  /** The base URL the config's `[model_providers.*] base_url` points Codex at. */
  get baseUrl(): string {
    return `http://127.0.0.1:${this.port}/v1`;
  }

  static async start(options: FakeProviderOptions): Promise<FakeProvider> {
    if ((options.credential === undefined) === (options.allowedBearerDigests === undefined)) {
      throw new Error("fake provider requires exactly one bearer authentication mode");
    }
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
    const provider = new FakeProvider(server, address.port, options.credential, options.allowedBearerDigests, options.respond);
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
          const auth = request.headers.authorization;
          const bearer = typeof auth === "string" && auth.startsWith("Bearer ") ? auth.slice("Bearer ".length) : undefined;
          if (bearer === undefined || bearer.length === 0) {
            throw new Error("unauthenticated request (no bearer token presented)");
          }
          const allowed = this.allowedBearerDigests === undefined
            ? this.credential !== undefined && bearer === this.credential
            : this.allowedBearerDigests.has(bearerDigest(bearer));
          if (!allowed) {
            throw new Error("unauthenticated request (bearer was not released by the worker client)");
          }
          this.observedBearers.push(bearer);
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
 * PRD #1171 m6 (item 8): the machine-recorded evidence the packaged Block-B lifecycle proof
 * derives its real-path counts from. Every field is what the fake provider OBSERVED the
 * packaged executor drive — the responder self-records each workflow stage it emitted, so the
 * counts are provider-visible evidence, not the executor's own bookkeeping. `steps` equals the
 * number of `/v1/responses` POSTs handled (== {@link FakeProvider.requests}.length).
 */
export interface LifecycleEvidence {
  steps: number;
  /** A provider request carrying the delegated child's user task was observed. */
  childTurns: number;
  /** A `Bash` tool call was emitted (drives the real command supervisor root). */
  bash: number;
  /** An `apply_patch` custom tool call was emitted (drives the real openat2 fileop root; it
   *  writes a marker file into the worktree, the test's command-root filesystem evidence). */
  patch: number;
  /** A `spawn_agent` delegation was emitted (drives a synchronous demuxed child turn). */
  spawn: number;
  /** A cooperative `checkpoint` signal was emitted (drives ctx.checkpoint + a new-root resume). */
  checkpoint: number;
  /** A `submit_plan` root workflow signal was emitted. */
  submitPlan: number;
  /** A `signal_done` root workflow signal was emitted (the run-completing signal). */
  signalDone: number;
  /** A finish message was emitted (completes the turn once the scripted sequence is exhausted). */
  finish: number;
}

/**
 * The IMAGE-leg lifecycle responder that ALSO records what it drove (item 8). One output item
 * per `/v1/responses` POST, in a fixed sequence — Bash → apply_patch → spawn_agent → checkpoint
 * → submit_plan → signal_done — after which it emits a finish message on every subsequent POST
 * so the current (and any straggler child) turn completes cleanly. Each stage's argument carries
 * the matching secret-shaped canary from {@link CodexCanaries} (model-chosen tool args, distinct
 * from the credential/capability the boundary asserts absent). The returned `evidence` is mutated
 * in place as the run drives, so the test reads it AFTER the run.
 *
 * VERIFIED 2026-09-10: pinned Codex accepted this Responses framing and parent/child
 * interleaving in both native AMD64 packaged images. The always-eventually
 * `signal_done` keeps the fixture bounded after the scripted stages.
 */
export function recordingLifecycleResponder(
  canaries: CodexCanaries,
): { respond: (body: ResponsesBody, provider: FakeProvider) => ResponseItem[]; evidence: LifecycleEvidence } {
  const evidence: LifecycleEvidence = {
    steps: 0, childTurns: 0, bash: 0, patch: 0, spawn: 0, checkpoint: 0, submitPlan: 0, signalDone: 0, finish: 0,
  };
  // patchTool's FROZEN filename constraint is /^[a-z-]+$/, so map the canary to a filename-safe
  // form (the marker file only proves the fileop command root ran; the credential/capability
  // boundary is what the suite asserts). [#1171 m5 review]
  const patchName = canaries.patchArg.replace(/[^a-z-]+/g, "-");
  const script: Array<{ item: () => ResponseItem; mark: () => void; endsTurn?: boolean }> = [
    { item: () => tool("cc-plan", "submit_plan", { plan_md: canaries.planArg }) as ResponseItem, mark: () => { evidence.submitPlan += 1; }, endsTurn: true },
    { item: () => tool("cc-bash", "uzi_bash", { command: `echo ${canaries.bashArg}` }) as ResponseItem, mark: () => { evidence.bash += 1; } },
    { item: () => tool("cc-patch", "uzi_apply_patch", { path: patchName, content: "M3b marker\n" }) as ResponseItem, mark: () => { evidence.patch += 1; } },
    { item: () => tool("cc-spawn", "spawn_agent", { subagent_type: "coder", prompt: canaries.spawnArg }) as ResponseItem, mark: () => { evidence.spawn += 1; } },
    { item: () => tool("cc-ckpt", "checkpoint", {}) as ResponseItem, mark: () => { evidence.checkpoint += 1; }, endsTurn: true },
    { item: () => tool("cc-done", "signal_done", {}) as ResponseItem, mark: () => { evidence.signalDone += 1; }, endsTurn: true },
  ];
  let stage = 0;
  let finishNextRootTurn = false;
  const isChildTurn = (body: ResponsesBody): boolean => {
    const input = Array.isArray(body.input) ? body.input : [];
    return input.some((item) => {
      if (item === null || typeof item !== "object") return false;
      const record = item as Record<string, unknown>;
      // Pinned Codex may encode the turn's user input either as a message content
      // array or as a direct input item. The parent can later retain spawnArg only
      // inside its historical function_call arguments, which this exclusion keeps
      // from being mistaken for the child thread.
      return record.type !== "function_call" && JSON.stringify(record).includes(canaries.spawnArg);
    });
  };
  const respond = (body: ResponsesBody): ResponseItem[] => {
    evidence.steps += 1;
    // A spawn_agent callback starts a synchronous CHILD turn on the same provider.
    // Complete that turn without consuming the root workflow's next scripted stage;
    // the parent history later contains the spawn argument as a function call, but
    // only the child's user message contains it as user content.
    if (isChildTurn(body)) {
      evidence.childTurns += 1;
      evidence.finish += 1;
      return [message("m3b child finished") as ResponseItem];
    }
    if (finishNextRootTurn) {
      finishNextRootTurn = false;
      evidence.finish += 1;
      return [message("m3b lifecycle turn finished") as ResponseItem];
    }
    const next = script[stage];
    if (next !== undefined) {
      stage += 1;
      next.mark();
      finishNextRootTurn = next.endsTurn === true;
      return [next.item()];
    }
    evidence.finish += 1;
    return [message("m3b lifecycle finished") as ResponseItem];
  };
  return { respond, evidence };
}

/** Count the tool-call REPLIES (function/custom tool-call OUTPUT items) the app-server fed back
 *  across every recorded request — provider-visible evidence that the packaged executor's broker
 *  executed a tool and returned its result to Codex (the `callbacks` real-path count). */
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

/** Text returned to one provider-issued dynamic callback, observed in the next
 * Responses request. This distinguishes "the fixture emitted a call" from "the
 * app-server delivered it to the worker and fed the worker's result back". */
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

/** A trivial always-finish responder (a turn with no tool call, just a message). */
export function trivialResponder(): (body: ResponsesBody) => ResponseItem[] {
  return () => [message() as ResponseItem];
}
