// PRD #1171 (M3, milestone 3) — the Codex TOOL-LESS, isolated advice pass.
//
// This is the `AdviceHarness` for `kind:"codex"`: it runs ONE isolated judge/review/
// summary turn against an isolated Codex provider root and returns the accumulated
// assistant text. It is the SEPARATELY-GATED advice ceiling by construction — mirror
// `ClaudeAdviceHarness` (claude-advice-harness.ts) in SEMANTICS, not shape:
//   - it has NO broker, NO callback/handler registry, NO run worktree, NO agents/tools
//     and NO run working directory as a construction input, so the model has NO tool
//     surface (harness-contract.md §"Rate limits and advice" / §"Skills and policy
//     inputs": "An advice constructor must not accept agents, MCP handlers, general
//     tools or a run working directory");
//   - the advice turn is built by `renderCodexAdvice` (render.ts) — model + effort +
//     prompt + output ONLY — which ALSO enforces the ceiling at runtime (it throws if
//     the request carries agents/tools/toolServers/cwd);
//   - the isolated thread/turn is started with the native tool/agent/MCP surface
//     disabled, the canonical project EXPLICITLY untrusted, `project_doc_max_bytes = 0`
//     and NEVER a hook-trust bypass (mirrors config.ts, the native-disabled config).
//
// FULLY UNIT-TESTABLE, NO REAL CODEX: every external effect is an INJECTED SEAM.
//   - `launchRoot(spec)` launches an ISOLATED provider root with its OWN disposable
//     HOME and returns an in-memory {@link CodexTransport} plus a best-effort
//     `dispose()`; production wires an advice launcher, tests inject a fake returning a
//     scripted in-memory transport. There is NO registry and NO broker in this lane.
//
// SECURITY: the provider credential is a PRIVATE construction input (the narrow
// provider-auth bridge for the selected credential, NOT a general callback registry);
// it flows only into the launch spec (→ the isolated app-server's env), NEVER into a
// thread/turn param, a frame, a reply or a log line. This module NEVER logs raw frames,
// tokens or bodies, and it exposes NO credential to the model.
//
// POLICY + TIMEOUT (harness-contract.md §"Rate limits and advice"):
//   - the synchronous uzi `policy.onTerminal` runs ONCE inside terminal consumption,
//     BEFORE the iterator is closed and BEFORE HOME cleanup (mirror ClaudeAdviceHarness
//     ordering); it MAY throw to override the default;
//   - the wall-clock timeout ABORTS the in-flight work AND rejects the race with the
//     EXACT `${label} model call exceeded ${timeoutMs}ms` message; it then waits for the
//     streaming work to settle OR the grace (default 500ms) before best-effort HOME
//     disposal; a cleanup warning NEVER replaces the primary failure; partial text is
//     NEVER returned after a timeout.

import { renderCodexAdvice } from "./render.js";
import { normalizeCodexStatus, normalizeCodexTerminalErrors, normalizeCodexUsage } from "./terminal-normalize.js";
import { errMessage } from "../util.js";

import type { Logger } from "../log.js";
import type {
  AdviceHarness,
  AdviceRequest,
  AdviceResult,
  AdviceResultPolicy,
  HarnessError,
  HarnessTerminal,
  HarnessThrownFailure,
} from "../harness.js";
import type { CodexProviderConfig } from "./codex-harness.js";
import type { CodexNotification, CodexTransport } from "./transport.js";
import type { RenderedCodexAdvice } from "./render.js";

// --- typed advice error -------------------------------------------------------

/**
 * A neutral, typed error carrying a reusable {@link HarnessError}, mirroring
 * `CodexHarnessError` / `CodexTransportError`. The advice harness throws one for its OWN
 * classifications — a setup/protocol violation, its Codex-specific unexpected-EOF
 * `protocol` throw, an abort, or the fail-closed unsupported-output-schema refusal — so a
 * caller inspects `.failure.category` rather than a message.
 */
export class CodexAdviceError extends Error {
  readonly failure: HarnessError;
  constructor(failure: HarnessError) {
    super(failure.message);
    this.name = "CodexAdviceError";
    this.failure = failure;
  }
}

// --- construction inputs / seams ---------------------------------------------

/** The spec the harness hands to {@link LaunchAdviceRootSeam}. It carries ONLY trusted,
 *  launcher-fixed values plus the private credential; it NEVER carries a run worktree, a
 *  broker/handler registry, agents or tools — the advice ceiling by construction. */
export interface CodexAdviceLaunchSpec {
  readonly kind: "advice";
  /** The advice lane label (judge/review/summary); used only for the launcher's own
   *  isolation naming/diagnostics, never model-visible. */
  readonly label: AdviceRequest["label"];
  readonly provider: CodexProviderConfig;
  /** The resolved model; the launcher points the isolated app-server at it. */
  readonly model: string;
  /** The provider credential; PRIVATE — the seam forwards it to the isolated launcher
   *  env ONLY, never anywhere model-visible. */
  readonly credentialValue?: string;
  /** OPTIONAL deadline/cancel signal for the launch itself. The harness passes its internal
   *  abort signal (fired by the external request.signal OR the wall-clock timeout) so a slow
   *  launcher can be cancelled instead of running uncancellable past the timeout. The
   *  production launcher wiring is a deferred seam; honoring this is additive/best-effort. */
  readonly signal?: AbortSignal;
}

/** What {@link LaunchAdviceRootSeam} returns: the app-server transport over the isolated
 *  root's stdio, the isolated (launcher-chosen, throwaway) project dir the harness pins
 *  untrusted, and a best-effort teardown of the root + its OWN disposable HOME. There is
 *  NO registry root and NO supervisor pid here — advice owns no callback lane. */
export interface CodexAdviceLaunchResult {
  readonly transport: CodexTransport;
  /** The isolated, launcher-chosen project dir (a throwaway, NOT a run worktree), pinned
   *  EXPLICITLY untrusted on thread/start (an unset trust promotes to trusted). */
  readonly cwd: string;
  /** Best-effort teardown of the isolated provider root and its disposable HOME. */
  dispose(): Promise<void>;
}

/** The injected advice-launch seam. Production wires an advice launcher (an isolated
 *  provider root with its own disposable HOME and NO worker-callback/broker registry);
 *  tests inject a fake returning an in-memory transport (NO real Codex process). */
export type LaunchAdviceRootSeam = (spec: CodexAdviceLaunchSpec) => Promise<CodexAdviceLaunchResult>;

/** Construction inputs — the SEPARATELY-GATED advice surface. Deliberately NARROW: NO
 *  agents, NO tools, NO broker, NO registry, NO run worktree, NO handler registry. The
 *  credential is a PRIVATE input (the narrow provider-auth bridge for the selected
 *  credential, not a general callback registry). */
export interface CodexAdviceHarnessOptions {
  readonly launchRoot: LaunchAdviceRootSeam;
  readonly provider: CodexProviderConfig;
  /** PRIVATE provider credential; never model-visible, never logged, never in a frame. */
  readonly credentialValue?: string;
  readonly log: Logger;
}

/** Default bounded grace (ms) for the aborted app-server to settle before its isolated
 *  HOME is disposed — mirrors model-pass.ts `DEFAULT_ABORT_GRACE_MS`. Best-effort: the
 *  grace only ever elapses on the timeout path when the work does not settle promptly
 *  after abort (the success path settles first, so no grace is waited). */
const DEFAULT_ADVICE_GRACE_MS = 500;

/**
 * Hard ceiling on the TOTAL accumulated advice text, enforced as a running byte sum in
 * {@link CodexAdviceHarness.consume}. The transport's per-frame (4 MiB) and aggregate
 * inbound-queue (64 MiB) ceilings bound only momentary BUFFERING: because `consume` drains
 * notifications ONE at a time, the queue stays near-empty and never binds, so the running
 * `text` string it accumulates would otherwise be UNBOUNDED — a hostile or looping
 * app-server streaming near-cap frames within the timeout window could accrue arbitrary
 * text (100+ MiB observed) and OOM the worker, and the wall-clock timeout is not a memory
 * bound. 8 MiB is generous headroom for any judge/review/summary output (advice results are
 * model text, realistically kilobytes) yet well below Node's default old-space (~2 GB,
 * `--max-old-space-size`), so a genuine advice response never trips it while a runaway
 * stream fails closed long before OOM.
 */
const MAX_ADVICE_TEXT_BYTES = 8 * 1024 * 1024;

// --- small pure helpers -------------------------------------------------------

function asObject(v: unknown): Record<string, unknown> | undefined {
  return v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined;
}

function asString(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}

/** Extract non-empty text off an item, from a bare `text` string and/or a `content`
 *  array of `{ text }` parts. Empty strings are omitted (mirrors CodexHarness decode). */
function extractText(item: Record<string, unknown>): string[] {
  const out: string[] = [];
  const bare = item.text;
  if (typeof bare === "string" && bare.length > 0) out.push(bare);
  const content = item.content;
  if (Array.isArray(content)) {
    for (const part of content) {
      const t = asObject(part)?.text;
      if (typeof t === "string" && t.length > 0) out.push(t);
    }
  }
  return out;
}

/** Wait for the streaming work to settle before the isolated HOME is disposed, bounded by
 *  `graceMs` so work that never settles after abort can never wedge cleanup. NEVER
 *  rejects: the work's own rejection was already surfaced to the caller by the race in
 *  {@link CodexAdviceHarness.run}, so it is swallowed here (attaching a rejection handler
 *  also keeps a post-abort rejection from surfacing as an unhandled rejection). Mirrors
 *  model-pass.ts `awaitQuerySettled`. */
async function awaitSettled(work: Promise<unknown>, graceMs: number): Promise<void> {
  let graceTimer: NodeJS.Timeout | undefined;
  const grace = new Promise<void>((resolve) => {
    graceTimer = setTimeout(resolve, graceMs);
    graceTimer.unref?.();
  });
  try {
    await Promise.race([work.then(() => {}, () => {}), grace]);
  } finally {
    if (graceTimer) clearTimeout(graceTimer);
  }
}

// --- the advice harness -------------------------------------------------------

export class CodexAdviceHarness implements AdviceHarness {
  readonly kind = "codex" as const;

  private readonly launchRoot: LaunchAdviceRootSeam;
  private readonly provider: CodexProviderConfig;
  // PRIVATE construction input; NEVER model-visible, never logged, never in a frame.
  private readonly credentialValue?: string;
  private readonly log: Logger;

  constructor(opts: CodexAdviceHarnessOptions) {
    this.launchRoot = opts.launchRoot;
    this.provider = opts.provider;
    this.credentialValue = opts.credentialValue;
    this.log = opts.log;
  }

  async run(request: AdviceRequest, policy: AdviceResultPolicy): Promise<AdviceResult> {
    // 1. Render (pure). renderCodexAdvice ENFORCES the advice ceiling at runtime: a
    //    request carrying agents/tools/toolServers/cwd throws here — a programming error,
    //    not a silently-ignored field.
    const rendered = renderCodexAdvice(request);

    // 2. Output schema: FAIL-CLOSED. Codex native outputSchema enforcement is not yet
    //    lane-specific/measured (harness-contract.md §"Rate limits and advice": "Codex's
    //    output schema support must be lane-specific and measured"). A {kind:"json"}
    //    request is REFUSED rather than run as unenforced text — we never silently pretend
    //    native outputSchema enforcement. All existing advice callers use {kind:"text"}.
    if (request.output.kind !== "text") {
      throw new CodexAdviceError({
        category: "protocol",
        message:
          `codex advice does not support output {kind:"${request.output.kind}"}: `
          + `native outputSchema enforcement is unmeasured (fail-closed)`,
      });
    }

    const model = rendered.model ?? this.provider.model;

    // Internal abort: fired by the external request.signal OR the wall-clock timeout. It
    // bounds the thread/turn RPCs and the notifications race so a hung pass settles.
    const internalAbort = new AbortController();
    const onExternalAbort = (): void => internalAbort.abort();
    if (request.signal.aborted) internalAbort.abort();
    else request.signal.addEventListener("abort", onExternalAbort, { once: true });

    // Wall-clock cap: abort the in-flight work AND hard-reject the race with the EXACT
    // label/timeout message (mirror the Claude advice message format), so a hung/retrying
    // advice call can never wedge the caller. Partial text is never returned after this.
    let timer: NodeJS.Timeout | undefined;
    const timeout = new Promise<never>((_, reject) => {
      timer = setTimeout(() => {
        internalAbort.abort();
        reject(new Error(`${request.label} model call exceeded ${request.timeoutMs}ms`));
      }, request.timeoutMs);
    });

    // Launch + stream is ONE raced unit so the timeout bounds setup too. `disposeSeam` is
    // captured once the launch resolves. Disposal is funneled through the idempotent
    // `disposeOnce` so it runs EXACTLY ONCE regardless of race ordering.
    let disposeSeam: (() => Promise<void>) | undefined;
    let disposed = false;
    const disposeOnce = async (): Promise<void> => {
      if (disposed) return;
      const d = disposeSeam;
      if (d === undefined) return; // nothing launched yet (a launch that itself rejected leaves nothing)
      disposed = true;
      await d().catch((e) =>
        this.log.warn(`${request.label} codex advice HOME cleanup failed`, { error: errMessage(e) }),
      );
    };
    const work = (async (): Promise<AdviceResult> => {
      const launched = await this.launchRoot({
        kind: "advice",
        label: request.label,
        provider: this.provider,
        model,
        // PRIVATE → the isolated launcher env ONLY; never a thread/turn param or frame.
        credentialValue: this.credentialValue,
        // Bound the launch itself by the same deadline: a slow launcher can be aborted
        // instead of resolving uncancellable past the timeout and leaking its root.
        signal: internalAbort.signal,
      });
      disposeSeam = launched.dispose;
      return this.consume(launched.transport, launched.cwd, rendered, model, policy, internalAbort.signal);
    })();

    // Late-disposal owner: a launch that resolves AFTER the finally already ran (a slow
    // launch that outlives the grace on the timeout path) still gets disposed here. Idempotent
    // with the finally's `disposeOnce`, so the root is torn down EXACTLY ONCE whichever wins;
    // `work`'s own rejection is swallowed (it was already surfaced by the race).
    void work.then(() => disposeOnce(), () => disposeOnce());

    try {
      return await Promise.race([work, timeout]);
    } finally {
      if (timer) clearTimeout(timer);
      request.signal.removeEventListener("abort", onExternalAbort);
      // Wait for the streaming work to settle (bounded by grace after abort) BEFORE the
      // isolated HOME is disposed, so cleanup does not race an aborted app-server that may
      // still touch its HOME. On the success path work already settled → returns at once.
      await awaitSettled(work, request.graceMs ?? DEFAULT_ADVICE_GRACE_MS);
      // Best-effort HOME/root disposal, EXACTLY ONCE. If the launch has not resolved within
      // the grace this is a no-op and the late disposer above owns cleanup instead. A cleanup
      // warning NEVER replaces the primary failure (the try's throw/return already stands).
      await disposeOnce();
    }
  }

  // --- setup + stream -----------------------------------------------------------

  /** Start the isolated thread + turn and consume the notification stream, accumulating
   *  assistant text. On the terminal, invoke `policy.onTerminal` SYNCHRONOUSLY before the
   *  iterator is closed, then return. Server→client requests are REFUSED fail-closed (no
   *  broker, no tool surface). An abort ends the stream without partial text; an
   *  unexpected EOF is a Codex protocol throw. */
  private async consume(
    transport: CodexTransport,
    cwd: string,
    rendered: RenderedCodexAdvice,
    model: string | undefined,
    policy: AdviceResultPolicy,
    signal: AbortSignal,
  ): Promise<AdviceResult> {
    const threadId = await this.startThread(transport, cwd, rendered, model, signal);
    await this.startTurnRpc(transport, threadId, rendered, model, signal);

    // Single-consumer: obtain the notifications iterator exactly once.
    const notes = transport.notifications();
    let text = "";
    // Running byte sum of the accumulated advice text, so the total is bounded against an
    // unbounded/looping stream (see MAX_ADVICE_TEXT_BYTES). Kept O(n) total: each appended
    // chunk is measured EXACTLY ONCE here, never a re-scan of the whole `text`.
    let textBytes = 0;

    // Abort race: an aborted signal (timeout OR external) ends the stream promptly rather
    // than blocking forever on notes.next().
    let onAbort: (() => void) | undefined;
    const aborted = new Promise<"aborted">((resolve) => {
      if (signal.aborted) {
        resolve("aborted");
        return;
      }
      onAbort = (): void => resolve("aborted");
      signal.addEventListener("abort", onAbort, { once: true });
    });

    try {
      for (;;) {
        const step = await Promise.race([notes.next(), aborted]);
        if (step === "aborted") {
          // Owner/timeout abort wins. End the stream WITHOUT a terminal and WITHOUT
          // returning partial text: the race's rejection is the primary failure and this
          // throw is swallowed by awaitSettled.
          throw new CodexAdviceError({ category: "aborted", message: `codex advice ${rendered.label} aborted` });
        }
        if (step.done) {
          // Clean EOF with no terminal is Codex's own unexpected-EOF → a protocol throw
          // (a transport failure is thrown, not returned; mirrors CodexHarness rule 9).
          throw new CodexAdviceError({
            category: "protocol",
            message: "codex advice stream ended before turn completion",
          });
        }
        const note = step.value;

        // A server→client request: NO broker, NO callback registry, NO tool surface.
        // Answer fail-closed and run NOTHING — this is the tool-less advice ceiling by
        // construction (there is nowhere to route an effect even if the model asked).
        if (note.kind === "activity" && note.requestId !== undefined) {
          this.refuseServerRequest(transport, note.requestId);
          continue;
        }

        if (note.kind === "turn_completed") {
          const terminal = this.decodeTerminal(note);
          const isError = terminal.outcome === "failed";
          // The synchronous uzi policy runs HERE, inside terminal consumption, BEFORE the
          // iterator is closed and BEFORE HOME cleanup (mirror ClaudeAdviceHarness). It
          // MAY throw to override the default; a throw propagates out of run(). Codex M3
          // carries no rate-limit facts yet, so `latest` is undefined (measured/deferred).
          policy.onTerminal(terminal, { isError, latest: undefined });
          return { text, end: { kind: "terminal", terminal }, usage: terminal.usage };
        }

        // Assistant text accumulation (item/completed agentMessage). Everything else
        // (thread/started, turn/started, item deltas, unknown methods) is liveness only.
        if (note.kind === "activity") {
          const chunk = this.extractAdviceText(note);
          if (chunk.length > 0) {
            const chunkBytes = Buffer.byteLength(chunk, "utf8");
            // FAIL-CLOSED when appending WOULD exceed the ceiling: throw the bounded
            // overflow error (mirroring the transport's own {category:"transport"} inbound
            // overflow discipline) rather than return a silently-truncated verdict — a
            // truncated judge/review result is worse than a thrown failure, which advice
            // callers already handle. The throw propagates through run()'s settlement /
            // grace / HOME-dispose finally like any other stream failure.
            if (textBytes + chunkBytes > MAX_ADVICE_TEXT_BYTES) {
              throw new CodexAdviceError({
                category: "transport",
                message: "advice response exceeded the maximum size",
              });
            }
            textBytes += chunkBytes;
            text += chunk;
          }
        }
      }
    } finally {
      if (onAbort) signal.removeEventListener("abort", onAbort);
      // Idempotent iterator/transport closure AFTER the policy ran (or after abort/EOF).
      // This is the "iterator closure" the synchronous policy precedes.
      await transport.close();
    }
  }

  /** The isolated advice thread config, pinned fail-closed ON THE WIRE: the native
   *  tool/agent/MCP surface disabled (mirrors config.ts, the native-disabled config), the
   *  canonical project EXPLICITLY untrusted, `project_doc_max_bytes = 0`, and NEVER a
   *  hook-trust bypass. The stock config.toml already pins these; re-asserting them here is
   *  defense-in-depth against a promoted trust. */
  private adviceThreadConfig(cwd: string): Record<string, unknown> {
    return {
      project_doc_max_bytes: 0,
      web_search: "disabled",
      agents: { enabled: false },
      features: {
        apps: false,
        plugins: false,
        shell_snapshot: false,
        shell_snapshot_v2: false,
        code_mode: false,
        code_mode_only: false,
        code_mode_host: false,
        code_mode_prewarm: false,
        remote_models: false,
        unified_exec: false,
        hooks: false,
        multi_agent: false,
        multi_agent_v2: false,
        enable_request_compression: false,
      },
      projects: { [cwd]: { trust_level: "untrusted" } },
    };
  }

  private async startThread(
    transport: CodexTransport,
    cwd: string,
    rendered: RenderedCodexAdvice,
    model: string | undefined,
    signal: AbortSignal,
  ): Promise<string> {
    const res = await transport.request<{ thread?: { id?: string } }>(
      "thread/start",
      {
        model,
        modelProvider: this.provider.name,
        cwd,
        approvalPolicy: "never",
        ephemeral: true,
        config: this.adviceThreadConfig(cwd),
        instructions: rendered.systemPrompt,
      },
      { signal },
    );
    const id = res?.thread?.id;
    if (typeof id !== "string" || id.length === 0) {
      throw new CodexAdviceError({ category: "protocol", message: "codex advice thread/start returned no thread id" });
    }
    return id;
  }

  private async startTurnRpc(
    transport: CodexTransport,
    threadId: string,
    rendered: RenderedCodexAdvice,
    model: string | undefined,
    signal: AbortSignal,
  ): Promise<void> {
    const params: Record<string, unknown> = {
      threadId,
      input: [{ type: "text", text: rendered.prompt }],
    };
    if (model !== undefined) params.model = model;
    if (rendered.modelReasoningEffort !== undefined) params.modelReasoningEffort = rendered.modelReasoningEffort;
    const res = await transport.request<{ turn?: { id?: string } }>("turn/start", params, { signal });
    const id = res?.turn?.id;
    if (typeof id !== "string" || id.length === 0) {
      throw new CodexAdviceError({ category: "protocol", message: "codex advice turn/start returned no turn id" });
    }
  }

  /** Refuse a server→client request fail-closed — advice is tool-less. There is no broker
   *  to route to, so the reply is a JSON-RPC method error and NO effect ever runs. The
   *  reply may fail if the transport is already closed (an abort/timeout raced it); a
   *  reply that can no longer be delivered is dropped, NEVER thrown. */
  private refuseServerRequest(transport: CodexTransport, requestId: number | string): void {
    try {
      transport.respond(requestId, { error: { code: -32601, message: "codex advice is tool-less" } });
    } catch {
      /* transport already closed: an undeliverable reply is dropped, never thrown */
    }
  }

  /** Extract assistant text off an `item/completed` agent-message note. Only text (never
   *  reasoning) is accumulated into AdviceResult.text. Mirrors CodexHarness item decode;
   *  the provisional item-type strings are the same current best guess and are verified in
   *  the packaged integration, not here. */
  private extractAdviceText(note: Extract<CodexNotification, { kind: "activity" }>): string {
    if (note.method !== "item/completed") return "";
    const item = asObject(asObject(note.params)?.item);
    if (!item) return "";
    const type = asString(item.type);
    if (type === "agentMessage" || type === "assistantMessage" || type === "agent_message") {
      return extractText(item).join("");
    }
    return "";
  }

  /** Decode a `turn/completed` note into a neutral {@link HarnessTerminal}. Outcome is
   *  success only for status `completed`; anything else (incl. absent) is fail-closed
   *  `failed`. Usage is labeled `basis:"turn"` (Codex declares turn usage). A terminal
   *  PROVIDER failure is DATA here — it is never also thrown. */
  private decodeTerminal(note: Extract<CodexNotification, { kind: "turn_completed" }>): HarnessTerminal {
    const turn = asObject(asObject(note.params)?.turn);
    const rawStatus = note.status ?? asString(turn?.status);
    // Route through the single-source-of-truth normalizers so this advice decoder and the
    // run-lane decoder (codex-harness.ts) cannot diverge and no raw provider field is ever
    // retained: subtype/outcome come from the CLOSED vocabulary, errors are provider-text-
    // free (the raw, possibly secret-bearing `turn.error` is never read), and usage is a
    // bounded numeric-only subset (the raw, possibly multi-megabyte object is never kept).
    const { subtype, outcome } = normalizeCodexStatus(rawStatus);
    const errors = normalizeCodexTerminalErrors(subtype, outcome);
    const usage = normalizeCodexUsage(turn?.usage, "turn");
    return {
      outcome,
      subtype,
      errors,
      usage,
      metrics: { cost: { kind: "unreported" } },
      failure: {
        // Deferred, invoked only at the owner's classification point. Codex M3 carries no
        // limit facts, so this constructs the generic terminal exception; it never invents
        // an auth/model/effort category from a provider status, and its message is based on
        // the CLOSED subtype, never the raw provider status string.
        materialize: (_limit): HarnessThrownFailure => {
          const original = new Error(`codex advice turn failed: ${subtype}`);
          return { failure: { category: "unknown", message: original.message }, original };
        },
      },
    };
  }
}
