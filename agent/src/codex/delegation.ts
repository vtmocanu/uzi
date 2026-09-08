// PRD #1171 (M3, milestone 2) — the real worker-created SYNCHRONOUS child-thread
// delegation runner: the production implementation of the broker's injected
// {@link DelegateSeam}.
//
// A root turn's model can select a delegation callback (`spawn_agent` / the
// code-mode `collaboration*`/`Subagent*` family). The broker admits it ROOT-ONLY and
// NON-NESTED (see broker.ts `dispatchDelegate`) and then AWAITS this seam; the parent
// callback resolves only after the child fully settles. This module is what actually
// runs that child: it starts a NEW child thread+turn on the provider root, drives the
// child's own callbacks through a SECOND broker bound to the CHILD's honest identity,
// and returns a bounded neutral result.
//
// FAIL-CLOSED and HONEST-ATTRIBUTION by construction:
//   - the child broker is constructed with the child role's IMMUTABLE `isRoot:false`
//     grants, so a subagent can never hold root authority no matter what its args say;
//   - every child callback is routed with origin `"child"`, so the SAME broker code
//     that root-gates signals/delegation denies a subagent `submit_plan`/`signal_done`
//     (non-root signal) and a subagent `spawn_agent` (NESTED delegation) — there is no
//     second copy of that policy here, only the honest origin fed to the one enforcer;
//   - the child's callbacks carry the child's OWN `(threadId, turnId)` and role, never
//     the parent's, so the registry's idempotency/quiescence accounting and any audit
//     see the true lineage;
//   - the child runs on the SAME {@link ExecutionRegistry}, so its callbacks are part
//     of the run's quiescence barrier;
//   - the parent callback resolves ONLY after the child turn reaches its terminal AND
//     every child callback has settled AND the child controller is closed — full child
//     settlement precedes the parent's resolution;
//   - cancellation (the run/turn abort) and a bounded per-child deadline both end the
//     child promptly with a stable `child_aborted` / `child_timeout` code the broker's
//     fixed CHILD_FAILURE_CODES vocabulary permits.
//
// COMPOSABLE, NO REAL CODEX: the raw thread/turn RPCs are an INJECTED SEAM
// ({@link StartChildTurnSeam}); m3 wires it to the provider transport, tests inject a
// fake controller yielding scripted notifications. This module owns only the
// security-relevant loop (attribution, origin, settlement, cancellation) and never
// wires itself into runner.ts.

import {
  CodexCallbackBroker,
  type CallbackResult,
  type CallbackRuntimeId,
  type ChildDelegationRequest,
  type ChildDelegationResult,
  type FileopClient,
  type RunGrants,
  type ScreenPolicy,
  type SpawnCommandSeam,
  type ToolHandler,
} from "./broker.js";
import type { HarnessEffort } from "../harness.js";
import type { ExecutionRegistry } from "./registry.js";
import type { CodexNotification } from "./transport.js";

/** One known delegation target: the child role's rendered prompt, its IMMUTABLE
 *  `isRoot:false` grants (the renderer's `perRoleGrants` entry) and its resolved
 *  model/effort. `grants.isRoot` MUST be false; a root-graned role is refused. */
export interface DelegationRole {
  readonly grants: RunGrants;
  readonly systemPrompt: string;
  readonly model?: string;
  readonly effort?: HarnessEffort;
}

/** The spec the runner hands to {@link StartChildTurnSeam}. It carries only the child
 *  role, its trusted rendered prompt, the (untrusted) model-supplied task text, the
 *  resolved model/effort, the parent lineage and the combined abort signal. */
export interface StartChildTurnSpec {
  readonly role: string;
  readonly systemPrompt: string;
  /** The model-supplied task text (from the parent's spawn args) — UNTRUSTED DATA
   *  passed as the child turn's input, never as authority. */
  readonly taskInput: string;
  readonly model?: string;
  readonly effort?: HarnessEffort;
  /** The parent callback's identity, for lineage/audit only (never authority). */
  readonly parent: CallbackRuntimeId;
  /** Fires on the run/turn abort OR the per-child deadline; the seam should bound its
   *  RPCs by it so a slow child start does not outlive cancellation. */
  readonly signal: AbortSignal;
}

/** A live child thread+turn on the provider root. The runner drives ONLY these
 *  primitives; the seam owns the raw transport/thread RPCs. `threadId`/`turnId` are the
 *  child's HONEST ids — the runner keys the child broker and every callback on them. */
export interface ChildThreadController {
  readonly threadId: string;
  readonly turnId: string;
  /** Single-consumer async iterator of the child turn's decoded notifications. */
  notifications(): AsyncIterableIterator<CodexNotification>;
  /** Answer a child server→client tool-call request with the neutral broker result. */
  respond(
    requestId: number | string,
    reply: { readonly result: unknown } | { readonly error: { readonly code: number; readonly message: string } },
  ): void;
  /** Best-effort cancellation REQUEST for the child turn (not a process kill). */
  interrupt(): Promise<void>;
  /** Idempotent teardown of the child thread/turn iteration. */
  close(): Promise<void>;
}

export type StartChildTurnSeam = (spec: StartChildTurnSpec) => Promise<ChildThreadController>;

export interface CodexDelegationRunnerOptions {
  readonly registry: ExecutionRegistry;
  /** Known delegation targets keyed by role; unset/absent role denies (fail-closed). */
  readonly roles: ReadonlyMap<string, DelegationRole>;
  readonly startChildTurn: StartChildTurnSeam;
  /** The child effect seams — a subagent's shell/file effects run as the SAME
   *  credential-free command identity the root's do. */
  readonly spawnCommand: SpawnCommandSeam;
  readonly fileop: FileopClient;
  readonly worktreePath: string;
  readonly toolHandlers?: ReadonlyMap<string, ToolHandler>;
  readonly screenPolicy?: ScreenPolicy;
  /** The run/turn abort. When it fires, in-flight children are interrupted + settled
   *  and resolve `child_aborted`. Absent ⇒ children are bounded only by the deadline. */
  readonly signal?: AbortSignal;
  /** Wall-clock bound for a single child turn; a child that never terminates resolves
   *  `child_timeout`. Defaults to {@link DEFAULT_CHILD_TURN_DEADLINE_MS}. */
  readonly childTurnDeadlineMs?: number;
  /** Ceiling on the accumulated child result text (bounds a runaway child stream). */
  readonly maxChildTextBytes?: number;
}

/** Default per-child wall-clock deadline (ms). Generous for a real subagent turn while
 *  finite, so a wedged child cannot hold the parent callback open forever. */
const DEFAULT_CHILD_TURN_DEADLINE_MS = 10 * 60 * 1000;

/** Ceiling on the child's accumulated result text, mirroring the advice harness's
 *  MAX_ADVICE_TEXT_BYTES: a hostile/looping child streaming near-cap frames would
 *  otherwise accrue unbounded text and OOM the worker. 8 MiB is generous for a
 *  subagent's report yet well below Node's default heap. */
const DEFAULT_MAX_CHILD_TEXT_BYTES = 8 * 1024 * 1024;

function asObject(v: unknown): Record<string, unknown> | undefined {
  return v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined;
}

function asString(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}

/** The first present non-empty string among `keys` on `args`, else undefined. */
function firstStr(args: unknown, keys: readonly string[]): string | undefined {
  const o = asObject(args);
  if (!o) return undefined;
  for (const k of keys) {
    const v = o[k];
    if (typeof v === "string" && v.length > 0) return v;
  }
  return undefined;
}

/** Extract non-empty assistant text off an item (bare `text` and/or a `content` array
 *  of `{ text }` parts). Mirrors the CodexHarness/advice decode. */
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

/** A child broker's delegate seam: nested delegation is impossible, so this is never
 *  reached (the child broker's `isRoot:false` grants deny a delegate at dispatch, before
 *  the seam). It exists only to satisfy the broker's required `delegate` input and
 *  fails closed if a future change ever routed to it. */
const denyNestedDelegate = async (): Promise<ChildDelegationResult> => ({
  ok: false,
  code: "child_denied",
  message: "nested delegation is denied",
});

/**
 * The synchronous child-thread delegation runner. Construct once with the run's role
 * table + seams; `toDelegateSeam()` yields the {@link DelegateSeam} the production
 * broker awaits.
 */
export class CodexDelegationRunner {
  private readonly registry: ExecutionRegistry;
  private readonly roles: ReadonlyMap<string, DelegationRole>;
  private readonly startChildTurn: StartChildTurnSeam;
  private readonly spawnCommand: SpawnCommandSeam;
  private readonly fileop: FileopClient;
  private readonly worktreePath: string;
  private readonly toolHandlers?: ReadonlyMap<string, ToolHandler>;
  private readonly screenPolicy?: ScreenPolicy;
  private readonly signal?: AbortSignal;
  private readonly childTurnDeadlineMs: number;
  private readonly maxChildTextBytes: number;

  constructor(opts: CodexDelegationRunnerOptions) {
    this.registry = opts.registry;
    this.roles = opts.roles;
    this.startChildTurn = opts.startChildTurn;
    this.spawnCommand = opts.spawnCommand;
    this.fileop = opts.fileop;
    this.worktreePath = opts.worktreePath;
    this.toolHandlers = opts.toolHandlers;
    this.screenPolicy = opts.screenPolicy;
    this.signal = opts.signal;
    this.childTurnDeadlineMs = opts.childTurnDeadlineMs ?? DEFAULT_CHILD_TURN_DEADLINE_MS;
    this.maxChildTextBytes = opts.maxChildTextBytes ?? DEFAULT_MAX_CHILD_TEXT_BYTES;
  }

  /** The seam the broker is constructed with. Bound so it can be passed directly. */
  toDelegateSeam(): (request: ChildDelegationRequest) => Promise<ChildDelegationResult> {
    return (request) => this.run(request);
  }

  /** Run one delegated child to full settlement and return its bounded result. */
  async run(request: ChildDelegationRequest): Promise<ChildDelegationResult> {
    const roleDef = this.roles.get(request.role);
    // Fail closed on an unknown role or a role that is NOT a subagent (isRoot). The
    // broker already checked its own allowedRoles set; this is the runner's own
    // independent guard so a mis-wired role table cannot run a root-graned child.
    if (roleDef === undefined || roleDef.grants.isRoot) {
      return { ok: false, code: "child_denied", message: "unknown or non-subagent delegation role" };
    }

    // Combined abort: the run/turn signal OR a per-child deadline. `timedOut` lets the
    // outcome distinguish a deadline (child_timeout) from a cancellation (child_aborted).
    const childAbort = new AbortController();
    let timedOut = false;
    let onParentAbort: (() => void) | undefined;
    const parentSignal = this.signal;
    if (parentSignal) {
      if (parentSignal.aborted) childAbort.abort();
      else {
        onParentAbort = (): void => childAbort.abort();
        parentSignal.addEventListener("abort", onParentAbort, { once: true });
      }
    }
    const deadlineTimer = setTimeout(() => {
      timedOut = true;
      childAbort.abort();
    }, this.childTurnDeadlineMs);
    deadlineTimer.unref?.();

    try {
      return await this.runChild(request, roleDef, childAbort.signal, () => timedOut);
    } finally {
      clearTimeout(deadlineTimer);
      if (onParentAbort && parentSignal) parentSignal.removeEventListener("abort", onParentAbort);
    }
  }

  private async runChild(
    request: ChildDelegationRequest,
    roleDef: DelegationRole,
    signal: AbortSignal,
    wasTimeout: () => boolean,
  ): Promise<ChildDelegationResult> {
    const taskInput = firstStr(request.args, ["prompt", "description", "task", "input", "message"]) ?? "";

    // Honor a stop that already fired before any child work begins.
    if (signal.aborted) {
      return { ok: false, code: wasTimeout() ? "child_timeout" : "child_aborted", message: "delegation cancelled before start" };
    }

    let controller: ChildThreadController;
    try {
      controller = await this.startChildTurn({
        role: request.role,
        systemPrompt: roleDef.systemPrompt,
        taskInput,
        model: roleDef.model,
        effort: roleDef.effort,
        parent: request.parent,
        signal,
      });
    } catch {
      if (signal.aborted) {
        return { ok: false, code: wasTimeout() ? "child_timeout" : "child_aborted", message: "delegation cancelled during start" };
      }
      return { ok: false, code: "child_failed", message: "the delegated child failed to start" };
    }

    // The CHILD broker: the SAME registry (so child callbacks join the run's quiescence
    // barrier), the child role's IMMUTABLE isRoot:false grants, the same command-identity
    // effect seams, NO delegation targets (a subagent cannot delegate), and a deny-only
    // delegate seam that a child's isRoot:false grants make unreachable anyway.
    const childBroker = new CodexCallbackBroker({
      registry: this.registry,
      spawnCommand: this.spawnCommand,
      fileop: this.fileop,
      worktreePath: this.worktreePath,
      grants: roleDef.grants,
      delegate: denyNestedDelegate,
      toolHandlers: this.toolHandlers,
      allowedRoles: new Set<string>(),
      screenPolicy: this.screenPolicy,
    });

    const pending = new Set<Promise<void>>();
    let text = "";
    let textBytes = 0;
    let terminalOutcome: "success" | "failed" | undefined;

    // Abort race so a cancel/deadline ends the child stream promptly (rule: the parent
    // must be able to cancel a child that is wedged in a never-settling callback).
    let onAbort: (() => void) | undefined;
    const abortPromise = new Promise<"aborted">((resolve) => {
      if (signal.aborted) resolve("aborted");
      else {
        onAbort = (): void => resolve("aborted");
        signal.addEventListener("abort", onAbort, { once: true });
      }
    });

    let aborted = false;
    const notes = controller.notifications();
    try {
      for (;;) {
        const step = await Promise.race([notes.next(), abortPromise]);
        if (step === "aborted") {
          aborted = true;
          break;
        }
        if (step.done) {
          // The child stream ended without a terminal for its turn: fail closed
          // (mirrors the harness's unexpected-EOF discipline) rather than report success.
          break;
        }
        const note = step.value;

        // A child server→client tool-call request: route to the CHILD broker with the
        // honest child origin. The broker await is itself raced against abort so a
        // never-settling child effect cannot make the child un-cancellable.
        if (note.kind === "activity" && note.requestId !== undefined) {
          const routed = this.routeChildToolCall(controller, childBroker, note, pending);
          const outcome = await Promise.race([routed, abortPromise]);
          if (outcome === "aborted") {
            aborted = true;
            break;
          }
          continue;
        }

        if (note.kind === "turn_completed") {
          // Serve ONLY the child's own turn; a stale/foreign completion is liveness.
          if (note.threadId !== controller.threadId || note.turnId !== controller.turnId) continue;
          terminalOutcome = note.status === "completed" ? "success" : "failed";
          break;
        }

        // Assistant text accumulation (bounded). Everything else is liveness only.
        if (note.kind === "activity") {
          const chunk = this.extractChildText(controller.threadId, note);
          if (chunk.length > 0) {
            const chunkBytes = Buffer.byteLength(chunk, "utf8");
            if (textBytes + chunkBytes <= this.maxChildTextBytes) {
              textBytes += chunkBytes;
              text += chunk;
            }
            // Over the ceiling: stop accumulating (keep the bounded prefix); the child
            // still runs to its terminal, which the loop captures.
          }
        }
      }
    } finally {
      if (onAbort) signal.removeEventListener("abort", onAbort);
      // FULL SETTLEMENT BARRIER: every child callback admitted above is awaited here
      // before the parent callback resolves. They are awaited inline in the loop too,
      // so this is belt-and-braces against any concurrently-admitted callback.
      await Promise.allSettled(pending);
      // Interrupt (a cancel/deadline) then idempotently close the child thread — the
      // child is fully torn down before we return to the parent broker.
      if (aborted) await controller.interrupt().catch(() => {});
      await controller.close().catch(() => {});
    }

    if (aborted) {
      return { ok: false, code: wasTimeout() ? "child_timeout" : "child_aborted", message: "the delegated child was cancelled" };
    }
    if (terminalOutcome === "success") {
      return { ok: true, output: { role: request.role, text } };
    }
    if (terminalOutcome === "failed") {
      return { ok: false, code: "child_failed", message: "the delegated child turn failed" };
    }
    // No terminal was observed (clean EOF before completion): fail closed.
    return { ok: false, code: "child_failed", message: "the delegated child ended before completion" };
  }

  /** Route ONE child `item/tool/call` to the child broker bound to the CHILD's honest
   *  identity, with origin `"child"`, and reply with the neutral result. Tracks its
   *  promise in `pending` for the settlement barrier. A callback whose ids do not match
   *  the active child turn is denied here (fail-closed) without reaching the broker. */
  private routeChildToolCall(
    controller: ChildThreadController,
    broker: CodexCallbackBroker,
    note: Extract<CodexNotification, { kind: "activity" }>,
    pending: Set<Promise<void>>,
  ): Promise<void> {
    const requestId = note.requestId;
    if (requestId === undefined) return Promise.resolve();
    const task = (async (): Promise<void> => {
      let result: CallbackResult;
      if (note.method !== "item/tool/call") {
        // Any other child server→client request is unsupported (a child has no extra
        // lane); answer fail-closed WITHOUT invoking the broker.
        this.safeRespond(controller, requestId, { error: { code: -32601, message: "unsupported request" } });
        return;
      }
      const p = asObject(note.params) ?? {};
      const rawThreadId = asString(p.threadId);
      const rawTurnId = asString(p.turnId);
      const matchesActive =
        rawThreadId !== undefined &&
        rawThreadId === controller.threadId &&
        rawTurnId !== undefined &&
        rawTurnId === controller.turnId;
      if (!matchesActive) {
        result = { ok: false, code: "not_active_turn", message: "callback does not match the active child turn" };
      } else {
        try {
          // Honest CHILD attribution + origin "child": the broker's own root-only gates
          // deny a subagent signal (submit_plan/signal_done) and a nested spawn_agent.
          result = await broker.handleToolCall(
            { threadId: rawThreadId, turnId: rawTurnId, callId: asString(p.callId) ?? "" },
            p.tool,
            p.arguments,
            "child",
          );
        } catch {
          result = { ok: false, code: "broker_error", message: "the callback failed" };
        }
      }
      this.safeRespond(controller, requestId, { result: this.replyBody(result) });
    })();
    pending.add(task);
    void task.finally(() => pending.delete(task));
    return task;
  }

  /** Map a broker {@link CallbackResult} to the app-server tool reply body (mirrors the
   *  run harness's `replyOf`): `{ success, contentItems:[{type,text}] }`. */
  private replyBody(result: CallbackResult): unknown {
    const text = result.ok ? this.stringify(result.output) : result.message;
    return { success: result.ok, contentItems: [{ type: "inputText", text }] };
  }

  private stringify(output: unknown): string {
    if (typeof output === "string") return output;
    try {
      return JSON.stringify(output) ?? "";
    } catch {
      return "";
    }
  }

  private safeRespond(
    controller: ChildThreadController,
    requestId: number | string,
    reply: { readonly result: unknown } | { readonly error: { readonly code: number; readonly message: string } },
  ): void {
    try {
      controller.respond(requestId, reply);
    } catch {
      // The child transport may be closing (abort/deadline raced this reply): an
      // undeliverable reply is dropped, never thrown.
    }
  }

  /** Extract child assistant text off an `item/completed` agent-message note bound to
   *  the child's own thread (a foreign/absent thread id contributes nothing). */
  private extractChildText(threadId: string, note: Extract<CodexNotification, { kind: "activity" }>): string {
    if (note.method !== "item/completed") return "";
    const params = asObject(note.params);
    if (params === undefined || params.threadId !== threadId) return "";
    const item = asObject(params.item);
    if (!item) return "";
    const type = asString(item.type);
    if (type === "agentMessage" || type === "assistantMessage" || type === "agent_message") {
      return extractText(item).join("");
    }
    return "";
  }
}
