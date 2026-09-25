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
//                               the run-lane reducer folds them (the model reply is unchanged).
//                               An ACTIVE-TURN non-signal, non-delegation root call is ALSO
//                               projected (issue #1583) as a lead tool started/finished frame
//                               pair, queued on the turn's outbox and drained ahead of its
//                               `activity`. An active-turn DELEGATION is projected as a lead
//                               "Agent" dispatch/completion pair around its child's
//                               subagent-origin frames (bindChildDispatch / emitChildFrame)
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
import {
  formatCodexClassification,
  normalizeCodexErrorInfo,
  normalizeCodexStatus,
  normalizeCodexTerminalErrors,
  normalizeCodexUsage,
  pickCodexClassification,
  type CodexErrorClassification,
} from "./terminal-normalize.js";
import { CodexUsageAccountant, deriveCodexRunCost } from "./token-accounting.js";
import {
  childProjectedId,
  newProjectionNonce,
  projectedId,
  projectLabel,
  projectOutputValue,
  projectText,
  projectToolInput,
  projectToolName,
  projectToolOutput,
  type ProjectionScrub,
} from "./projection.js";

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
  TurnSignals,
} from "../harness.js";
import type { CallbackResult, CodexCallbackBroker } from "./broker.js";
import type { CodexAppServerAuthMode, CodexAppServerAuthSession } from "./appserver-auth.js";
import type { ExecutionRegistry, RegisteredRoot } from "./registry.js";
import { CodexTransportError, type CodexNotification, type CodexTransport } from "./transport.js";
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

/** A rejected thread/resume, with no provider response content exposed to callers. */
export class CodexResumeError extends Error {
  constructor() {
    super("codex thread/resume failed");
    this.name = "CodexResumeError";
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
  /** PRD #1332 C4b / D5: the RUN's immutable credential auth mode, from the binding. It selects
   *  the terminal cost semantics — a subscription run's per-model usage is `subscription` (no
   *  per-token charge), an api_key run's is `metered` (versioned Standard price table) or
   *  `unreported`. When ABSENT (a transitional/test construction that supplies no mode) the
   *  terminal prices NOTHING — every entry stays `unreported` and the run cost `unreported` — the
   *  conservative choice that retains tokens and never invents a subscription or a dollar figure. */
  readonly authMode?: CodexAppServerAuthMode;
  /** PRD #1332 C4a / CodeRabbit 4004800880: the executor-claim-leg token accountant, INJECTED so it
   *  survives provider-epoch recreation (plan approval, cooperative-checkpoint reaps). Each epoch
   *  builds a FRESH {@link CodexHarness}, but the accountant keeps the claim leg's cumulative
   *  per-thread reconciliation. A later worker claim constructs a new executor and accountant, and
   *  its explicit init marker gives that resumed delta a new server lineage. When ABSENT (a
   *  single-epoch or test construction) this defaults to a fresh accountant. */
  readonly accountant?: CodexUsageAccountant;
  /** Emit this executor claim leg's one explicit `initialized` event. The first provider epoch sets
   *  this true; recreated internal epochs set it false, so each worker claim creates exactly one
   *  persisted usage-lineage marker regardless of app-server `thread/started` behavior. */
  readonly emitClaimInit?: boolean;
  /** Issue #1583: the redactor applied to every PROJECTED tool input/output string BEFORE it is
   *  bounded (the executor wires the run's runtime-released Codex tokens, which the batcher's
   *  claim-secret redactor never sees). Identity when absent; the batcher still redacts at persist. */
  readonly scrubProjected?: ProjectionScrub;
  /** Issue #1583: the 12-hex-char namespace of this harness's projected tool ids. Generated from
   *  `crypto.randomBytes` when absent; injectable so a test can pin the ids. */
  readonly idNonce?: string;
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

/** PRD #1332 C4a: fold the reconciled per-model `modelUsage` into a terminal {@link
 *  HarnessUsage}. When `modelUsage` is undefined (no usage reconciled) the base usage passes
 *  through UNCHANGED, so a Codex terminal with no token-usage notes is byte-identical to before
 *  C4a. When usage is reconciled but the turn carried no `turn.usage` (base is undefined), a
 *  minimal `turn`-basis usage is created to carry `modelUsage` — its `wire.usage` stays
 *  undefined so `payload.usage` remains absent exactly as before. */
function attachModelUsage(base: HarnessUsage | undefined, modelUsage: unknown): HarnessUsage | undefined {
  if (modelUsage === undefined) return base;
  if (base === undefined) {
    return { basis: "turn", tokens: {}, wire: { usage: undefined, modelUsage } };
  }
  return { ...base, wire: { usage: base.wire?.usage, modelUsage } };
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

/** Issue #1583: one ACTIVE-TURN root delegation callback, recorded when it is routed so the child
 *  it starts can be bound to it (see {@link CodexHarness.bindChildDispatch}). `pending` until its
 *  child starts, `bound` once the lead dispatch frame was emitted, `closed` once a completion was
 *  emitted (or can no longer be: the turn stopped/ended). Removed when its callback settles. */
interface DelegationDispatch {
  readonly threadId: string;
  readonly turnId: string;
  readonly callId: string;
  /** The namespaced projected id: the lead dispatch tool_use id AND the child lane's instance. */
  readonly dispatchId: string;
  /** The scrubbed + bounded `description` arg (display only, never authority); may be "". */
  readonly label: string;
  readonly ordinal: number;
  state: "pending" | "bound" | "closed";
  /** The child binding once bound; kept after the child sink unregisters so a close can still
   *  settle its open child tools. */
  child?: ChildBinding;
}

/** A projected child tool `started` awaiting its `finished`. */
interface OpenChildTool {
  readonly id: string;
  readonly name: string;
}

/** A child thread bound to its dispatch: the projected (scrubbed, bounded) admitted role, and the
 *  per-child tool-id bookkeeping (ids unique within the child, started paired to finished). */
interface ChildBinding {
  readonly dispatch: DelegationDispatch;
  readonly role: string;
  counter: number;
  readonly issuedIds: Set<string>;
  /** Raw provider call id -> the projected starteds still open under it, oldest first: a reused
   *  raw id queues, and each `finished` closes the oldest. An empty queue is deleted. */
  readonly openTools: Map<string, OpenChildTool[]>;
  /** Projected child items / JSON bytes emitted so far for this dispatch (see
   *  {@link MAX_CHILD_ITEMS_PER_DISPATCH}); `capped` once this dispatch's or the turn's budget
   *  was exhausted and its one {@link CHILD_OUTPUT_CAPPED} marker went out. */
  items: number;
  bytes: number;
  capped: boolean;
}

/** The synthesized lead completion for a bound dispatch the run stopped (abort/stop/close). */
const DISPATCH_STOPPED = "delegation stopped by the run; child settlement not confirmed";
/** The synthesized lead completion for a bound dispatch still open when its turn finished. */
const DISPATCH_OPEN_AT_TURN_END = "delegation still open at turn end; child settlement not confirmed";
/** The synthesized lead completion for a bound dispatch whose turn stream failed (a protocol or
 *  transport throw, e.g. an unexpected provider EOF). */
const DISPATCH_STREAM_FAILED = "delegation ended by a provider stream failure; child settlement not confirmed";
/** The lead tool name a dispatch is projected under (the server's milestone-lane contract). */
const DISPATCH_TOOL_NAME = "Agent";
/** The per-dispatch budget of projected child items and of their JSON-serialized bytes. A child
 *  producing more (one frame with a huge part list, or many frames) gets ONE
 *  {@link CHILD_OUTPUT_CAPPED} text item and nothing further except the `finished` of a child
 *  tool whose `started` was already projected, whose output is then replaced by
 *  {@link CHILD_TOOL_OUTPUT_OMITTED}; its lead completion is unaffected. */
const MAX_CHILD_ITEMS_PER_DISPATCH = 2000;
const MAX_CHILD_BYTES_PER_DISPATCH = 4 * 1024 * 1024;
/** The same budget summed over EVERY dispatch of one turn, so N dispatches cannot project N times
 *  the per-dispatch cap. Once it is spent, each still-uncapped dispatch gets its one marker. */
const MAX_CHILD_ITEMS_PER_TURN = 10_000;
const MAX_CHILD_BYTES_PER_TURN = 16 * 1024 * 1024;
const CHILD_OUTPUT_CAPPED = "[further subagent output not shown]";
/** The output of a child tool `finished` that closes a projected `started` but does not fit the
 *  budget: the pair still closes, at a small constant size. */
const CHILD_TOOL_OUTPUT_OMITTED = "[output not shown: subagent output cap reached]";
/** The output synthesized for a child tool still open when its dispatch was closed. */
const CHILD_TOOL_UNCONFIRMED = "tool result not confirmed: delegation closed";

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
  // PRD #1332 C4b / D5: the run's immutable auth mode, used ONLY to project the terminal cost
  // semantics from the accountant. Undefined leaves the terminal price-free (unreported).
  private readonly authMode?: CodexAppServerAuthMode;

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

  // PRD #1332 C4a: the executor-claim-leg token accountant. It holds the IMMUTABLE thread->model
  // map and survives provider-epoch recreation inside this executor invocation. Each terminal emits
  // the claim leg's cumulative-since-baseline modelUsage, which one server lineage row de-duplicates
  // with GREATEST. A later worker claim gets a new accountant and a new explicit init lineage.
  private readonly accountant: CodexUsageAccountant;
  private readonly emitClaimInit: boolean;
  private claimInitEmitted = false;

  // Issue #1583: the tool-projection OUTBOX. A root callback is awaited inside mapNote, but its
  // `started` tool frame must reach the stream BEFORE the (possibly long) broker effect settles,
  // so routeToolCall pushes projected frames here and runTurn races `outboxReady()` against the
  // note/mapNote awaits, draining the queue whenever it wakes. Frames are accepted ONLY for the
  // turn whose stream is live (`liveProjectionTurn`): a wedged callback that settles after its
  // turn ended (abort/stop) can never leak a frame into a later turn's stream.
  private readonly scrubProjected: ProjectionScrub;
  private readonly idNonce: string;
  private outbox: HarnessEvent[] = [];
  private wakeOutbox?: () => void;
  private turnOrdinal = 0;
  private liveProjectionTurn?: number;
  // Per-turn id counter: the fallback for a call id empty after sanitising, and the `-n<k>`
  // suffix that disambiguates a colliding id.
  private projectionCounter = 0;
  // Ids already issued this turn: a reused call id (or two that sanitise identically) gets a
  // `-n<k>` suffix so every started/finished pair stays distinct.
  private issuedProjectionIds = new Set<string>();
  // Projected child items / JSON bytes emitted this turn across all dispatches (see
  // MAX_CHILD_ITEMS_PER_TURN); reset in startTurn.
  private childTurnItems = 0;
  private childTurnBytes = 0;

  // Child-thread demux (part C): a registered sink receives every frame carrying its
  // child thread id off the SAME transport, so a delegation's child turn can consume its
  // own notifications while the root loop keeps reading. Empty on the non-delegation path.
  private readonly childSinks = new Map<string, CodexChildSink>();
  // Concurrently-running DELEGATION callbacks. A delegate callback drives a child turn
  // whose frames this root loop demuxes, so it CANNOT be awaited inline (that would
  // deadlock the transport read). It runs in the background, tracked here, and the turn
  // stream flushes it before ending. Every non-delegate callback stays inline.
  private readonly pendingToolCalls = new Set<Promise<unknown>>();
  // Issue #1583: the in-flight root delegation callbacks (see DelegationDispatch) and the child
  // threads bound to one of them. A child frame projects ONLY through a binding.
  private readonly dispatches = new Set<DelegationDispatch>();
  private readonly childBindings = new Map<string, ChildBinding>();

  // Per-turn state (turns run strictly sequentially).
  private activeTurnId?: string;
  private turnClosed = false;
  private terminalEmitted = false;
  private stopRequested = false;
  // PRD #1534: the classification captured from the active turn's final non-retrying
  // provider method:"error" frame, folded into the terminal at decode. Reset per turn.
  private pendingCodexError?: CodexErrorClassification;
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
    this.authMode = opts.authMode;
    this.accountant = opts.accountant ?? new CodexUsageAccountant();
    this.emitClaimInit = opts.emitClaimInit ?? true;
    this.scrubProjected = opts.scrubProjected ?? ((s: string): string => s);
    this.idNonce = opts.idNonce ?? newProjectionNonce();
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

  /** PRD #1332 C4a: capture the IMMUTABLE CHILD `threadId -> configured model` mapping. The
   *  executor's child-turn seam calls this the moment a delegated child thread id is known,
   *  with the model it selected (`spec.model ?? provider.model`), so the accountant can charge
   *  the child's token-usage notes to its ACTUAL model rather than the root's. A child thread
   *  is always fresh (ephemeral, never resumed), so it baselines at zero. */
  recordChildThreadModel(threadId: string, model: string): void {
    this.accountant.registerThread(threadId, model, false);
  }

  /** Stop routing frames for `threadId` to a child sink (the child turn is done). Also drops the
   *  thread's dispatch binding, so a late note for it projects nothing. */
  unregisterChildSink(threadId: string): void {
    this.childSinks.delete(threadId);
    this.childBindings.delete(threadId);
  }

  /**
   * Issue #1583: bind a just-started child thread to the root delegation callback that started it,
   * and emit the LEAD dispatch frame (`tool_use` named "Agent", id = the dispatch id, input =
   * `{subagent_type, description}`). `parent` is the callback identity the broker handed the
   * delegate seam; `admittedRole` is the role the broker ADMITTED (never re-read from the args).
   *
   * Binds only a REGISTERED child thread to EXACTLY ONE pending, unbound dispatch of the live turn
   * with that parent key; none, or an ambiguous key (two in-flight calls sharing it), binds nothing
   * and the child runs unprojected. Fail-safe: never throws.
   */
  bindChildDispatch(
    childThreadId: string,
    parent: { readonly threadId: string; readonly turnId: string; readonly callId: string },
    admittedRole: string,
  ): void {
    try {
      if (!this.childSinks.has(childThreadId) || this.childBindings.has(childThreadId)) return;
      const matches = [...this.dispatches].filter(
        (d) =>
          d.state === "pending" &&
          d.threadId === parent.threadId &&
          d.turnId === parent.turnId &&
          d.callId === parent.callId,
      );
      if (matches.length !== 1) return;
      const dispatch = matches[0]!;
      if (dispatch.ordinal !== this.liveProjectionTurn) return;
      let role: string;
      try {
        role = projectToolName(admittedRole, this.scrubProjected);
      } catch {
        role = "unknown";
      }
      dispatch.state = "bound";
      const binding: ChildBinding = {
        dispatch,
        role,
        counter: 0,
        issuedIds: new Set(),
        openTools: new Map(),
        items: 0,
        bytes: 0,
        capped: false,
      };
      dispatch.child = binding;
      this.childBindings.set(childThreadId, binding);
      this.emitProjected(
        this.leadFrame({
          kind: "tool",
          phase: "started",
          id: dispatch.dispatchId,
          name: DISPATCH_TOOL_NAME,
          input: { subagent_type: role, description: dispatch.label },
        }),
        dispatch.ordinal,
      );
    } catch {
      /* projection is best-effort: the child runs regardless */
    }
  }

  /**
   * Issue #1583: project RAW child items as one subagent-origin frame attributed to the bound
   * dispatch (`agent` = the admitted role, `agentInstance` = the dispatch id, `agentLabel` = its
   * label when non-empty). Pushed ONLY while `childThreadId` is registered AND bound to a dispatch
   * that is still open, and under the dispatch's turn ordinal, so nothing projects after its turn
   * ended. The frame NEVER carries `signals`.
   *
   * Items are raw and projected here, so the caller never needs the scrub: text/thinking are
   * scrubbed then bounded; a tool item's `id` is the provider call id (a `started` is issued a
   * fresh `<dispatchId>/<callId>` id, and its `finished` reuses it; a finished with no open started
   * is dropped), its `name` the canonical tool name, its `input` the raw args and its `output` the
   * raw output value (the broker's message on failure).
   *
   * Bounded per dispatch AND per turn: every projected item is counted against
   * {@link MAX_CHILD_ITEMS_PER_DISPATCH} / {@link MAX_CHILD_BYTES_PER_DISPATCH} for this dispatch and
   * {@link MAX_CHILD_ITEMS_PER_TURN} / {@link MAX_CHILD_BYTES_PER_TURN} summed over every dispatch of
   * the turn (item by item, so one oversized frame is cut too). The first item that would exceed
   * any of them caps the dispatch: a single {@link CHILD_OUTPUT_CAPPED} text item is emitted for
   * it, and every later child item of it is dropped, EXCEPT a `finished` that closes a projected
   * `started`. Such a closer is emitted in full while it fits; once it does not (or the dispatch is
   * already capped) it is still emitted, so no projected tool is left running, but with its output
   * replaced by {@link CHILD_TOOL_OUTPUT_OMITTED}, so each over-budget closer costs a small constant
   * number of bytes (still counted). A `started` that would exceed a budget is never projected.
   * Child tools still open when the dispatch is closed are settled by
   * {@link closeOpenDispatches}. The lead completion is NOT a child item and is always still
   * emitted. Fail-safe: never throws.
   */
  emitChildFrame(childThreadId: string, items: readonly HarnessItem[]): void {
    try {
      if (!this.childSinks.has(childThreadId)) return;
      const binding = this.childBindings.get(childThreadId);
      if (binding === undefined || binding.dispatch.state !== "bound") return;
      if (binding.capped && binding.openTools.size === 0) return;
      const projected: HarnessItem[] = [];
      for (const item of items) {
        const closesOpen = item.kind === "tool" && item.phase === "finished" && binding.openTools.has(item.id ?? "");
        if (binding.capped && !closesOpen) continue;
        let one = this.projectChildItem(binding, item);
        if (one === undefined) continue;
        let size = Buffer.byteLength(JSON.stringify(one), "utf8");
        if (binding.capped || this.exceedsChildBudget(binding, size)) {
          if (!binding.capped) {
            binding.capped = true;
            projected.push({ kind: "text", text: CHILD_OUTPUT_CAPPED });
          }
          if (!closesOpen || one.kind !== "tool" || one.phase !== "finished") continue;
          // Close the visible tool anyway, at a constant size.
          one = { ...one, output: CHILD_TOOL_OUTPUT_OMITTED };
          size = Buffer.byteLength(JSON.stringify(one), "utf8");
        } else if (one.kind === "tool" && one.phase === "started" && one.id !== undefined) {
          const rawId = item.kind === "tool" ? (item.id ?? "") : "";
          const queue = binding.openTools.get(rawId) ?? [];
          queue.push({ id: one.id, name: one.name ?? "unknown" });
          binding.openTools.set(rawId, queue);
        }
        binding.items += 1;
        binding.bytes += size;
        this.childTurnItems += 1;
        this.childTurnBytes += size;
        projected.push(one);
      }
      this.emitChildItems(binding, projected);
    } catch {
      /* projection is best-effort: the child runs regardless */
    }
  }

  /** Whether one more projected child item of `size` bytes would exceed the dispatch's or the
   *  turn's item/byte budget. */
  private exceedsChildBudget(binding: ChildBinding, size: number): boolean {
    return (
      binding.items + 1 > MAX_CHILD_ITEMS_PER_DISPATCH ||
      binding.bytes + size > MAX_CHILD_BYTES_PER_DISPATCH ||
      this.childTurnItems + 1 > MAX_CHILD_ITEMS_PER_TURN ||
      this.childTurnBytes + size > MAX_CHILD_BYTES_PER_TURN
    );
  }

  /** Push already-projected child items as one subagent-origin frame of the binding's dispatch
   *  (nothing when empty). */
  private emitChildItems(binding: ChildBinding, items: HarnessItem[]): void {
    if (items.length === 0) return;
    const { dispatch, role } = binding;
    this.emitProjected(
      {
        kind: "frame",
        origin: { kind: "subagent", role, instanceId: dispatch.dispatchId },
        attribution: {
          agent: role,
          agentInstance: dispatch.dispatchId,
          ...(dispatch.label.length > 0 ? { agentLabel: dispatch.label } : {}),
        },
        items,
        sessionId: this.threadId,
      },
      dispatch.ordinal,
    );
  }

  /** Project one raw child item (see {@link emitChildFrame}); undefined drops it. A projected
   *  `started` is NOT recorded as open here: the caller records it only once it is emitted. */
  private projectChildItem(binding: ChildBinding, item: HarnessItem): HarnessItem | undefined {
    const scrub = this.scrubProjected;
    if (item.kind === "text" || item.kind === "thinking") return { kind: item.kind, text: projectText(item.text, scrub) };
    const rawId = item.id ?? "";
    let name: string;
    try {
      name = projectToolName(item.name, scrub);
    } catch {
      name = "unknown";
    }
    if (item.phase === "started") {
      const id = this.issueChildToolId(binding, rawId);
      let input: unknown;
      try {
        input = projectToolInput(item.input, scrub);
      } catch {
        input = { truncated: true };
      }
      return { kind: "tool", phase: "started", id, name, input };
    }
    const queue = binding.openTools.get(rawId);
    const open = queue?.shift();
    if (queue === undefined || open === undefined) return undefined;
    if (queue.length === 0) binding.openTools.delete(rawId);
    const id = open.id;
    let output: string;
    try {
      output = projectOutputValue(item.output, scrub);
    } catch {
      output = "[projection failed]";
    }
    return { kind: "tool", phase: "finished", id, name, output, isError: item.isError === true };
  }

  /** A `<dispatchId>/<callId>` id not yet issued within this child (a colliding one gets a
   *  `-n<counter>` suffix until unique). */
  private issueChildToolId(binding: ChildBinding, callId: string): string {
    binding.counter += 1;
    const dispatchId = binding.dispatch.dispatchId;
    let base: string;
    try {
      base = childProjectedId(dispatchId, callId, binding.counter, this.scrubProjected);
    } catch {
      base = childProjectedId(dispatchId, "", binding.counter);
    }
    let id = base;
    while (binding.issuedIds.has(id)) {
      binding.counter += 1;
      id = `${base}-n${binding.counter}`;
    }
    binding.issuedIds.add(id);
    return id;
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
    this.pendingCodexError = undefined;
    this.turnOrdinal += 1;
    this.projectionCounter = 0;
    this.issuedProjectionIds = new Set();
    this.childTurnItems = 0;
    this.childTurnBytes = 0;
    this.outbox = [];

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

  // --- projection outbox (issue #1583) ---------------------------------------------

  /** Queue one projected event for the turn `ordinal` and wake the run loop. Dropped when that
   *  turn's stream is no longer live (a late callback settling after its turn ended). */
  private emitProjected(ev: HarnessEvent, ordinal: number): void {
    if (ordinal !== this.liveProjectionTurn) return;
    this.outbox.push(ev);
    const wake = this.wakeOutbox;
    this.wakeOutbox = undefined;
    wake?.();
  }

  /** Resolves once the outbox holds at least one event. Only the run loop awaits it, and it
   *  races one at a time, so a single wake slot suffices; a superseded waiter simply never
   *  resolves (it is only ever referenced by a race that already settled). */
  private outboxReady(): Promise<"outbox"> {
    if (this.outbox.length > 0) return Promise.resolve("outbox");
    return new Promise((resolve) => {
      this.wakeOutbox = (): void => resolve("outbox");
    });
  }

  /** Before a stream-failure throw: give every bound dispatch of turn `ordinal` its synthesized,
   *  unconfirmed error completion and yield the drained outbox, so the lead lane settles instead
   *  of lingering. The caller throws AFTER this returns; a consumer that keeps pulling gets the
   *  throw, and one that stops early (return()) simply never sees it. */
  private *closeDispatchesOnFailure(ordinal: number): Generator<HarnessEvent> {
    this.closeOpenDispatches(ordinal, DISPATCH_STREAM_FAILED);
    yield* this.drainOutbox();
  }

  /** Yield every queued projected event, including any queued while a yield was pending. */
  private *drainOutbox(): Generator<HarnessEvent> {
    for (let ev = this.outbox.shift(); ev !== undefined; ev = this.outbox.shift()) yield ev;
  }

  private async *runTurn(request: RunTurnRequest, rendered: RenderedCodexRun): AsyncGenerator<HarnessEvent> {
    this.currentModel = rendered.lead.model ?? this.provider.model;
    const ordinal = this.turnOrdinal;
    this.liveProjectionTurn = ordinal;

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
        const resumed = request.resumeSessionId !== undefined;
        this.threadId = resumed
          ? await this.resumeThread(transport, request, rendered)
          : await this.startThread(transport, rendered, request.signal);
        // PRD #1332 C4a: capture the ROOT thread->model mapping the moment the thread id is
        // known. The model is the immutable configured root model (currentModel); a resumed
        // thread baselines at its pre-resume cumulative rather than zero.
        this.accountant.registerThread(this.threadId, this.currentModel ?? this.provider.model, resumed);
      }

      // 3. Start the turn with the rendered prompt / model / effort.
      this.activeTurnId = await this.startTurnRpc(transport, this.threadId, rendered, request.signal);
      if (request.signal.aborted || this.stopRequested) {
        // A stop during launch/thread/turn setup could not name a turn earlier. Now
        // that activeTurnId exists, issue the best-effort interrupt and end cleanly.
        this.endTurnOnStop();
        return;
      }

      // Usage lineage is one row per executor claim leg, not per provider epoch. Emit the marker
      // explicitly after thread start/resume + turn/start so a resumed claim gets one even though
      // the pinned app-server emits no thread/started on thread/resume. Internal epoch recreations
      // pass emitClaimInit=false, and this latch prevents another marker on later turns in epoch 0.
      if (this.emitClaimInit && !this.claimInitEmitted) {
        this.claimInitEmitted = true;
        yield { kind: "initialized", model: this.currentModel, sessionId: this.threadId };
      }

      // 4. Consume the notification stream, mapping each raw frame to ONE neutral event.
      //    Every return path below first drains the projection outbox (issue #1583), so a
      //    projected tool frame queued before the stream ends is never silently lost.
      for (;;) {
        if (this.turnClosed) {
          this.closeOpenDispatches(ordinal, DISPATCH_STOPPED);
          yield* this.drainOutbox();
          return;
        }
        const notePromise = this.pendingNote ?? notes.next();
        this.pendingNote = notePromise;
        let step: IteratorResult<CodexNotification> | "aborted" | "outbox";
        try {
          // An outbox wake drains + yields the queued frames and re-races the SAME pending
          // note (still held in `pendingNote`), so no provider notification is ever dropped.
          for (;;) {
            step = await Promise.race([notePromise, abortPromise, this.outboxReady()]);
            if (step !== "outbox") break;
            yield* this.drainOutbox();
          }
        } catch (error) {
          if (this.pendingNote === notePromise) this.pendingNote = undefined;
          yield* this.closeDispatchesOnFailure(ordinal);
          throw error;
        }
        if (step === "aborted") {
          // Owner cancel/watchdog wins: interrupt the turn and end the stream cleanly. A bound
          // delegation still open gets a synthesized, unconfirmed lead completion first.
          this.endTurnOnStop();
          this.closeOpenDispatches(ordinal, DISPATCH_STOPPED);
          yield* this.drainOutbox();
          return;
        }
        if (this.pendingNote === notePromise) this.pendingNote = undefined;
        if (step.done) {
          // A deliberate close or an already-emitted terminal ends cleanly. Otherwise
          // this is Codex's own unexpected EOF → a protocol throw (rule 9), never a
          // fabricated success.
          if (this.terminalEmitted || this.turnClosed) {
            this.closeOpenDispatches(ordinal, DISPATCH_STOPPED);
            yield* this.drainOutbox();
            return;
          }
          yield* this.closeDispatchesOnFailure(ordinal);
          throw new CodexHarnessError({
            category: "protocol",
            message: "codex app-server stream ended before turn completion",
          });
        }
        // The explicit claim init above replaces the root thread/started event without adding a
        // second neutral activity. Child/foreign thread starts still flow through demux/mapNote.
        if (step.value.kind === "thread_started" && step.value.threadId === this.threadId) {
          continue;
        }
        // PRD #1332 C4a: reconcile every typed token-usage note into the per-model accountant
        // BEFORE the demux, so BOTH root and demuxed-child usage is captured off this single
        // consumer. An unknown/unregistered thread id is dropped inside record() (never
        // attributed to root). The note still flows on to its normal handling below (a child's
        // routes to its sink; a root's maps to `activity`), so decode behavior is unchanged.
        if (step.value.kind === "token_usage_updated") {
          this.accountant.record(step.value.threadId, step.value.usage);
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
        //
        // mapNote is invoked EXACTLY ONCE per note (so a callback is brokered and replied
        // once); an outbox wake (issue #1583: a projected `started` frame queued while the
        // broker effect is still running) drains + yields the queue and re-races that SAME
        // promise. The outbox is drained again before `mapped` itself is yielded, so a
        // callback's started/finished pair always precedes the note's own event.
        const mappedPromise = this.mapNote(transport, step.value, request.signal);
        let mapped: HarnessEvent | "aborted" | "outbox";
        try {
          for (;;) {
            mapped = await Promise.race([mappedPromise, abortPromise, this.outboxReady()]);
            if (mapped !== "outbox") break;
            yield* this.drainOutbox();
          }
        } catch (error) {
          yield* this.closeDispatchesOnFailure(ordinal);
          throw error;
        }
        if (mapped === "aborted") {
          this.endTurnOnStop();
          this.closeOpenDispatches(ordinal, DISPATCH_STOPPED);
          yield* this.drainOutbox();
          return;
        }
        // The owner stops consuming at `turn_finished`, so a bound delegation still open here
        // gets its (unconfirmed) lead completion BEFORE the terminal, never after it.
        if (mapped.kind === "turn_finished") this.closeOpenDispatches(ordinal, DISPATCH_OPEN_AT_TURN_END);
        yield* this.drainOutbox();
        yield mapped;
        if (mapped.kind === "turn_finished") {
          // Flush any concurrently-running delegation callbacks: each drives a child turn
          // that settles (and replies) before this resolves, so the parent spawn_agent
          // callback has responded by the time the root turn stream closes. A wedged
          // child self-bounds via its own per-child deadline (delegation.ts), so this
          // await is finite. The non-delegation path has an empty set and never waits.
          if (this.pendingToolCalls.size > 0) await Promise.allSettled(this.pendingToolCalls);
          // Every dispatch of this turn was closed above, so a delegation settling here projects
          // no completion; this drain only keeps the "drain before return" invariant.
          yield* this.drainOutbox();
          return; // close the iterator after the terminal
        }
      }
    } finally {
      if (onAbort) request.signal.removeEventListener("abort", onAbort);
      if (this.stopTurn === settleStop) this.stopTurn = undefined;
      // Close the outbox for this turn: a callback settling after the stream ended drops its
      // projection rather than leaking it into a later turn. Only the turn that still owns the
      // outbox clears it, so a late finally (an abandoned iterator returned after a newer turn
      // went live) cannot wipe the newer turn's queue or wake slot.
      if (this.liveProjectionTurn === ordinal) {
        this.liveProjectionTurn = undefined;
        this.outbox = [];
        this.wakeOutbox = undefined;
      }
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
    let res: { thread?: { id?: string } };
    try {
      res = await transport.request<{ thread?: { id?: string } }>(
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
    } catch (error) {
      // Only a provider rejection of thread/resume is a lost lineage. Transport
      // timeouts, EOF, and cancellation must retain their original failure path.
      if (request.signal.aborted || !(error instanceof CodexTransportError) ||
          error.failure.category !== "protocol" ||
          !error.message.startsWith("codex app-server returned a JSON-RPC error")) throw error;
      throw new CodexResumeError();
    }
    const id = res?.thread?.id;
    if (typeof id !== "string" || id.length === 0) throw new CodexResumeError();
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
      // A cold thread/resume in pinned Codex does not restore the thread/start
      // environment selection. Reassert the empty selection on every turn so a
      // resumed root cannot inherit the provider's native execution environment.
      environments: [],
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
        // The active root's notification is consumed in runTurn because its explicit claim init
        // already carried the same session. Only a child/foreign start reaches here as liveness.
        return { kind: "activity", sessionId: note.threadId };
      case "turn_started":
        return { kind: "activity", sessionId: note.threadId };
      case "token_usage_updated":
        // PRD #1332 C4a: the token accounting already happened in the run loop before the
        // demux; on the event stream this is pure liveness (no frame, no items). A child's
        // token-usage note never reaches here (the demux routes it to the child sink first).
        return { kind: "activity", sessionId: note.threadId };
      case "codex_error": {
        // PRD #1534: bind the provider ErrorNotification to the ACTIVE root turn and retain
        // its classification ONLY on an explicit non-retrying frame (willRetry === false) —
        // final-non-retrying-wins. A retrying frame is liveness/diagnostic, never the terminal
        // cause; a stale/foreign/child identity is ignored. Never emits a frame or latches the
        // terminal; parsing stays in the single-source normalizer (no raw provider text kept).
        if (note.threadId === this.threadId && note.turnId === this.activeTurnId && note.willRetry === false) {
          const rawInfo = asObject(asObject(note.params)?.error)?.codexErrorInfo;
          this.pendingCodexError = normalizeCodexErrorInfo(rawInfo);
        }
        return { kind: "activity", sessionId: note.threadId };
      }
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
              // `activity` below; an active-turn non-signal root call's projected tool
              // frames (issue #1583) travel separately, on the outbox.
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
    // `beforeReply` runs once the broker settled (or threw → broker_error) and BEFORE the model
    // reply, so a projected `finished` frame is queued ahead of anything the reply triggers.
    const runAndReply = async (beforeReply?: (result: CallbackResult) => void): Promise<CallbackResult> => {
      let result: CallbackResult;
      try {
        // A matched callback is, by construction, the root turn's — origin is "root".
        result = await this.broker.handleToolCall({ threadId, turnId, callId }, p.tool, p.arguments, "root");
      } catch {
        result = { ok: false, code: "broker_error", message: "the callback failed" };
      }
      try {
        beforeReply?.(result);
      } catch {
        /* projection is best-effort (issue #1583): it never blocks the reply */
      }
      this.safeRespond(transport, requestId, result);
      return result;
    };
    const toolName = asString(p.tool);
    const canonical = toolName !== undefined ? canonicalizeCodexToolName(toolName) : undefined;
    const isSignal = canonical !== undefined && CODEX_SIGNAL_TOOLS.has(canonical);
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
    //
    // Issue #1583: the delegation is recorded as a pending dispatch. The lead dispatch frame is
    // emitted only once its child actually starts (bindChildDispatch), and its lead completion
    // is queued once the broker settles, BEFORE the reply; the record is dropped on settle.
    if (canonical !== undefined && CODEX_DELEGATE_TOOLS.has(canonical)) {
      const dispatch = this.recordDispatch(threadId, turnId, callId, p.arguments);
      const task = runAndReply(dispatch === undefined ? undefined : (result) => this.completeDispatch(dispatch, result));
      this.pendingToolCalls.add(task);
      void task.finally(() => {
        this.pendingToolCalls.delete(task);
        if (dispatch !== undefined) this.dispatches.delete(dispatch);
      });
      return undefined;
    }
    // Issue #1583: project every other ACTIVE-TURN root callback (shell/file/mcp/unknown) as a
    // lead tool_use/tool_result pair — Codex hands uzi no tool blocks of its own, so without
    // this the lanes see no tool activity at all. Signal tools are NOT projected (their
    // signals-frame path below stays byte-identical, and the reducer drops signal tool_use
    // anyway); a denied/stale/foreign identity already returned above, unprojected.
    const project = isSignal ? undefined : this.beginToolProjection(callId, canonical, p.arguments);
    const result = await runAndReply(project);
    // Route a TRUSTED ROOT SIGNAL callback's scanned result into the run-lane reducer: when
    // the tool canonicalizes to a signal tool AND the broker ACCEPTED it, `result.output` is
    // the scanned Partial<TurnSignals> (broker.dispatchSignal returns `{ok:true, output:
    // scanned}`), which the caller surfaces on a main-origin signals frame. A DENIED signal
    // (result.ok === false) folds nothing; a non-signal effect (shell/file/mcp) is undefined.
    if (isSignal && result.ok) {
      return { signals: result.output as Readonly<Partial<TurnSignals>> };
    }
    return undefined;
  }

  /** Issue #1583: queue the projected `started` tool frame for one root callback NOW (before the
   *  broker is awaited) and return the hook that queues its `finished` frame once the broker
   *  settles. Both frames are main-origin, lead-attributed, share one namespaced id (unique
   *  within the turn), and carry only scrubbed + bounded input/output (see projection.ts). They
   *  carry NO `signals`.
   *
   *  FAIL-SAFE: projection is observability only, so it never throws into the callback path.
   *  A failure projecting the input falls back to `{ truncated: true }`, one projecting the
   *  output to "[projection failed]", and anything else drops the frame; the broker call and
   *  the model reply proceed regardless. */
  private beginToolProjection(
    callId: string,
    canonical: string | undefined,
    args: unknown,
  ): ((result: CallbackResult) => void) | undefined {
    try {
      const ordinal = this.turnOrdinal;
      const id = this.issueProjectionId(ordinal, callId);
      let name: string;
      try {
        name = projectToolName(canonical, this.scrubProjected);
      } catch {
        name = "unknown";
      }
      const frame = (item: HarnessItem): HarnessEvent => this.leadFrame(item);
      let input: unknown;
      try {
        input = projectToolInput(args, this.scrubProjected);
      } catch {
        input = { truncated: true };
      }
      this.emitProjected(frame({ kind: "tool", phase: "started", id, name, input }), ordinal);
      return (result) => {
        let output: string;
        try {
          output = projectToolOutput(result, this.scrubProjected);
        } catch {
          output = "[projection failed]";
        }
        this.emitProjected(frame({ kind: "tool", phase: "finished", id, name, output, isError: !result.ok }), ordinal);
      };
    } catch {
      return undefined;
    }
  }

  /** One projected main-origin, lead-attributed frame carrying `item` (never `signals`). */
  private leadFrame(item: HarnessItem): HarnessEvent {
    return {
      kind: "frame",
      origin: { kind: "main" },
      attribution: { agent: "lead" },
      items: [item],
      model: this.currentModel,
      sessionId: this.threadId,
    };
  }

  /** Issue #1583: record one active-turn root delegation callback as a pending dispatch. Its id
   *  comes from the same namespaced, per-turn-unique helper as every projected id; its label is
   *  the `description` arg (scrubbed + bounded) or "". The role is NOT read here: it is bound
   *  later from what the broker admitted. Fail-safe: undefined on any failure. */
  private recordDispatch(threadId: string, turnId: string, callId: string, args: unknown): DelegationDispatch | undefined {
    try {
      const ordinal = this.turnOrdinal;
      const dispatchId = this.issueProjectionId(ordinal, callId);
      let label = "";
      try {
        const description = asObject(args)?.description;
        if (typeof description === "string") label = projectLabel(description, this.scrubProjected);
      } catch {
        label = "";
      }
      const dispatch: DelegationDispatch = { threadId, turnId, callId, dispatchId, label, ordinal, state: "pending" };
      this.dispatches.add(dispatch);
      return dispatch;
    } catch {
      return undefined;
    }
  }

  /** Queue the lead completion (`tool_result` for the dispatch id) of a BOUND dispatch whose
   *  broker call settled, and close it. A child tool still open (the child timed out or aborted
   *  mid-tool) first gets its synthesized error `finished` (see {@link closeOpenChildTools}), so
   *  the child frames precede the completion. Runs before the reply; an unbound or already-closed
   *  dispatch (child never started, or the turn stopped first) projects nothing. */
  private completeDispatch(dispatch: DelegationDispatch, result: CallbackResult): void {
    if (dispatch.state !== "bound") return;
    dispatch.state = "closed";
    this.closeOpenChildTools(dispatch);
    let output: string;
    try {
      output = projectToolOutput(result, this.scrubProjected);
    } catch {
      output = "[projection failed]";
    }
    this.emitProjected(
      this.leadFrame({ kind: "tool", phase: "finished", id: dispatch.dispatchId, name: DISPATCH_TOOL_NAME, output, isError: !result.ok }),
      dispatch.ordinal,
    );
  }

  /** Close every dispatch of turn `ordinal`: a BOUND one first gets a synthesized error `finished`
   *  ({@link CHILD_TOOL_UNCONFIRMED}) for each child tool still open, in one child frame, then a
   *  synthesized error completion carrying `content` (its child's settlement is not confirmed); a
   *  pending one can no longer bind. A later real settlement of a closed dispatch projects nothing,
   *  so there is never a duplicate completion. Fail-safe: never throws. */
  private closeOpenDispatches(ordinal: number, content: string): void {
    try {
      for (const dispatch of this.dispatches) {
        if (dispatch.ordinal !== ordinal || dispatch.state === "closed") continue;
        const wasBound = dispatch.state === "bound";
        dispatch.state = "closed";
        if (!wasBound) continue;
        this.closeOpenChildTools(dispatch);
        this.emitProjected(
          this.leadFrame({
            kind: "tool",
            phase: "finished",
            id: dispatch.dispatchId,
            name: DISPATCH_TOOL_NAME,
            output: content,
            isError: true,
          }),
          ordinal,
        );
      }
    } catch {
      /* projection is best-effort */
    }
  }

  /** Emit a synthesized error `finished` for every child tool of `dispatch` still open (oldest
   *  first per raw id), so none is left running once its dispatch closes. Fail-safe. */
  private closeOpenChildTools(dispatch: DelegationDispatch): void {
    try {
      const binding = dispatch.child;
      if (binding === undefined || binding.openTools.size === 0) return;
      const closers: HarnessItem[] = [];
      for (const queue of binding.openTools.values()) {
        for (const open of queue) {
          closers.push({ kind: "tool", phase: "finished", id: open.id, name: open.name, output: CHILD_TOOL_UNCONFIRMED, isError: true });
        }
      }
      binding.openTools.clear();
      this.emitChildItems(binding, closers);
    } catch {
      /* projection is best-effort */
    }
  }

  /** A namespaced projected id not yet issued this turn: a colliding id (a reused call id, or
   *  two that sanitise identically) gets a `-n<counter>` suffix until it is unique. */
  private issueProjectionId(ordinal: number, callId: string): string {
    this.projectionCounter += 1;
    let base: string;
    try {
      base = projectedId(this.idNonce, ordinal, callId, this.projectionCounter, this.scrubProjected);
    } catch {
      // A throwing scrub must not drop the projection: fall back to the counter id, which
      // carries no provider-supplied text at all.
      base = projectedId(this.idNonce, ordinal, "", this.projectionCounter);
    }
    let id = base;
    while (this.issuedProjectionIds.has(id)) {
      this.projectionCounter += 1;
      id = `${base}-n${this.projectionCounter}`;
    }
    this.issuedProjectionIds.add(id);
    return id;
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
    // PRD #1534: prefer the active turn's final non-retrying method:"error" classification,
    // falling back to the terminal turn.error's codexErrorInfo. Both go through the same
    // single-source normalizer, so only a CLOSED classification token + a bounded httpStatus
    // are ever surfaced — no raw provider text (message/additionalDetails/misalignment).
    const fromTerminal = normalizeCodexErrorInfo(asObject(turn?.error)?.codexErrorInfo);
    const classification = pickCodexClassification(this.pendingCodexError, fromTerminal);
    const errors = normalizeCodexTerminalErrors(subtype, outcome, classification);
    // PRD #1332 C4a/C4b: attach the per-model token accounting as the result-frame `modelUsage`,
    // now carrying each model's closed `costStatus` (and `costUSD` when metered) projected from
    // the run's auth mode against the D5 price table (C4b). The reducer emits
    // `terminal.usage.wire.modelUsage`, so the reconciled deltas + cost ride out there. When no
    // usage was reconciled, aggregateByModel() is undefined and the terminal usage is left
    // byte-identical to before C4a. `now` is the real clock at the terminal point; the price
    // table keeps the injectable-clock seam (D5) so the Sol boundary is deterministic in tests.
    const pricing = this.authMode === undefined ? undefined : { authMode: this.authMode, now: new Date() };
    const modelUsage = this.accountant.aggregateByModel(pricing);
    const usage = attachModelUsage(normalizeCodexUsage(turn?.usage, "turn"), modelUsage);
    // The RUN-level cost status: subscription/metered/unreported, folded with D5's unreported
    // dominance. Undefined auth mode leaves it `unreported` (price-free), matching `modelUsage`.
    const cost = this.authMode === undefined ? { kind: "unreported" as const } : deriveCodexRunCost(modelUsage, this.authMode);
    return {
      outcome,
      subtype,
      errors,
      usage,
      metrics: { cost },
      failure: {
        // Deferred, invoked only at the owner's classification point. The category now comes
        // from the CLOSED classification MAP (never invented from a raw provider status), and
        // the message suffix is the CLOSED display token plus a bounded http status only — no
        // raw provider text (message/additionalDetails/misalignment) is ever retained. When
        // there is no classification the message stays based on the CLOSED subtype alone and
        // the category defaults to "unknown", byte-identical to before #1534.
        materialize: (_limit): HarnessThrownFailure => {
          const suffix = classification !== undefined ? ` (${formatCodexClassification(classification)})` : "";
          const original = new Error(`codex turn failed: ${subtype}${suffix}`);
          const category = classification?.category ?? "unknown";
          return { failure: { category, message: original.message }, original };
        },
      },
    };
  }
}
