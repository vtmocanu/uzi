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
// SECURITY: pinned app-server auth receives the provider credential only through its
// private construction input and exchanges it on the initialize/login/refresh transport
// lane. The transitional env credential path is mutually exclusive. Neither path exposes
// auth to a thread/turn param, model callback broker, tool result or log line. This module
// NEVER logs raw frames, tokens or bodies.
//
// EVENT DECODE (app-server frame → neutral event), authoritative rules 1/3/8/9:
//   thread/started            → `initialized` (the model-bearing init; carries the
//                               configured model, distinct from a bare turn-start)
//   turn/started              → `activity`
//   item/completed (agent msg)→ `frame` (text/thinking items + call-basis usage + model)
//   item/* (deltas, other)    → `activity`  (Codex item updates are activity)
//   item/tool/call (id-bearing)→ intercepted: broker route + `transport.respond`; a DENIED,
//                               non-signal, delegation or child/foreign/stale call is yielded
//                               as `activity` (NOT a frame), while an ACCEPTED ROOT SIGNAL call
//                               (submit_plan/signal_done/checkpoint/progress/questions) ALSO
//                               yields a main-origin `frame` carrying the scanned `signals` so
//                               the run-lane reducer folds them (the model reply is unchanged)
//   auth refresh request      → narrow auth owner (never broker), then `activity`
//   other server→client req   → fail-closed JSON-RPC method error, then `activity`
//   turn/completed            → `turn_finished` (decoded terminal; success/failed by
//                               status), then the iterator closes
// ERROR PRECEDENCE (run-lane, rule 9): a local watchdog/cancel (the owner's aborted
// `request.signal`) ends the stream first; a transport/iterator throw propagates next; a
// terminal provider failure is represented ONCE as `turn_finished` data and NEVER also
// thrown; a clean EOF with no terminal is Codex's own unexpected-EOF → a `protocol`
// {@link CodexHarnessError} throw (Claude's clean-EOF `exhausted` is unaffected).

import { renderCodexRun } from "./render.js";
import { buildCodexDynamicTools } from "./dynamic-tools.js";
import { CODEX_DELEGATE_TOOLS, CODEX_SIGNAL_TOOLS, canonicalizeCodexToolName } from "./broker.js";
import { normalizeCodexStatus, normalizeCodexTerminalErrors, normalizeCodexUsage } from "./terminal-normalize.js";

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
  ProcessReap,
  RunHarness,
  RunTurnRequest,
  SessionPresence,
  ToolDisposal,
  TurnSignals,
} from "../harness.js";
import type { CallbackResult, CodexCallbackBroker } from "./broker.js";
import type { CodexAppServerAuthSession } from "./appserver-auth.js";
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
 *  carries only trusted, launcher-fixed values plus the per-run home/workspace. Managed
 *  app-server auth keeps the credential out of this shape; the optional legacy
 *  transitional credential remains private and never becomes model-visible. */
export interface CodexLaunchRootSpec {
  readonly kind: "provider";
  readonly provider: CodexProviderConfig;
  readonly model: string;
  /** The canonical project / launch cwd (provisioned untrusted). */
  readonly cwd: string;
  /** The runner-writable owned data root for the per-launch HOME/CODEX_HOME/XDG trees. */
  readonly ownedDataRoot: string;
  /** Executor-owned resume bit. Production derives the only allowed staging path from
   * ownedDataRoot; no caller- or model-supplied source path crosses the launcher. */
  readonly seedSession?: boolean;
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

/**
 * A per-child-thread demux sink (PRD #1171 m3, part C). The m2
 * {@link CodexDelegationRunner} needs a per-child `ChildThreadController.notifications()`
 * over the SAME app-server transport, but `CodexTransport.notifications()` is
 * single-consumer (owned by the root loop). So a delegation callback registers a sink for
 * its child thread id via {@link CodexHarness.registerChildSink}; the root loop routes
 * every frame carrying that thread id into the sink and leaves every root frame on the root
 * loop. When no sink is registered the root path is BYTE-IDENTICAL to before the demux.
 */
export interface CodexChildSink {
  /** Deliver one demuxed child-thread frame to the child controller's stream. */
  push(note: CodexNotification): void;
}

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
  /** Pinned app-server authentication owner. This narrow transport seam is never
   *  exposed to the callback broker or model. */
  readonly appServerAuth?: CodexAppServerAuthSession;
  /** Transitional pre-auth integration input. Mutually exclusive with appServerAuth. */
  readonly credentialValue?: string;
}

// --- small pure helpers -------------------------------------------------------

function asObject(v: unknown): Record<string, unknown> | undefined {
  return v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined;
}

function asString(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}

/** The app-server thread id a decoded notification carries, or undefined when it carries
 *  none. Lifecycle frames carry it top-level; `activity` frames (item/completed,
 *  item/tool/call, deltas) carry it in `params.threadId`. Used by the child-sink demux to
 *  decide whether a frame belongs to a delegated child thread. */
function noteThreadId(note: CodexNotification): string | undefined {
  if (note.kind !== "activity") return note.threadId;
  const params = asObject(note.params);
  return asString(params?.threadId);
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

/** A small best-effort deadline (ms) for tearing down a just-launched root that FAILED
 *  registry admission. The launch is already being failed; this only bounds the cleanup
 *  of the unadmitted root so it cannot hang the setup throw. */
const REGISTRY_ADMISSION_TEARDOWN_DEADLINE_MS = 1000;

/** What {@link CodexHarness.routeToolCall} hands back to {@link CodexHarness.mapNote} on the
 *  INLINE path when the broker ACCEPTED a TRUSTED ROOT workflow-signal callback: the scanned
 *  `signals` the reducer folds. It exists ONLY for a signal tool the active root turn owns —
 *  every non-signal, denied, delegation or non-active-turn outcome returns `undefined`, so a
 *  signals frame is emitted only where the authority gates already admitted a root signal. */
interface RoutedRootSignal {
  readonly signals: Readonly<Partial<TurnSignals>>;
}

// --- the harness --------------------------------------------------------------

export class CodexHarness implements RunHarness {
  readonly kind = "codex" as const;

  private readonly registry: ExecutionRegistry;
  private readonly launchRootSeam: LaunchRootSeam;
  // The active callback broker. NOT `readonly`: turns run strictly sequentially, so the
  // owner re-points it BETWEEN turns via {@link useBroker} to make the broker's IMMUTABLE
  // per-(thread,turn) grants genuinely PER-TURN (phase-correct) instead of frozen at
  // construction. Within a turn it is stable (a turn fully settles before the next starts).
  private broker: CodexCallbackBroker;
  private readonly provider: CodexProviderConfig;
  private readonly workspace: string;
  private readonly homeDir: string;
  private readonly log: Logger;
  private readonly render: (request: RunTurnRequest) => RenderedCodexRun;
  private readonly sessionInspect: SessionInspectSeam;
  private readonly appServerAuth?: CodexAppServerAuthSession;
  // Transitional private input; never combined with the pinned app-server auth path.
  private readonly credentialValue?: string;

  // Run-level provider root state (launched once, reused across turns; a reused
  // notifications() iterator is single-consumer so it is obtained exactly once).
  private transport?: CodexTransport;
  private notes?: AsyncIterableIterator<CodexNotification>;
  // At most one notifications().next() may be outstanding. If a turn aborts while
  // it is pending, the promise is retained and consumed by the next turn rather than
  // abandoning a waiter that would silently eat the next provider notification.
  private pendingNote?: Promise<IteratorResult<CodexNotification>>;
  private threadId?: string;
  private currentModel?: string;
  private closed = false;

  // Child-thread demux (part C): a registered sink receives every frame carrying its
  // child thread id off the SAME transport, so a delegation's child turn can consume its
  // own notifications while the root loop keeps reading. Empty on the non-delegation path.
  private readonly childSinks = new Map<string, CodexChildSink>();
  // Concurrently-running DELEGATION callbacks. A delegate callback drives a child turn
  // whose frames this root loop demuxes, so it CANNOT be awaited inline (that would
  // deadlock the transport read). It runs in the background, tracked here, and the turn
  // stream flushes it before ending. Every non-delegate callback stays inline.
  private readonly pendingToolCalls = new Set<Promise<unknown>>();

  // Per-turn state (turns run strictly sequentially).
  private activeTurnId?: string;
  private turnClosed = false;
  private terminalEmitted = false;
  private stopRequested = false;
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
    if (opts.appServerAuth !== undefined && opts.credentialValue !== undefined) {
      throw new CodexHarnessError({
        category: "protocol",
        message: "codex harness received conflicting authentication inputs",
      });
    }
    this.appServerAuth = opts.appServerAuth;
    this.credentialValue = opts.credentialValue;
  }

  inspectSession(id: string): Promise<SessionPresence> {
    return this.sessionInspect(id);
  }

  /** Re-point the callback broker used by the NEXT turn. The provider root, transport and
   *  the harness itself are constructed ONCE and reused; only the broker (its immutable
   *  per-(thread,turn) grants) changes per turn. Turns run strictly sequentially — the
   *  next turn's root frames only begin after this returns — so re-pointing between turns
   *  is race-safe: a root callback reads `this.broker` fresh at dispatch and a stale
   *  prior-turn root callback is rejected `not_active_turn`, while an in-flight CHILD
   *  callback holds its own child broker captured at spawn (this swap never touches it).
   *  Within a turn `this.broker` is stable. The executor uses it
   *  to serve each turn a PHASE-CORRECT broker (a plan-phase broker denies every file
   *  write; the implement-phase broker permits them), so the plan turn cannot mutate the
   *  worktree before its plan is approved. This STRENGTHENS the per-turn grant invariant:
   *  the grants are genuinely per-turn, not one implement-phase set frozen at construction. */
  useBroker(broker: CodexCallbackBroker): void {
    this.broker = broker;
  }

  // --- child-thread demux (part C) ---------------------------------------------
  // The delegation seam (built by the CodexExecutor) uses these to run a child turn on
  // the SAME provider transport as the root: it starts the child thread via
  // {@link requestOnTransport}, registers a sink for the child thread id so the root loop
  // routes the child's frames into the child controller, and answers the child's tool
  // callbacks via {@link respondOnTransport}. Root frames are untouched, so a run with no
  // subagents behaves exactly as before the demux.

  /** Route frames carrying `threadId` into `sink` instead of the root loop. */
  registerChildSink(threadId: string, sink: CodexChildSink): void {
    this.childSinks.set(threadId, sink);
  }

  /** Stop routing frames for `threadId` to a child sink (the child turn is done). */
  unregisterChildSink(threadId: string): void {
    this.childSinks.delete(threadId);
  }

  /** Send a request on the shared provider transport. The child-thread seam uses it to
   *  start/interrupt a child thread on the SAME app-server; the child's frames are
   *  demuxed back via {@link registerChildSink}. Rejects if the provider root is not
   *  launched (a child turn is only ever started from inside an active root turn). */
  requestOnTransport<T = unknown>(method: string, params?: unknown, opts?: { signal?: AbortSignal }): Promise<T> {
    const transport = this.transport;
    if (!transport) {
      return Promise.reject(new CodexHarnessError({ category: "protocol", message: "codex provider root is not launched" }));
    }
    return transport.request<T>(method, params, opts);
  }

  /** Answer a child server→client tool-call on the shared transport. Best-effort: an
   *  undeliverable reply into a closed transport is dropped, never thrown (mirrors the
   *  root {@link routeToolCall} reply guard). */
  respondOnTransport(
    requestId: number | string,
    response: { readonly result: unknown } | { readonly error: { readonly code: number; readonly message: string } },
  ): void {
    try {
      this.transport?.respond(requestId, response);
    } catch {
      /* the transport may already be closed; the child settles via its own terminal/abort */
    }
  }

  startTurn(request: RunTurnRequest): HarnessTurn {
    if (this.closed) {
      throw new CodexHarnessError({ category: "transport", message: "codex harness is closed" });
    }
    // Render synchronously so a render throw surfaces to the caller here; the rest of
    // the setup (launch, thread/start, turn/start) is deferred into the events
    // generator so a setup throw surfaces to the owner's for-await (its trip > throw >
    // terminal precedence handles it), exactly as the Claude adapter defers query
    // creation.
    const rendered = this.render(request);
    this.turnClosed = false;
    this.terminalEmitted = false;
    this.activeTurnId = undefined;
    this.stopRequested = false;
    this.stopTurn = undefined;

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
    this.stopRequested = true;
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
    // End the stream even when it is wedged in a pending broker callback. The sticky
    // stopRequested bit also covers the lazy-generator window before stopTurn exists.
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
   *  reap — that is quiesce/reap/dispose. Public so the executor's terminal cleanup can
   *  close the transport (rejecting any straggler request) after the registry reaps the
   *  provider root; the per-turn `HarnessTurn.close` seam routes here too. */
  async close(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    this.turnClosed = true;
    this.stopRequested = true;
    // Wake a run loop wedged in a pending broker callback so the generator returns rather
    // than hang on that await while the transport is torn down underneath it.
    this.stopTurn?.();
    this.appServerAuth?.closeAdmissionAndCancel();
    try {
      await this.transport?.close();
    } finally {
      await this.appServerAuth?.drainInterceptedRequests();
    }
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
    let settleStop: (() => void) | undefined;
    const abortPromise = new Promise<"aborted">((resolve) => {
      settleStop = (): void => resolve("aborted");
      this.stopTurn = settleStop;
      if (request.signal.aborted || this.stopRequested) {
        resolve("aborted");
        return;
      }
      onAbort = (): void => resolve("aborted");
      request.signal.addEventListener("abort", onAbort, { once: true });
    });

    try {
      // startTurn returns a lazy iterable. A stop may arrive before its first next();
      // honor that request without launching a provider root or starting model work.
      if (request.signal.aborted || this.stopRequested) {
        this.turnClosed = true;
        return;
      }
      // 1. Ensure the provider root + transport (launched once, reused after).
      await this.ensureRoot(request.signal);
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
      if (request.signal.aborted || this.stopRequested) {
        // A stop during launch/thread/turn setup could not name a turn earlier. Now
        // that activeTurnId exists, issue the best-effort interrupt and end cleanly.
        this.endTurnOnStop();
        return;
      }

      // 4. Consume the notification stream, mapping each raw frame to ONE neutral event.
      for (;;) {
        if (this.turnClosed) return;
        const notePromise = this.pendingNote ?? notes.next();
        this.pendingNote = notePromise;
        let step: IteratorResult<CodexNotification> | "aborted";
        try {
          step = await Promise.race([notePromise, abortPromise]);
        } catch (error) {
          if (this.pendingNote === notePromise) this.pendingNote = undefined;
          throw error;
        }
        if (step === "aborted") {
          // Owner cancel/watchdog wins: interrupt the turn and end the stream cleanly.
          this.endTurnOnStop();
          return;
        }
        if (this.pendingNote === notePromise) this.pendingNote = undefined;
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
        // CHILD-THREAD DEMUX (part C). A frame carrying a REGISTERED child thread id is a
        // delegated child's frame: route its CONTENT to the child controller's sink and
        // NEVER map/yield that content on the root loop. But routing must still count as
        // LIVENESS for the root's idle watchdog: the root driveCodexTurn re-arms idle on
        // every event it consumes (codex-executor.ts), so a bare `continue` here would
        // starve that re-arm for the whole delegation, and a subagent turn longer than
        // `idleMs` would falsely trip REASON_IDLE while the child is actively producing.
        // So after routing, yield a CONTENT-FREE `activity` carrying ONLY the ROOT thread
        // id (no items, no child text) — enough to re-arm idle without leaking any child
        // content onto the root frame stream. The reducer treats `activity` as pure
        // liveness (no message, no frame). The `size > 0` guard keeps the non-delegation
        // path (no child sinks) BYTE-IDENTICAL to before this edit.
        if (this.childSinks.size > 0) {
          const childId = noteThreadId(step.value);
          if (childId !== undefined) {
            const sink = this.childSinks.get(childId);
            if (sink !== undefined) {
              sink.push(step.value);
              yield { kind: "activity", sessionId: this.threadId };
              continue;
            }
          }
        }
        // The broker-callback await (mapNote → routeToolCall → broker.handleToolCall) is
        // itself raced against the owner abort — WITHOUT this, a never-settling broker seam
        // (a model-selected long-running shell) leaves the turn un-cancellable, falsifying
        // rule 9. On abort while a callback is pending we STOP awaiting it and end the
        // stream promptly; the pending effect's process cleanup is the registry's reap job
        // at the safety boundary, and a late broker reply is guarded in routeToolCall so it
        // never throws into a closed transport. The happy path is unchanged: a callback
        // that settles normally still replies via transport.respond after the broker settles.
        const mapped = await Promise.race([this.mapNote(transport, step.value, request.signal), abortPromise]);
        if (mapped === "aborted") {
          this.endTurnOnStop();
          return;
        }
        yield mapped;
        if (mapped.kind === "turn_finished") {
          // Flush any concurrently-running delegation callbacks: each drives a child turn
          // that settles (and replies) before this resolves, so the parent spawn_agent
          // callback has responded by the time the root turn stream closes. A wedged
          // child self-bounds via its own per-child deadline (delegation.ts), so this
          // await is finite. The non-delegation path has an empty set and never waits.
          if (this.pendingToolCalls.size > 0) await Promise.allSettled(this.pendingToolCalls);
          return; // close the iterator after the terminal
        }
      }
    } finally {
      if (onAbort) request.signal.removeEventListener("abort", onAbort);
      if (this.stopTurn === settleStop) this.stopTurn = undefined;
    }
  }

  private async ensureRoot(signal?: AbortSignal): Promise<void> {
    if (this.transport) {
      // A prior startup may have failed after the root was registered. Re-enter the
      // pinned auth owner so its poison state remains the explicit failure, and never
      // proceed merely because a transport object exists.
      await this.appServerAuth?.authenticate(this.transport, signal);
      if (!this.notes) this.notes = this.transport.notifications();
      return;
    }
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
        credentialValue: this.appServerAuth === undefined ? this.credentialValue : undefined,
      });
    } catch (error) {
      // The launch aborted before producing a root: settle the reservation so it does
      // not poison the epoch at quiesce, then re-throw the setup failure.
      this.registry.cancelReservation(reservation.reservation);
      throw error;
    }
    const admission = this.registry.registerRoot(reservation.reservation, launched.root);
    if (!admission.ok) {
      // The registry POISONED on a kind / unknown-reservation mismatch: this root was
      // never admitted, so model work must NOT proceed on it. Do NOT assign transport/
      // notes; best-effort tear the just-launched root down (a poisoned epoch will not
      // reap it), then fail closed. A close/dispose throw is swallowed — we are already
      // failing the launch and only bound the cleanup.
      await launched.transport.close().catch(() => {});
      await launched.root.dispose(REGISTRY_ADMISSION_TEARDOWN_DEADLINE_MS).catch(() => {});
      throw new CodexHarnessError({ category: "protocol", message: "codex provider root failed registry admission" });
    }
    this.transport = launched.transport;
    // The pinned initialize/initialized/login sequence completes before any thread/model
    // work. The auth owner has no reference to the model callback broker.
    await this.appServerAuth?.authenticate(launched.transport, signal);
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
        // Run roots must persist their rollout under CODEX_HOME so approval and
        // cooperative-checkpoint root recreation can adopt it and thread/resume.
        // Child and advice threads remain ephemeral because they are never resumed.
        ephemeral: false,
        environments: [],
        dynamicTools: buildCodexDynamicTools(rendered.leadGrants),
        config: this.threadConfig(),
        developerInstructions: rendered.leadPrompt.systemPrompt,
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
        developerInstructions: rendered.leadPrompt.systemPrompt,
      },
      { signal: request.signal },
    );
    const id = res?.thread?.id;
    if (typeof id !== "string" || id.length === 0) {
      throw new CodexHarnessError({ category: "protocol", message: "codex thread/resume returned no thread id" });
    }
    return id;
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
  private async mapNote(
    transport: CodexTransport,
    note: CodexNotification,
    signal?: AbortSignal,
  ): Promise<HarnessEvent> {
    switch (note.kind) {
      case "thread_started":
        // The model-bearing init: carries the model the harness configured, distinct
        // from the bare turn-start below. Bind it to the ACTIVE ROOT thread: a child
        // `thread/started` (a delegated subagent's) must never latch the root session id
        // or emit a root `initialized`. The demux already routes a registered child's
        // frames away, so this is defense-in-depth for the pre-registration window and any
        // stray/foreign thread id — such a frame is liveness only.
        if (note.threadId !== this.threadId) {
          return { kind: "activity", sessionId: note.threadId };
        }
        return { kind: "initialized", model: this.currentModel, sessionId: note.threadId };
      case "turn_started":
        return { kind: "activity", sessionId: note.threadId };
      case "turn_completed": {
        // The harness serves ONLY the ACTIVE root turn. A terminal for a stale turn id or
        // a child/foreign thread is liveness ONLY — it must never latch `terminalEmitted`
        // or emit `turn_finished`, or a stale/child completion could end the root turn.
        // Keeping the loop waiting also means a later clean EOF without the REAL terminal
        // still protocol-throws (unexpected-EOF), never a fabricated success.
        if (note.threadId !== this.threadId || note.turnId !== this.activeTurnId) {
          return { kind: "activity", sessionId: note.threadId };
        }
        // A same-chunk refresh is intercepted before this terminal reaches the queue.
        // Do not publish success until every accepted auth operation settles; a sticky
        // protocol poison throws here instead of allowing the terminal through.
        await this.appServerAuth?.drainInterceptedRequests();
        const terminal = this.decodeTerminal(note);
        this.terminalEmitted = true;
        return { kind: "turn_finished", terminal, sessionId: note.threadId };
      }
      case "activity": {
        if (note.requestId !== undefined) {
          // Refresh is the only non-tool request admitted here. It is consumed by the
          // narrow auth owner before the broker lane, so no credential authority becomes
          // model-visible through a callback.
          if (await this.appServerAuth?.handleServerRequest(transport, note, signal)) {
            return { kind: "activity", sessionId: this.threadId };
          }
          // A server→client request. Only the tool-call lane routes to the broker; any
          // other server-initiated request is answered fail-closed as unsupported.
          if (note.method === "item/tool/call") {
            const routed = await this.routeToolCall(transport, note.requestId, note.params);
            if (routed !== undefined) {
              // A TRUSTED ROOT signal callback the broker ACCEPTED (submit_plan/signal_done/
              // checkpoint/report_progress/questions): surface its scanned signals as a
              // main-origin frame so the run-lane reducer's foldSignals fires. The model reply
              // already went out via routeToolCall (replyOf) — this frame is IN ADDITION, never
              // instead. Every other tool call (denied, non-signal, delegation, or a child/
              // foreign/stale identity) returns undefined and falls through to the
              // byte-identical `activity` below.
              return {
                kind: "frame",
                origin: { kind: "main" },
                attribution: {},
                items: [],
                signals: routed.signals,
                model: this.currentModel,
                sessionId: this.threadId,
              };
            }
          } else {
            try {
              transport.respond(note.requestId, { error: { code: -32601, message: "unsupported request" } });
            } catch {
              // The owner may close the run-scoped transport while this reply is
              // being prepared. An undeliverable fail-closed response is dropped.
            }
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
   *  neutral result. The harness FIRST binds the request fail-closed to the ACTIVE
   *  (threadId, activeTurnId): only a callback for the active root turn reaches the broker
   *  (origin `"root"`); any other identity is denied here with a bounded FAILED tool result
   *  and the broker is never called. When the broker IS called, the reply is sent ONLY AFTER
   *  it settles (a broker deny is a FAILED tool RESULT — `success:false` — not a JSON-RPC
   *  error, mirroring the M0 policy-broker). Authority over WHAT a matched callback may do
   *  stays the broker's; the harness only gates WHICH turn's callbacks reach it.
   *
   *  RETURN: a {@link RoutedRootSignal} ONLY when the accepted callback was a TRUSTED ROOT
   *  workflow signal (the caller surfaces its scanned signals on a main-origin frame the
   *  reducer folds); `undefined` for every other outcome — a stale/foreign/absent identity, a
   *  denied signal, a backgrounded DELEGATION, or a non-signal effect (shell/file/mcp) — none
   *  of which may move the run's workflow, so none emits a signals frame. The model reply is
   *  identical in every case (via {@link replyOf}); the return only carries the fold input. */
  private async routeToolCall(
    transport: CodexTransport,
    requestId: number | string,
    params: unknown,
  ): Promise<RoutedRootSignal | undefined> {
    const p = asObject(params) ?? {};
    // A callback is served ONLY for the ACTIVE root turn. We bind on BOTH the raw thread
    // id AND the raw turn id (never folded to this.threadId/activeTurnId), so an absent/
    // stale/foreign identity fails CLOSED. Binding on the turn as well as the thread closes
    // TWO gaps at once: a stale-turn root-signal latch (right thread, wrong turn) and a
    // child/foreign-thread effect running under the root's grants. On ANY mismatch we do
    // NOT call the broker (no effect ever runs) and reply with a bounded FAILED tool result.
    const rawThreadId = asString(p.threadId);
    const rawTurnId = asString(p.turnId);
    const threadId = rawThreadId ?? "";
    const turnId = rawTurnId ?? "";
    const callId = asString(p.callId) ?? "";
    const matchesActive =
      rawThreadId !== undefined &&
      rawThreadId === this.threadId &&
      rawTurnId !== undefined &&
      rawTurnId === this.activeTurnId;
    if (!matchesActive) {
      this.safeRespond(transport, requestId, { ok: false, code: "not_active_turn", message: "callback does not match the active turn" });
      return undefined;
    }
    const runAndReply = async (): Promise<CallbackResult> => {
      let result: CallbackResult;
      try {
        // A matched callback is, by construction, the root turn's — origin is "root".
        result = await this.broker.handleToolCall({ threadId, turnId, callId }, p.tool, p.arguments, "root");
      } catch {
        result = { ok: false, code: "broker_error", message: "the callback failed" };
      }
      this.safeRespond(transport, requestId, result);
      return result;
    };
    const toolName = asString(p.tool);
    const canonical = toolName !== undefined ? canonicalizeCodexToolName(toolName) : undefined;
    // A DELEGATION callback (spawn_agent / the collaboration*/Subagent* family) drives a
    // CHILD turn whose frames THIS root loop demuxes off the SAME transport
    // (registerChildSink). It therefore MUST run CONCURRENTLY with continued note
    // consumption — awaiting it inline would deadlock, because the child's frames would
    // never be read. It runs in the background, tracked in `pendingToolCalls`, and the
    // turn stream flushes it before ending; the broker's own delegate seam guarantees the
    // child settles before this callback (the parent spawn_agent) resolves. A delegation is
    // never a workflow signal, so it emits NO signals frame. EVERY OTHER callback is awaited
    // inline exactly as before, so the non-delegation path is byte-identical (the abort race
    // in runTurn still bounds a wedged inline callback).
    if (canonical !== undefined && CODEX_DELEGATE_TOOLS.has(canonical)) {
      const task = runAndReply();
      this.pendingToolCalls.add(task);
      void task.finally(() => this.pendingToolCalls.delete(task));
      return undefined;
    }
    const result = await runAndReply();
    // Route a TRUSTED ROOT SIGNAL callback's scanned result into the run-lane reducer: when
    // the tool canonicalizes to a signal tool AND the broker ACCEPTED it, `result.output` is
    // the scanned Partial<TurnSignals> (broker.dispatchSignal returns `{ok:true, output:
    // scanned}`), which the caller surfaces on a main-origin signals frame. A DENIED signal
    // (result.ok === false) folds nothing; a non-signal effect (shell/file/mcp) is undefined.
    if (canonical !== undefined && CODEX_SIGNAL_TOOLS.has(canonical) && result.ok) {
      return { signals: result.output as Readonly<Partial<TurnSignals>> };
    }
    return undefined;
  }

  /** Reply to a server→client tool-call with a broker {@link CallbackResult}, mapped to the
   *  app-server reply shape. Best-effort: an undeliverable reply into a closed transport
   *  (an owner abort/close raced this pending callback) is dropped, NEVER thrown. */
  private safeRespond(transport: CodexTransport, requestId: number | string, result: CallbackResult): void {
    try {
      transport.respond(requestId, this.replyOf(result));
    } catch {
      /* transport closed underneath a late reply; the run settles via its terminal/abort */
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
   *  undefined for a non-assistant item OR a non-active-thread item (the caller yields
   *  `activity`). Usage and model ride the frame (call basis), bounded/redacted through
   *  {@link normalizeCodexUsage} so no raw provider blob is retained. */
  private decodeItemFrame(note: Extract<CodexNotification, { kind: "activity" }>): HarnessEvent | undefined {
    if (note.method !== "item/completed") return undefined;
    const params = asObject(note.params);
    // Bind the frame to the ACTIVE root thread. `item/completed` carries NO turnId (only a
    // threadId), so we bind on threadId ONLY: the item's own thread id must be PRESENT and
    // EQUAL to this.threadId. A child/foreign/absent thread id yields undefined so the
    // caller surfaces `activity`, never a {origin:{kind:"main"}} frame for a non-root thread.
    if (params === undefined || params.threadId !== this.threadId) return undefined;
    const item = asObject(params.item);
    if (!item) return undefined;
    const items = this.decodeItemContent(item);
    if (items.length === 0) return undefined;
    const usage = normalizeCodexUsage(item.usage, "call");
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

  // VERIFIED 2026-09-10: the native both-image packaged proof drove pinned Codex
  // 0.153.2 and confirmed its active agent-message form decodes to a non-empty public
  // frame. The alternate spellings and reasoning arm remain compatibility cases; do
  // not add or rename a type on a guess. An unrecognized type intentionally falls
  // through to `[]`, which the caller surfaces as `activity` (never a frame).
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
    const rawStatus = note.status ?? asString(turn?.status);
    // Normalize through the single-source-of-truth module so no raw provider field
    // (arbitrary status, secret-bearing turn.error, or a multi-megabyte usage blob) is
    // ever retained: subtype/outcome come from the CLOSED vocabulary, errors are provider-
    // text-free, and usage is a bounded numeric-only subset.
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
          const original = new Error(`codex turn failed: ${subtype}`);
          return { failure: { category: "unknown", message: original.message }, original };
        },
      },
    };
  }
}
