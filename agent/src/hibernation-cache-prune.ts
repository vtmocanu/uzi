// Hibernation cache prune (PRD #4206): before a hosted persistent worker is hibernated the api
// asks it, through the heartbeat, to run the two frozen DinD prune commands exactly once while
// the worker holds the claim gate exclusively. Modelled on dind-maintenance.ts, with these
// differences: the work is the api's request (no local pressure trigger), the gate owner is
// "hibernation" and must be the SOLE holder, and the prune steps run to completion (no timeout,
// no abort signal: execDocker SIGKILLs on either) so a step is never killed half way.
//
// Safety posture, in order:
//   1. Only projections bound to this worker's current registration nonce are acted on.
//   2. At most once: a disposition per `${op_id}|${register_nonce}` is set synchronously, before
//      the first await. A key that settled, was declined or reported a result is tombstoned for
//      the registration's lifetime and never closes the gate, sends ready or spawns again; the
//      phase filter (requested < started < unknown < settled) ignores replayed older phases.
//   3. Exclusivity: the gate is closed under "hibernation", claims drained, and the gate's
//      generation pinned; any later close/open by another owner is non-exclusive.
//   4. Everything the api needs to know is a report: refused (nothing ran), ready, or one result
//      listing both commands exactly once.
import { spawn } from "node:child_process";
import { DOCKER_BIN, DIND_PRUNE_ARGV, DindPruneGate, assertDockerArgvAllowed, dockerExecEnvironment, execDocker, type DockerExec } from "./dind-prune.js";
import { DIND_VOLUME_SERVER_VERSION_ARGV } from "./dind-volume-argv.js";
import type {
  HibernationCachePrune, HibernationCachePruneReport, HibernationCachePruneStep, HibernationPruneCommand,
} from "./protocol.js";
import type { Logger } from "./log.js";
import { errMessage, sleep as abortableSleep } from "./util.js";

/** Read-only probe, in its own frozen table (never part of the prune allowlist). */
const PROBE = Object.freeze({ version: DIND_VOLUME_SERVER_VERSION_ARGV });
const PROBE_TIMEOUT_MS = 30_000;
/** Bound on waiting for another gate owner and for claims/work/custody to settle, from the first
 *  observation of the request. */
const ACQUIRE_BOUND_MS = 60_000;
/** A started projection older than this since ready was sent is not trusted to start work. */
const START_WINDOW_MS = 45_000;

/** The only two commands a step may run, in order. */
const STEPS: readonly { readonly command: HibernationPruneCommand; readonly argv: readonly string[] }[] = Object.freeze([
  Object.freeze({ command: "image_prune_dangling" as const, argv: DIND_PRUNE_ARGV.pruneDangling }),
  Object.freeze({ command: "builder_prune" as const, argv: DIND_PRUNE_ARGV.pruneBuildCache }),
]);
for (const step of STEPS) assertDockerArgvAllowed(step.argv);

const RANK = { requested: 1, started: 2, unknown: 3, settled: 4 } as const;

/** How a prune child ended; only a clean exit 0 is `exited_ok`. */
export type StepExit = { code: number | null; signal: string | null } | { error: string };
/** Runs one step to its `close` event with no timeout and no abort signal. */
export type StepRunner = (argv: readonly string[], opts: { file: string; env: NodeJS.ProcessEnv }) => Promise<StepExit>;

const spawnStep: StepRunner = (argv, opts) =>
  new Promise((resolve) => {
    let done = false;
    const finish = (exit: StepExit) => { if (!done) { done = true; resolve(exit); } };
    try {
      const child = spawn(opts.file, [...argv], { env: opts.env, stdio: "ignore" });
      child.once("error", (err) => { if (child.pid === undefined) finish({ error: errMessage(err) }); });
      child.once("close", (code, signal) => finish({ code, signal }));
    } catch (err) {
      finish({ error: errMessage(err) });
    }
  });

type Reason = NonNullable<HibernationCachePruneReport["reason"]>;

interface Attempt {
  key: string;
  opId: string;
  nonce: string;
  rank: number;
  seq: number;
  cancel: boolean;
  disposition?: "executing" | "declined";
  tombstoned: boolean;
  acquireStartedAt?: number;
  held: boolean;
  generation: number;
  probed: boolean;
  armed: boolean;
  readySentAt?: number;
  readyDropped: boolean;
  refusal?: Reason;
  refusalDone: boolean;
  result?: { steps: HibernationCachePruneStep[]; throughout: boolean };
  resultSeq?: number;
  pendingSeq?: number;
  /** Live step list while executing, so a shutdown can report the in-flight step. */
  live?: { steps: HibernationCachePruneStep[]; running: number };
  releasePending: boolean;
}

interface Options {
  gate: DindPruneGate;
  dockerHost: string;
  heartbeatIntervalMs: number;
  isIdle: () => boolean;
  log: Logger;
  exec?: DockerExec;
  runStep?: StepRunner;
  dockerBin?: string;
  now?: () => number;
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
}

function keyOf(opId: string, nonce: string): string { return `${opId}|${nonce}`; }

function notStarted(): HibernationCachePruneStep[] {
  return STEPS.map((s) => ({ command: s.command, state: "not_started" as const }));
}

/** Construct only for a worker with Docker wiring and the prune flag on (see main.ts). */
export class HibernationCachePruneController {
  private readonly now: () => number;
  private readonly sleep: (ms: number, signal?: AbortSignal) => Promise<void>;
  private readonly exec: DockerExec;
  private readonly runStep: StepRunner;
  private nonce?: string;
  private enabled = false;
  private lastBusyAt = 0;
  private custody?: { retaining: boolean | undefined; sentAt: number };
  private attempts = new Map<string, Attempt>();
  /** The attempt the latest projection names (undefined when it named none). */
  private current?: Attempt;
  private held?: Attempt;
  private child?: Promise<void>;
  private probing = false;
  private stopping = false;

  constructor(private readonly deps: Options) {
    this.now = deps.now ?? Date.now;
    this.sleep = deps.sleep ?? abortableSleep;
    this.exec = deps.exec ?? execDocker;
    this.runStep = deps.runStep ?? spawnStep;
  }

  get gate(): DindPruneGate { return this.deps.gate; }

  /** A new registration nonce clears every disposition and tombstone; an unchanged one keeps them.
   *  A held gate is released at once unless a child is still running (then at its close). */
  register(nonce: string | undefined): void {
    if (nonce !== this.nonce) {
      this.attempts = new Map();
      this.current = undefined;
      if (this.held) this.held.releasePending = true;
    }
    this.nonce = nonce;
    this.enabled = !!nonce && !!this.deps.dockerHost;
    this.lastBusyAt = this.now();
    this.custody = undefined;
    this.releaseIfSettled();
  }

  noteActivityEnded(): void {
    this.lastBusyAt = this.now();
  }

  /** Feed one heartbeat's projection (null: none; undefined: unknown/malformed/failed) with the
   *  api's custody flag, stamped with that heartbeat's SEND time. */
  observe(projection: HibernationCachePrune | null | undefined, retaining: boolean | undefined, sentAtMs: number): void {
    this.custody = { retaining, sentAt: sentAtMs };
    if (!this.enabled || projection === undefined) return;
    if (projection === null) { this.onNone(); return; }
    if (projection.register_nonce !== this.nonce) return;
    const rank = RANK[projection.phase];
    const key = keyOf(projection.op_id, projection.register_nonce);
    let a = this.attempts.get(key);
    if (!a) {
      a = { key, opId: projection.op_id, nonce: projection.register_nonce, rank: 0, seq: -1, cancel: false,
        tombstoned: false, held: false, generation: 0, probed: false, armed: false, readyDropped: false,
        refusalDone: false, releasePending: false };
      this.attempts.set(key, a);
    }
    if (rank < a.rank) return; // a replayed older phase
    a.rank = rank;
    a.seq = projection.reconcile_seq;
    a.cancel = a.cancel || projection.cancel_requested;
    // A different attempt superseding a held one that has no child: the held one is over.
    if (this.held && this.held !== a && !this.child) { this.held.releasePending = true; this.releaseIfSettled(); }
    this.current = a;
    if (projection.phase === "requested") this.onRequested(a);
    else if (projection.phase === "started") this.onStarted(a);
    else if (projection.phase === "unknown") this.onUnknown(a);
    else this.onSettled(a);
  }

  private onNone(): void {
    const a = this.current;
    this.current = undefined;
    if (this.held) this.held.releasePending = true;
    if (a && !a.tombstoned && a.disposition === undefined) {
      a.tombstoned = true;
      a.disposition = "declined";
      a.refusalDone = true; // nothing left to report on
    }
    this.releaseIfSettled();
  }

  private onRequested(a: Attempt): void {
    if (a.tombstoned || a.disposition !== undefined) return;
    if (a.cancel) { this.refuse(a, "cancelled"); return; }
    a.acquireStartedAt ??= this.now();
  }

  private onUnknown(a: Attempt): void {
    if (a.disposition === undefined && !a.tombstoned) {
      a.tombstoned = true;
      a.disposition = "declined";
      a.refusalDone = true;
      a.releasePending = true;
      this.releaseIfSettled();
    }
  }

  private onSettled(a: Attempt): void {
    a.tombstoned = true;
    a.releasePending = true;
    this.releaseIfSettled();
  }

  private onStarted(a: Attempt): void {
    if (a.tombstoned || a.disposition !== undefined) return;
    const timely = a.armed && a.readySentAt !== undefined && this.now() - a.readySentAt <= START_WINDOW_MS;
    if (!timely || a.cancel || this.child || this.stopping || this.unsafe(a) !== undefined) {
      this.declineWithResult(a);
      return;
    }
    a.disposition = "executing";
    this.child = this.execute(a).finally(() => {
      this.child = undefined;
      this.releaseIfSettled();
    });
  }

  private releaseIfSettled(): void {
    const a = this.held;
    if (!a || !a.releasePending || this.child) return;
    this.deps.gate.open("hibernation");
    a.held = false;
    a.releasePending = false;
    this.held = undefined;
  }

  /** True only while this attempt still holds the gate alone and no other owner touched it. */
  private exclusive(a: Attempt): boolean {
    return a.held && this.held === a && this.deps.gate.onlyHeldBy("hibernation") &&
      this.deps.gate.generation() === a.generation;
  }

  /** The first failing safety condition, or undefined when it is safe to proceed. */
  private unsafe(a: Attempt): Reason | undefined {
    if (!this.enabled || a.nonce !== this.nonce || !this.exclusive(a)) return "gate_not_exclusive";
    if (this.deps.gate.inFlightClaims() > 0) { this.lastBusyAt = this.now(); return "claim_in_flight"; }
    if (!this.deps.isIdle()) { this.lastBusyAt = this.now(); return "active_work"; }
    const c = this.custody;
    const age = c ? this.now() - c.sentAt : Infinity;
    if (!c || c.retaining !== false || age < 0 || age > 2 * this.deps.heartbeatIntervalMs || c.sentAt <= this.lastBusyAt) {
      return "custody_stale";
    }
    return undefined;
  }

  private refuse(a: Attempt, reason: Reason): void {
    a.disposition = "declined";
    a.tombstoned = true;
    a.refusal = reason;
  }

  /** Reports every step not_started; the gate stays held until settled/none, like any result. */
  private declineWithResult(a: Attempt): void {
    a.disposition = "declined";
    a.tombstoned = true;
    a.result = { steps: notStarted(), throughout: this.exclusive(a) };
  }

  /** Run the two steps in order; no further step after a cancel, a failed revalidation or a
   *  step that did not exit cleanly. The result is recorded at the child's close. */
  private async execute(a: Attempt): Promise<void> {
    const steps = notStarted();
    a.live = { steps, running: -1 };
    let throughout = true;
    try {
      for (let i = 0; i < STEPS.length; i++) {
        const step = STEPS[i]!;
        if (a.cancel || this.stopping) break;
        if (this.unsafe(a) !== undefined) { throughout = false; break; }
        assertDockerArgvAllowed(step.argv);
        a.live.running = i;
        steps[i]!.state = "unknown"; // until the child closes cleanly
        const exit = await this.runStep(step.argv, { file: this.deps.dockerBin ?? DOCKER_BIN, env: dockerExecEnvironment(this.deps.dockerHost) });
        a.live.running = -1;
        const clean = "code" in exit && exit.code === 0 && exit.signal === null;
        steps[i]!.state = clean ? "exited_ok" : "unknown";
        // Custody staleness is the next step's pre-spawn check; exclusivity and quiet are throughout's.
        const after = this.unsafe(a);
        if (after !== undefined && after !== "custody_stale") throughout = false;
        if (!clean) break;
      }
    } catch (err) {
      this.deps.log.warn("hibernation-cache-prune: step failed", { error: errMessage(err) });
      throughout = false;
    } finally {
      a.live = undefined;
      a.result = { steps, throughout };
      a.tombstoned = true;
    }
  }

  /** The report to send with this heartbeat (at most one), revalidated now. */
  report(): HibernationCachePruneReport | undefined {
    const a = this.current;
    if (!this.enabled || !a || a.nonce !== this.nonce) return undefined;
    const base = { op_id: a.opId, register_nonce: a.nonce };
    if (a.refusal && !a.refusalDone) return { ...base, kind: "refused", reason: a.refusal };
    if (a.result && (a.rank === RANK.started || a.rank === RANK.unknown) && a.resultSeq !== a.seq) {
      a.pendingSeq = a.seq;
      return { ...base, kind: "result", steps: a.result.steps.map((s) => ({ ...s })), gate_exclusive_throughout: a.result.throughout };
    }
    if (a.rank === RANK.requested && a.armed && !a.tombstoned && a.disposition === undefined &&
        !a.readyDropped && !this.child) {
      const reason = this.unsafe(a);
      if (reason) {
        this.refuse(a, reason);
        return { ...base, kind: "refused", reason };
      }
      a.readySentAt = this.now();
      return {
        ...base, kind: "ready", local_claims: 0, local_executions: 0, custody_clear: true,
        custody_checked_at: new Date(this.custody!.sentAt).toISOString(), gate_exclusive: true,
      };
    }
    return undefined;
  }

  /** The caller's outcome for a report returned by {@link report}: `sent` (accepted), `dropped`
   *  (the api answered 409: never resend it) or `failed` (the heartbeat did not land: resend). */
  reported(report: HibernationCachePruneReport, outcome: "sent" | "dropped" | "failed"): void {
    const a = this.attempts.get(keyOf(report.op_id, report.register_nonce));
    if (!a || outcome === "failed") return;
    if (report.kind === "ready") {
      if (outcome === "dropped") a.readyDropped = true;
    } else if (report.kind === "refused") {
      a.refusalDone = true;
    } else {
      a.resultSeq = a.pendingSeq ?? a.seq;
    }
  }

  /** Advance the acquisition of the current requested attempt; never blocks on a gate owner. */
  async tick(signal?: AbortSignal): Promise<void> {
    const a = this.current;
    if (signal?.aborted || !this.enabled || this.stopping || this.probing || !a || a.tombstoned ||
        a.disposition !== undefined || a.rank !== RANK.requested || a.armed || a.acquireStartedAt === undefined) return;
    const now = this.now();
    // Never ready a new op while an earlier child is unclosed: its wait does not count down.
    if (this.child) { a.acquireStartedAt = now; return; }
    // An earlier attempt's hold (its child has closed) is superseded by this newer request.
    if (this.held && this.held !== a) { this.held.releasePending = true; this.releaseIfSettled(); }
    if (a.cancel) { this.refuse(a, "cancelled"); return; }
    const expired = now - a.acquireStartedAt >= ACQUIRE_BOUND_MS;
    if (!a.held) {
      if (this.deps.gate.claimsClosed()) {
        if (expired) this.refuse(a, "gate_not_exclusive");
        return;
      }
      this.deps.gate.close("hibernation");
      a.held = true;
      a.generation = this.deps.gate.generation();
      this.held = a;
      this.lastBusyAt = now; // claims admitted before the close must be out of the custody evidence
    }
    const reason = this.unsafe(a);
    if (reason === "gate_not_exclusive") { this.refuse(a, reason); return; }
    if (reason) {
      if (expired) this.refuse(a, reason);
      return;
    }
    if (!a.probed) {
      this.probing = true;
      try {
        await this.exec(PROBE.version, { file: this.deps.dockerBin ?? DOCKER_BIN, env: dockerExecEnvironment(this.deps.dockerHost), timeoutMs: PROBE_TIMEOUT_MS, ...(signal ? { signal } : {}) });
      } catch (err) {
        this.deps.log.debug("hibernation-cache-prune: docker probe failed", { error: errMessage(err) });
        if (!a.tombstoned) this.refuse(a, "daemon_unreachable");
        return;
      } finally {
        this.probing = false;
      }
      if (a.tombstoned || a.disposition !== undefined) return;
      a.probed = true;
    }
    a.armed = true;
  }

  /** Serial ticks until aborted; on abort an in-flight step is reported unknown (best effort)
   *  through `flush` and its child is left to the process exit. */
  async loop(signal: AbortSignal, flush?: (report: HibernationCachePruneReport) => Promise<void>): Promise<void> {
    while (!signal.aborted) {
      try {
        await this.tick(signal);
        await this.sleep(1000, signal);
      } catch (err) {
        if (signal.aborted) break;
        this.deps.log.warn("hibernation-cache-prune: evaluation failed", { error: errMessage(err) });
        break; // a broken sleep must not spin
      }
    }
    await this.shutdown(flush);
  }

  private async shutdown(flush?: (report: HibernationCachePruneReport) => Promise<void>): Promise<void> {
    this.stopping = true;
    const a = this.held;
    if (!this.child || !a?.live) return;
    const steps = a.live.steps.map((s, i) => ({ ...s, state: i === a.live!.running ? "unknown" as const : s.state }));
    a.result = { steps, throughout: false };
    if (!flush) return;
    try {
      await flush({ op_id: a.opId, register_nonce: a.nonce, kind: "result", steps, gate_exclusive_throughout: false });
    } catch (err) {
      this.deps.log.debug("hibernation-cache-prune: shutdown report not delivered", { error: errMessage(err) });
    }
  }
}
