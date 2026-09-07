// PRD #1171 (M3, milestone 3) — the Codex run-harness core.
//
// This is the `RunHarness` for `kind:"codex"`: it integrates the M1/M2 units
// (the immutable per-run {@link ExecutionRegistry}, the isolated launch primitive,
// the app-server JSON-RPC {@link CodexTransport}, the fail-closed
// {@link CodexCallbackBroker} and the deterministic {@link renderCodexRun}) into a
// working per-turn stream. It decodes each raw app-server frame into EXACTLY ONE
// neutral {@link HarnessEvent} per the accepted event/error authority
// (e2e/codex-m0/harness-contract.md §"Event, reducer and error authority") and it
// routes every server→client tool-call request to the broker.
//
// FULLY UNIT-TESTABLE, NO REAL CODEX: every external effect is an INJECTED SEAM.
//   - `launchRoot(spec)` reserves+launches a provider supervisor root and returns an
//     in-memory {@link CodexTransport}; production wires `launchCodexRoot` + a
//     `CodexRootHandle`→transport adapter, tests inject a fake returning a scripted
//     in-memory transport. The harness RESERVES the launch in the registry before the
//     call and REGISTERS the returned root after it.
//   - `broker` is the sole callback authority; the harness never authorizes an effect
//     itself. It routes each `item/tool/call` to `broker.handleToolCall(...)` and only
//     replies (via `transport.respond`) AFTER the broker settles.
//   - `render` (default {@link renderCodexRun}) resolves the model/effort/prompt and the
//     per-role grants; the harness uses it to configure the thread/turn.
//   - `sessionInspect` backs {@link inspectSession}.
//
// SECURITY: the provider credential is a PRIVATE construction input; it flows only into
// the launch spec (→ the app-server's env), NEVER into a thread/turn param, a frame, a
// tool result or a log line. This module NEVER logs raw frames, tokens or bodies.
//
// EVENT DECODE (app-server frame → neutral event), authoritative rules 1/3/8/9:
//   thread/started            → `initialized` (the model-bearing init; carries the
//                               configured model, distinct from a bare turn-start)
//   turn/started              → `activity`
//   item/completed (agent msg)→ `frame` (text/thinking items + call-basis usage + model)
//   item/* (deltas, other)    → `activity`  (Codex item updates are activity)
//   item/tool/call (id-bearing)→ intercepted: broker route + `transport.respond`, then
//                               yielded as `activity` (NOT a frame)
//   other server→client req   → answered with a fail-closed JSON-RPC method error, then
//                               yielded as `activity`
//   turn/completed            → `turn_finished` (decoded terminal; success/failed by
//                               status), then the iterator closes
// ERROR PRECEDENCE (run-lane, rule 9): a local watchdog/cancel (the owner's aborted
// `request.signal`) ends the stream first; a transport/iterator throw propagates next; a
// terminal provider failure is represented ONCE as `turn_finished` data and NEVER also
// thrown; a clean EOF with no terminal is Codex's own unexpected-EOF → a `protocol`
// {@link CodexHarnessError} throw (Claude's clean-EOF `exhausted` is unaffected).

import { renderCodexRun } from "./render.js";

import type { Logger } from "../log.js";
import type {
  BoundaryRequest,
  ChildQuiescence,
  HarnessContext,
  HarnessError,
  HarnessEvent,
  HarnessItem,
  HarnessTerminal,
  HarnessThrownFailure,
  HarnessTurn,
  HarnessUsage,
  ProcessReap,
  RunHarness,
  RunTurnRequest,
  SessionPresence,
  ToolDisposal,
} from "../harness.js";
import type { CallbackOrigin, CallbackResult, CodexCallbackBroker } from "./broker.js";
import type { ExecutionRegistry, RegisteredRoot } from "./registry.js";
import type { CodexNotification, CodexTransport } from "./transport.js";
import type { RenderedCodexRun } from "./render.js";

// --- typed harness error ------------------------------------------------------

/**
 * A neutral, typed error carrying a reusable {@link HarnessError}, mirroring
 * `CodexTransportError`. The harness throws one only for its OWN classifications —
 * chiefly the Codex-specific unexpected-EOF `protocol` throw and setup/protocol
 * violations — so a caller inspects `.failure.category` rather than a message.
 */
export class CodexHarnessError extends Error {
  readonly failure: HarnessError;
  constructor(failure: HarnessError) {
    super(failure.message);
    this.name = "CodexHarnessError";
    this.failure = failure;
  }
}

// --- construction inputs / seams ---------------------------------------------

/** The FIXED provider the run points Codex at. In production these are immutable
 *  launcher-fixed values (endpoint/name/envKey/model); the credential is separate. */
export interface CodexProviderConfig {
  readonly name: string;
  readonly baseUrl: string;
  readonly envKey: string;
  readonly model: string;
}

/** The spec the harness builds and hands to {@link CodexHarnessOptions.launchRoot}. It
 *  carries only trusted, launcher-fixed values plus the per-run home/workspace; the
 *  credential rides here (→ the app-server env) and never anywhere model-visible. */
export interface CodexLaunchRootSpec {
  readonly kind: "provider";
  readonly provider: CodexProviderConfig;
  readonly model: string;
  /** The canonical project / launch cwd (provisioned untrusted). */
  readonly cwd: string;
  /** The runner-writable owned data root for the per-launch HOME/CODEX_HOME/XDG trees. */
  readonly ownedDataRoot: string;
  /** The provider credential; PRIVATE — the seam forwards it to the launcher env only. */
  readonly credentialValue?: string;
}

/** What {@link CodexHarnessOptions.launchRoot} returns: a registry-ownable supervisor
 *  root, the app-server transport over its stdio, and the supervisor pid. */
export interface CodexLaunchRootResult {
  readonly root: RegisteredRoot;
  readonly transport: CodexTransport;
  readonly supervisorPid: number;
}

/** The injected launch seam. Production wraps `launchCodexRoot`; tests inject a fake
 *  returning an in-memory transport (NO real Codex process). */
export type LaunchRootSeam = (spec: CodexLaunchRootSpec) => Promise<CodexLaunchRootResult>;

/** The injected session-presence seam backing {@link CodexHarness.inspectSession}. */
export type SessionInspectSeam = (id: string) => Promise<SessionPresence>;

export interface CodexHarnessOptions {
  readonly registry: ExecutionRegistry;
  readonly launchRoot: LaunchRootSeam;
  readonly broker: CodexCallbackBroker;
  readonly provider: CodexProviderConfig;
  /** The per-run canonical project / launch cwd. */
  readonly workspace: string;
  /** The per-run runner-writable owned data root. */
  readonly homeDir: string;
  readonly log: Logger;
  /** Defaults to {@link renderCodexRun}; injectable for tests. */
  readonly render?: (request: RunTurnRequest) => RenderedCodexRun;
  readonly sessionInspect: SessionInspectSeam;
  /** PRIVATE provider credential; never model-visible, never logged, never in a frame. */
  readonly credentialValue?: string;
}

// --- small pure helpers -------------------------------------------------------

function asObject(v: unknown): Record<string, unknown> | undefined {
  return v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined;
}

function asString(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}

/** Extract non-empty text off an item, from a bare `text` string and/or a `content`
 *  array of `{ text }` parts. Empty strings are omitted (mirrors Claude decode). */
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

// --- the harness --------------------------------------------------------------

export class CodexHarness implements RunHarness {
  readonly kind = "codex" as const;

  private readonly registry: ExecutionRegistry;
  private readonly launchRootSeam: LaunchRootSeam;
  private readonly broker: CodexCallbackBroker;
  private readonly provider: CodexProviderConfig;
  private readonly workspace: string;
  private readonly homeDir: string;
  private readonly log: Logger;
  private readonly render: (request: RunTurnRequest) => RenderedCodexRun;
  private readonly sessionInspect: SessionInspectSeam;
  // Private construction input; NEVER model-visible, never logged, never in a frame.
  private readonly credentialValue?: string;

  // Run-level provider root state (launched once, reused across turns; a reused
  // notifications() iterator is single-consumer so it is obtained exactly once).
  private transport?: CodexTransport;
  private notes?: AsyncIterableIterator<CodexNotification>;
  private threadId?: string;
  private currentModel?: string;

  // Per-turn state (turns run strictly sequentially).
  private activeTurnId?: string;
  private turnClosed = false;
  private terminalEmitted = false;
  // Resolves the current turn's abort race (see runTurn). The owner-aborted signal
  // resolves it via the listener; requestStop()/close() resolve it directly so a turn
  // WEDGED in a pending broker callback (a never-settling model-selected effect) still
  // ends promptly instead of hanging on that bare await. Undefined until the stream starts.
  private stopTurn?: () => void;

  constructor(opts: CodexHarnessOptions) {
    this.registry = opts.registry;
    this.launchRootSeam = opts.launchRoot;
    this.broker = opts.broker;
    this.provider = opts.provider;
    this.workspace = opts.workspace;
    this.homeDir = opts.homeDir;
    this.log = opts.log;
    this.render = opts.render ?? renderCodexRun;
    this.sessionInspect = opts.sessionInspect;
    this.credentialValue = opts.credentialValue;
  }

  inspectSession(id: string): Promise<SessionPresence> {
    return this.sessionInspect(id);
  }

  startTurn(request: RunTurnRequest): HarnessTurn {
    // Render synchronously so a render throw surfaces to the caller here; the rest of
    // the setup (launch, thread/start, turn/start) is deferred into the events
    // generator so a setup throw surfaces to the owner's for-await (its trip > throw >
    // terminal precedence handles it), exactly as the Claude adapter defers query
    // creation.
    const rendered = this.render(request);
    this.turnClosed = false;
    this.terminalEmitted = false;
    this.activeTurnId = undefined;

    return {
      events: this.runTurn(request, rendered),
      requestStop: (reason) => this.requestStop(reason),
      readContext: (timeoutMs) => this.readContext(timeoutMs),
      close: () => this.close(),
    };
  }

  // --- process-safety surface: delegate to the registry (it owns the epoch/reap/
  //     dispose logic). Only CodexHarness implements these optional members. --------

  quiesceChildren(request: BoundaryRequest): Promise<ChildQuiescence> {
    return this.registry.quiesceChildren(request.deadlineMs);
  }

  reapProcesses(request: BoundaryRequest, closedEpoch: number): Promise<ProcessReap> {
    return this.registry.reapProcesses(request.deadlineMs, closedEpoch);
  }

  disposeTools(request: BoundaryRequest): Promise<ToolDisposal> {
    return this.registry.disposeTools(request.deadlineMs);
  }

  // --- per-turn control ---------------------------------------------------------

  /** Request the in-flight turn stop by interrupting the thread's active turn. This is
   *  a CANCELLATION REQUEST, not a process kill (rule: "request root stop"). Best-effort
   *  and fire-and-forget; a failed interrupt never surfaces as a turn failure. */
  private requestStop(_reason: "terminal" | "cancel" | "timeout"): void {
    const transport = this.transport;
    const threadId = this.threadId;
    const turnId = this.activeTurnId;
    if (transport && threadId !== undefined && turnId !== undefined) {
      void transport
        .request("turn/interrupt", { threadId, turnId })
        .catch(() => {
          /* interrupt is best-effort; the terminal/EOF path settles the turn */
        });
    }
    // End the stream even when it is wedged in a pending broker callback: the run loop
    // otherwise only races notes.next(), so resolve the turn's stop promise to settle the
    // abort race and return the generator promptly. A no-op before the stream starts.
    this.stopTurn?.();
  }

  /** Bounded lead-context read. Codex exposes no characterized context-window RPC yet,
   *  so this returns `undefined` (absence) rather than fabricate a reading. The contract
   *  is bounded + swallow-to-undefined on absence/error/hang; immediate-undefined honors
   *  it. Deferred to a later milestone when a context source is measured. */
  private async readContext(_timeoutMs: number): Promise<HarnessContext | undefined> {
    return undefined;
  }

  /** Idempotent root iterator/transport closure ONLY. Closes the turn's iteration and
   *  the shared root transport (idempotent); it does NOT drain children, kill groups or
   *  reap — that is quiesce/reap/dispose. */
  private async close(): Promise<void> {
    this.turnClosed = true;
    // Wake a run loop wedged in a pending broker callback so the generator returns rather
    // than hang on that await while the transport is torn down underneath it.
    this.stopTurn?.();
    await this.transport?.close();
  }

  /** Shared stop path for an owner abort / watchdog / requestStop / close: interrupt the
   *  turn (a cancellation REQUEST, not a process kill) and mark the stream closed. The
   *  pending effect's process cleanup is the registry's reap job at the safety boundary
   *  (quiesce/reap), not the harness's — here we only guarantee the STREAM ends. */
  private endTurnOnStop(): void {
    this.requestStop("cancel");
    this.turnClosed = true;
  }

  // --- setup + stream -----------------------------------------------------------

  private async *runTurn(request: RunTurnRequest, rendered: RenderedCodexRun): AsyncGenerator<HarnessEvent> {
    this.currentModel = rendered.lead.model ?? this.provider.model;

    // A local watchdog/cancel (owner-aborted signal) ends the stream FIRST (rule 9).
    // requestStop()/close() also settle it via `stopTurn`, so a turn wedged in a pending
    // broker callback ends promptly and does not depend on a new notification arriving.
    let onAbort: (() => void) | undefined;
    const abortPromise = new Promise<"aborted">((resolve) => {
      this.stopTurn = (): void => resolve("aborted");
      if (request.signal.aborted) {
        resolve("aborted");
        return;
      }
      onAbort = (): void => resolve("aborted");
      request.signal.addEventListener("abort", onAbort, { once: true });
    });

    try {
      // 1. Ensure the provider root + transport (launched once, reused after).
      await this.ensureRoot();
      const transport = this.transport;
      const notes = this.notes;
      if (!transport || !notes) {
        throw new CodexHarnessError({ category: "protocol", message: "codex provider root is unavailable after launch" });
      }

      // 2. Start (or resume) the thread with the explicit untrusted / doc-max config —
      //    and NEVER a hook-trust bypass. Reused across turns once established.
      if (this.threadId === undefined) {
        this.threadId =
          request.resumeSessionId !== undefined
            ? await this.resumeThread(transport, request, rendered)
            : await this.startThread(transport, rendered, request.signal);
      }

      // 3. Start the turn with the rendered prompt / model / effort.
      this.activeTurnId = await this.startTurnRpc(transport, this.threadId, rendered, request.signal);

      // 4. Consume the notification stream, mapping each raw frame to ONE neutral event.
      for (;;) {
        if (this.turnClosed) return;
        const step = await Promise.race([notes.next(), abortPromise]);
        if (step === "aborted") {
          // Owner cancel/watchdog wins: interrupt the turn and end the stream cleanly.
          this.endTurnOnStop();
          return;
        }
        if (step.done) {
          // A deliberate close or an already-emitted terminal ends cleanly. Otherwise
          // this is Codex's own unexpected EOF → a protocol throw (rule 9), never a
          // fabricated success.
          if (this.terminalEmitted || this.turnClosed) return;
          throw new CodexHarnessError({
            category: "protocol",
            message: "codex app-server stream ended before turn completion",
          });
        }
        // The broker-callback await (mapNote → routeToolCall → broker.handleToolCall) is
        // itself raced against the owner abort — WITHOUT this, a never-settling broker seam
        // (a model-selected long-running shell) leaves the turn un-cancellable, falsifying
        // rule 9. On abort while a callback is pending we STOP awaiting it and end the
        // stream promptly; the pending effect's process cleanup is the registry's reap job
        // at the safety boundary, and a late broker reply is guarded in routeToolCall so it
        // never throws into a closed transport. The happy path is unchanged: a callback
        // that settles normally still replies via transport.respond after the broker settles.
        const mapped = await Promise.race([this.mapNote(transport, step.value), abortPromise]);
        if (mapped === "aborted") {
          this.endTurnOnStop();
          return;
        }
        yield mapped;
        if (mapped.kind === "turn_finished") return; // close the iterator after the terminal
      }
    } finally {
      if (onAbort) request.signal.removeEventListener("abort", onAbort);
    }
  }

  private async ensureRoot(): Promise<void> {
    if (this.transport) return;
    const reservation = this.registry.reserveLaunch("provider");
    if (reservation.kind !== "reserved") {
      throw new CodexHarnessError({ category: "protocol", message: "codex provider launch admission is closed" });
    }
    let launched: CodexLaunchRootResult;
    try {
      launched = await this.launchRootSeam({
        kind: "provider",
        provider: this.provider,
        model: this.currentModel ?? this.provider.model,
        cwd: this.workspace,
        ownedDataRoot: this.homeDir,
        credentialValue: this.credentialValue,
      });
    } catch (error) {
      // The launch aborted before producing a root: settle the reservation so it does
      // not poison the epoch at quiesce, then re-throw the setup failure.
      this.registry.cancelReservation(reservation.reservation);
      throw error;
    }
    this.registry.registerRoot(reservation.reservation, launched.root);
    this.transport = launched.transport;
    // Single-consumer: obtain the notifications iterator exactly once for the root.
    this.notes = launched.transport.notifications();
    this.log.debug("codex provider root launched", { supervisorPid: launched.supervisorPid });
  }

  /** The thread/start (and thread/resume) config, pinned fail-closed ON THE WIRE: the
   *  canonical project is EXPLICITLY untrusted and `project_doc_max_bytes = 0`, and there
   *  is NEVER a hook-trust bypass. The stock config.toml (config.ts) already pins these;
   *  re-asserting them on start/resume is defense-in-depth against a promoted trust. */
  private threadConfig(): Record<string, unknown> {
    return {
      project_doc_max_bytes: 0,
      projects: { [this.workspace]: { trust_level: "untrusted" } },
    };
  }

  private async startThread(
    transport: CodexTransport,
    rendered: RenderedCodexRun,
    signal: AbortSignal,
  ): Promise<string> {
    const res = await transport.request<{ thread?: { id?: string } }>(
      "thread/start",
      {
        model: this.currentModel,
        modelProvider: this.provider.name,
        cwd: this.workspace,
        approvalPolicy: "never",
        ephemeral: true,
        config: this.threadConfig(),
        instructions: rendered.leadPrompt.systemPrompt,
      },
      { signal },
    );
    const id = res?.thread?.id;
    if (typeof id !== "string" || id.length === 0) {
      throw new CodexHarnessError({ category: "protocol", message: "codex thread/start returned no thread id" });
    }
    return id;
  }

  private async resumeThread(
    transport: CodexTransport,
    request: RunTurnRequest,
    rendered: RenderedCodexRun,
  ): Promise<string> {
    const resumeId = request.resumeSessionId;
    const res = await transport.request<{ thread?: { id?: string } }>(
      "thread/resume",
      {
        threadId: resumeId,
        model: this.currentModel,
        modelProvider: this.provider.name,
        cwd: this.workspace,
        approvalPolicy: "never",
        config: this.threadConfig(),
        instructions: rendered.leadPrompt.systemPrompt,
      },
      { signal: request.signal },
    );
    return res?.thread?.id ?? resumeId ?? "";
  }

  private async startTurnRpc(
    transport: CodexTransport,
    threadId: string,
    rendered: RenderedCodexRun,
    signal: AbortSignal,
  ): Promise<string> {
    const params: Record<string, unknown> = {
      threadId,
      input: [{ type: "text", text: rendered.leadPrompt.prompt }],
    };
    if (this.currentModel !== undefined) params.model = this.currentModel;
    if (rendered.lead.modelReasoningEffort !== undefined) params.modelReasoningEffort = rendered.lead.modelReasoningEffort;
    const res = await transport.request<{ turn?: { id?: string } }>("turn/start", params, { signal });
    const id = res?.turn?.id;
    if (typeof id !== "string" || id.length === 0) {
      throw new CodexHarnessError({ category: "protocol", message: "codex turn/start returned no turn id" });
    }
    return id;
  }

  // --- frame → neutral event ----------------------------------------------------

  /** Map one decoded {@link CodexNotification} to exactly one {@link HarnessEvent}.
   *  Server→client tool-call requests are intercepted here (routed to the broker and
   *  answered via `transport.respond`) and surfaced as `activity`, never as a frame. */
  private async mapNote(transport: CodexTransport, note: CodexNotification): Promise<HarnessEvent> {
    switch (note.kind) {
      case "thread_started":
        // The model-bearing init: carries the model the harness configured, distinct
        // from the bare turn-start below.
        return { kind: "initialized", model: this.currentModel, sessionId: note.threadId };
      case "turn_started":
        return { kind: "activity", sessionId: note.threadId };
      case "turn_completed": {
        const terminal = this.decodeTerminal(note);
        this.terminalEmitted = true;
        return { kind: "turn_finished", terminal, sessionId: note.threadId };
      }
      case "activity": {
        if (note.requestId !== undefined) {
          // A server→client request. Only the tool-call lane routes to the broker; any
          // other server-initiated request is answered fail-closed as unsupported.
          if (note.method === "item/tool/call") {
            await this.routeToolCall(transport, note.requestId, note.params);
          } else {
            transport.respond(note.requestId, { error: { code: -32601, message: "unsupported request" } });
          }
          return { kind: "activity", sessionId: this.threadId };
        }
        const frame = this.decodeItemFrame(note);
        if (frame !== undefined) return frame;
        // A Codex item update / unknown method: liveness only.
        return { kind: "activity", sessionId: this.threadId };
      }
    }
  }

  /** Route one `item/tool/call` server→client request to the broker and reply with the
   *  neutral result. The reply is sent ONLY AFTER the broker settles (a broker deny is a
   *  FAILED tool RESULT — `success:false` — not a JSON-RPC error, mirroring the M0
   *  policy-broker). Authority is the broker's; the harness binds nothing itself. */
  private async routeToolCall(transport: CodexTransport, requestId: number | string, params: unknown): Promise<void> {
    const p = asObject(params) ?? {};
    // Origin is computed from the RAW parsed thread id ONLY — never folded to
    // this.threadId. An absent/non-string threadId can therefore NEVER become "root":
    // it fails CLOSED to "unknown" (matching e2e/codex-m0/policy-broker.mjs), so a
    // missing id can neither latch a signal nor delegate. The raw value (or "" when
    // absent) is what we hand the broker's `rt`, so the broker's OWN ingestion denies an
    // absent/empty id — we do not substitute this.threadId into that identity.
    const rawThreadId = asString(p.threadId);
    const threadId = rawThreadId ?? "";
    const turnId = asString(p.turnId) ?? "";
    const callId = asString(p.callId) ?? "";
    const origin: CallbackOrigin = rawThreadId !== undefined && rawThreadId === this.threadId ? "root" : "unknown";
    let result: CallbackResult;
    try {
      result = await this.broker.handleToolCall({ threadId, turnId, callId }, p.tool, p.arguments, origin);
    } catch {
      result = { ok: false, code: "broker_error", message: "the callback failed" };
    }
    try {
      transport.respond(requestId, this.replyOf(result));
    } catch {
      // The transport may already be closed (an owner abort/close raced this pending
      // broker callback): a reply that can no longer be delivered is dropped, NEVER thrown
      // into a closed transport.
    }
  }

  /** Map a broker {@link CallbackResult} to the app-server tool reply shape
   *  (`{ result: { success, contentItems } }`, mirroring e2e/codex-m0/policy-broker.mjs).
   *  The stable machine `code` and human `message` are already bounded by the broker; a
   *  denial is returned as `success:false` so it lands as a failed tool result. */
  private replyOf(result: CallbackResult): { result: unknown } {
    const text = result.ok ? this.stringifyOutput(result.output) : result.message;
    return { result: { success: result.ok, contentItems: [{ type: "inputText", text }] } };
  }

  private stringifyOutput(output: unknown): string {
    if (typeof output === "string") return output;
    try {
      return JSON.stringify(output) ?? "";
    } catch {
      return "";
    }
  }

  /** Decode an `item/completed` note carrying assistant content into a `frame`; return
   *  undefined for a non-assistant item (the caller yields `activity`). Usage and model
   *  ride the frame (call basis); token normalization is deferred, so only the raw usage
   *  is retained in the wire capsule (unknown fields absent, never 0). */
  private decodeItemFrame(note: Extract<CodexNotification, { kind: "activity" }>): HarnessEvent | undefined {
    if (note.method !== "item/completed") return undefined;
    const item = asObject(asObject(note.params)?.item);
    if (!item) return undefined;
    const items = this.decodeItemContent(item);
    if (items.length === 0) return undefined;
    const usageObj = asObject(item.usage);
    const usage: HarnessUsage | undefined =
      usageObj !== undefined ? { basis: "call", tokens: {}, wire: { usage: usageObj } } : undefined;
    return {
      kind: "frame",
      origin: { kind: "main" },
      attribution: {},
      items,
      usage,
      model: this.currentModel,
      sessionId: this.threadId,
    };
  }

  // PROVISIONAL item-type strings. The exact app-server `item.type` values below
  // ("agentMessage"/"assistantMessage"/"agent_message"/"reasoning") are NOT yet confirmed
  // against a real app-server — they are the current best guess. They MUST be verified in
  // the packaged integration (m3b:packaged) before they are trusted; do NOT add or rename
  // a type here on a guess. An unrecognized type intentionally falls through to `[]`, which
  // the caller surfaces as `activity` (never a frame) — the safe default.
  private decodeItemContent(item: Record<string, unknown>): HarnessItem[] {
    const type = asString(item.type);
    if (type === "agentMessage" || type === "assistantMessage" || type === "agent_message") {
      return extractText(item).map((text) => ({ kind: "text", text }));
    }
    if (type === "reasoning") {
      return extractText(item).map((text) => ({ kind: "thinking", text }));
    }
    return [];
  }

  /** Decode a `turn/completed` note into a neutral {@link HarnessTerminal}. Outcome is
   *  success only for status `completed`; anything else (incl. absent) is fail-closed
   *  `failed`. A terminal PROVIDER failure is DATA here — it is never also thrown. */
  private decodeTerminal(note: Extract<CodexNotification, { kind: "turn_completed" }>): HarnessTerminal {
    const turn = asObject(asObject(note.params)?.turn);
    const status = note.status ?? asString(turn?.status) ?? "unknown";
    const outcome: "success" | "failed" = status === "completed" ? "success" : "failed";
    const usageObj = asObject(turn?.usage);
    const usage: HarnessUsage | undefined =
      usageObj !== undefined ? { basis: "turn", tokens: {}, wire: { usage: usageObj } } : undefined;
    const errors: string[] = [];
    const err = turn?.error;
    if (typeof err === "string" && err.length > 0) errors.push(err);
    return {
      outcome,
      subtype: status,
      errors,
      usage,
      metrics: { cost: { kind: "unreported" } },
      failure: {
        // Deferred, invoked only at the owner's classification point. Codex M3 carries no
        // limit facts, so this constructs the generic terminal exception; it never invents
        // an auth/model/effort category from a provider status.
        materialize: (_limit): HarnessThrownFailure => {
          const original = new Error(`codex turn failed: ${status}`);
          return { failure: { category: "unknown", message: original.message }, original };
        },
      },
    };
  }
}
