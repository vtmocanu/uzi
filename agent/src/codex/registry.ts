// PRD #1171 (M3, milestone 1, first unit): the immutable per-run *local execution
// epoch* registry. This is the fail-closed heart of the Codex execution spine.
//
// It owns, for one bound claim, every provider/command/boundary-action supervisor
// root, every launch reservation and every worker-callback reservation, and it is
// the sole authority for the child-quiescence, every-root reap and tool-disposal
// transitions the neutral `RunHarness` contract exposes. It is PURE logic: no real
// process is ever spawned or waited on here. A root is any object satisfying
// `RegisteredRoot`, so a test injects fakes and a later unit injects the real M3a
// supervisor roots. The actual worker-callback *effects* are the m2 broker's job;
// here we keep only the reservation / idempotency / poison bookkeeping.
//
// The local execution epoch is a distinct BRANDED domain from the API
// `codex_claim_epoch` (credential capability/reclaim authority). The two are never
// compared, copied or substituted; a new claim constructs a NEW registry with a
// NEW local execution epoch (PRD #1171 "Frozen boundaries", ADR-1106).

import type { ChildQuiescence, HarnessError, ProcessReap, ToolDisposal } from "../harness.js";

// --- Local execution epoch: a branded number, module-private brand. -----------
// Branding is compile-time only; at runtime it is a plain number. It exists so an
// API claim epoch (also a number) can never be passed where a local execution
// epoch is required, and vice-versa, within adapter/safety code.
declare const localExecutionEpochBrand: unique symbol;
export type LocalExecutionEpoch = number & { readonly [localExecutionEpochBrand]: true };

/** Construct a local execution epoch from a raw number. The safety/adapter wiring
 *  mints one per run; tests mint deterministic values. This is intentionally the
 *  ONLY way to obtain the branded type, so a claim epoch cannot masquerade as one. */
export function newLocalExecutionEpoch(value: number): LocalExecutionEpoch {
  return value as LocalExecutionEpoch;
}

export type RootKind = "provider" | "command" | "boundary_action";

/** The result of asking a supervisor root to reap its descendants. `ok: true` is
 *  only ever returned after the root observed `ECHILD` (using `__WALL` where
 *  required), never inferred from a CLI exit, a process-group kill or one root. */
export type ReapOutcome = { ok: true } | { ok: false; error: HarnessError };

/** The minimal contract a supervisor root must satisfy to be owned by the registry.
 *  Real roots wrap the M3a launcher/supervisor; tests inject fakes. */
export interface RegisteredRoot {
  readonly kind: RootKind;
  /** Resolves `{ ok: true }` ONLY on an observed `ECHILD` (+`__WALL`) emptiness for
   *  this root's descendants; otherwise `{ ok: false, error }`. Must respect the
   *  deadline and never wait unbounded. */
  reap(deadlineMs: number): Promise<ReapOutcome>;
  /** Idempotent teardown of the root's handlers/listeners/transport. */
  dispose(deadlineMs: number): Promise<void>;
}

export type RegistryState = "open" | "closing" | "closed" | "poisoned" | "disposed";

// --- Launch reservations ------------------------------------------------------

/** A token handed back by {@link ExecutionRegistry.reserveLaunch}. It MUST be
 *  settled before quiescence, either by {@link ExecutionRegistry.registerRoot}
 *  (the launch produced a root) or {@link ExecutionRegistry.cancelReservation}
 *  (the launch was aborted before it spawned anything). An unsettled reservation
 *  at quiesce poisons the epoch. */
export interface LaunchReservation {
  readonly id: string;
  readonly kind: RootKind;
}

export type LaunchReservationResult =
  | { kind: "reserved"; reservation: LaunchReservation }
  | { kind: "denied"; reason: "admission_closed" };

/** The result of {@link ExecutionRegistry.registerRoot}. A failed registration
 *  (unknown/already-settled reservation or a root-kind mismatch) poisons the epoch
 *  AND returns `{ ok: false, error }`, so a caller can never keep using an
 *  unadmitted (now-poisoned) root — it must tear the just-spawned root down instead
 *  of reaping it. */
export type RegisterRootResult =
  | { readonly ok: true }
  | { readonly ok: false; readonly error: HarnessError };

// --- Callback reservations (idempotency table; effects are m2's job) ----------

/** The full tuple a worker callback is keyed by. Reservation is per (registry
 *  instance, threadId, turnId, callId); a different thread/turn with the same
 *  callId is a DISTINCT reservation. */
export interface CallbackKey {
  readonly threadId: string;
  readonly turnId: string;
  readonly callId: string;
}

export interface CallbackReservationRequest extends CallbackKey {
  /** A stable fingerprint of the callback's payload + origin. Reuse of the same
   *  (thread, turn, call) tuple with a DIFFERENT fingerprint is a replay/forgery
   *  signal: it is denied AND poisons the epoch. */
  readonly fingerprint: string;
}

/** A bounded cached terminal marker. Deliberately carries NO payload, just the
 *  settled outcome class, so the idempotency table cannot grow with model data. */
export interface CallbackTerminalMarker {
  readonly settled: true;
  readonly outcome: "ok" | "error";
}

/** An admission token for a freshly reserved callback; the broker settles it via
 *  {@link ExecutionRegistry.settleCallback} once its effect (or owned child turn)
 *  terminates. */
export interface CallbackToken {
  readonly key: string;
}

export type CallbackReservationResult =
  | { kind: "admitted"; token: CallbackToken }
  | { kind: "replay"; marker: CallbackTerminalMarker }
  | {
      kind: "denied";
      reason: "admission_closed" | "in_flight_duplicate" | "changed_reuse" | "reservation_ceiling";
    };

interface CallbackRecord {
  readonly fingerprint: string;
  marker?: CallbackTerminalMarker;
}

interface RootRecord {
  readonly root: RegisteredRoot;
  reaped: boolean;
  disposed: boolean;
}

function keyOf(k: CallbackKey): string {
  // JSON-encode the tuple so no delimiter choice can let two distinct tuples
  // collide into one key regardless of what characters an id contains.
  return JSON.stringify([k.threadId, k.turnId, k.callId]);
}

/** Per-run ceiling on DISTINCT callback reservations retained until disposal.
 *  Call-ids are model/worker-influenced, so the idempotency table is an untrusted-
 *  input-sized structure; without a bound a run could be steered into unbounded
 *  memory growth. A single Codex run legitimately reserves far fewer than this, so
 *  10000 is generous headroom while still finite. Crossing it is treated as an
 *  attack/protocol fault: we FAIL CLOSED (poison + deny) rather than grow. Only
 *  distinct NEW keys count toward the ceiling; replays/duplicates of an already-
 *  tracked key are idempotent reads and never enlarge the table. Exported so the
 *  test asserts the exact ceiling rather than hard-coding a copy of it. */
export const MAX_CALLBACK_RESERVATIONS = 10000;

/** Backstop bound on the number of poison reasons the registry RETAINS. `poison()`
 *  can be driven by model/worker-influenced input (e.g. a flood of changed-
 *  fingerprint callback reuse on one tuple), so an UNBOUNDED poison list is itself
 *  an unbounded-model-influenced-memory-growth vector — the same class the callback
 *  ceiling closes, on the poison accumulator instead of the callbacks map. We retain
 *  only the FIRST MAX_POISON_ERRORS reasons (the earliest are the most diagnostic)
 *  and drop the rest. This is a bound on retained EVIDENCE only: the sticky
 *  `poisoned` flag, `isPoisoned()` and the "poisoned" state are unaffected, so
 *  fail-closed behaviour never changes no matter how many times `poison()` is
 *  called. 64 is generous headroom for a run's legitimate distinct fault reasons
 *  while staying small and finite. Exported so the test asserts the exact bound
 *  rather than hard-coding a copy of it. */
export const MAX_POISON_ERRORS = 64;

/**
 * The per-run immutable local-execution-epoch registry.
 *
 * State machine: `open -> closing -> closed`, with `poisoned` reachable (STICKY)
 * from any non-disposed state and `disposed` reachable from any state. Admission
 * (new launch/callback reservations) is open ONLY in the `open` state.
 */
export class ExecutionRegistry {
  private readonly epochValue: LocalExecutionEpoch;
  private currentState: RegistryState = "open";
  private readonly poisonList: HarnessError[] = [];
  // STICKY poison witness. `state()` reports "poisoned" only until disposal masks
  // it as "disposed" (poison then survives ONLY in poisonList); this flag is set on
  // ANY poison and NEVER cleared, so callers can ask "was this epoch ever poisoned?"
  // independent of the disposal-masked state enum.
  private poisoned = false;

  private readonly launches = new Map<string, LaunchReservation>();
  private readonly roots: RootRecord[] = [];
  private readonly callbacks = new Map<string, CallbackRecord>();
  private launchSeq = 0;

  // A single in-flight quiesce waiter, installed by `quiesceChildren` while it waits
  // bounded for accepted work to settle. Overlapping boundaries are serialized by the
  // safety facade, so at most ONE quiesce is ever in flight, hence a single waiter.
  private quiesceWaiter?: () => void;

  constructor(epoch: LocalExecutionEpoch) {
    this.epochValue = epoch;
  }

  // --- getters (for the safety facade and tests) ------------------------------

  state(): RegistryState {
    return this.currentState;
  }

  /** The frozen local execution epoch. Branded so it cannot be confused with the
   *  API claim epoch. */
  epoch(): LocalExecutionEpoch {
    return this.epochValue;
  }

  poisonErrors(): readonly HarnessError[] {
    return this.poisonList;
  }

  /** STICKY: true if this epoch was EVER poisoned, even after disposal has moved
   *  `state()` to "disposed" and masked the "poisoned" enum. Fail-closed callers
   *  consult this so a poisoned-then-disposed epoch can never read as clean. */
  isPoisoned(): boolean {
    return this.poisoned;
  }

  pendingLaunchCount(): number {
    return this.launches.size;
  }

  inFlightCallbackCount(): number {
    let n = 0;
    for (const rec of this.callbacks.values()) if (!rec.marker) n++;
    return n;
  }

  rootCount(): number {
    return this.roots.length;
  }

  /** True if any registered `command` root has not yet reaped. The PAT-bearing
   *  boundary-action guard ([R3-2]) consults this INDEPENDENTLY of any reap result
   *  it was handed, so a mis-reported reap cannot let a worker-PAT action spawn
   *  while a model-authorized command process is still alive. */
  hasLiveCommandRoot(): boolean {
    return this.roots.some((r) => r.root.kind === "command" && !r.reaped);
  }

  private admissionOpen(): boolean {
    return this.currentState === "open";
  }

  // --- poison (sticky) --------------------------------------------------------

  /** Record a poison. STICKY: once poisoned the registry stays poisoned until
   *  disposed. May be called from any non-disposed state. */
  poison(errors: HarnessError | readonly HarnessError[]): void {
    const list = Array.isArray(errors) ? errors : [errors];
    // Append only while under MAX_POISON_ERRORS; once at the cap we stop retaining
    // reasons (keeping the FIRST, most-diagnostic ones). This is the unconditional
    // backstop for ALL poison sources — present and future — so that no number of
    // poison() calls, however model/worker-driven, can grow the accumulator without
    // bound. The sticky flag/state below are set REGARDLESS of the cap, so the
    // registry still fails closed exactly as before.
    for (const e of list) {
      if (this.poisonList.length >= MAX_POISON_ERRORS) break;
      this.poisonList.push(e);
    }
    this.poisoned = true; // sticky; never cleared, survives disposal
    // A mid-wait poison (e.g. a changed-fingerprint reuse) is a real protocol fault:
    // short-circuit any bounded quiesce wait so it returns incomplete promptly. This
    // fires BEFORE the state flip below because `maybeSignalQuiesce` only releases the
    // waiter while the state is still "closing"; once we set "poisoned" the guard
    // would suppress it.
    this.maybeSignalQuiesce();
    if (this.currentState !== "disposed") this.currentState = "poisoned";
  }

  // --- launch reservations ----------------------------------------------------

  reserveLaunch(kind: RootKind): LaunchReservationResult {
    // The trusted boundary-action lane: once admission has CLOSED (post-quiesce),
    // ordinary provider/command launches stay refused, but a permitted checkpoint/
    // git/finalize action may still reserve a `boundary_action` root. Poisoned and
    // disposed refuse everything.
    const allowed =
      this.currentState === "open" ||
      (kind === "boundary_action" && this.currentState === "closed");
    if (!allowed) return { kind: "denied", reason: "admission_closed" };
    this.launchSeq += 1;
    const reservation: LaunchReservation = { id: `launch-${this.launchSeq}`, kind };
    this.launches.set(reservation.id, reservation);
    return { kind: "reserved", reservation };
  }

  /** Settle a launch reservation with the root it produced. The root's declared
   *  kind must match the reservation's. Returns a checked result: on an unknown/
   *  already-settled reservation or a kind mismatch it poisons AND returns
   *  `{ ok: false, error }`, so the caller tears the just-spawned root down instead
   *  of continuing to reap an unadmitted (now-poisoned) root. */
  registerRoot(reservation: LaunchReservation, root: RegisteredRoot): RegisterRootResult {
    const held = this.launches.get(reservation.id);
    if (!held) {
      const error: HarnessError = {
        category: "protocol",
        message: "registerRoot: unknown or already-settled reservation",
      };
      this.poison(error);
      return { ok: false, error };
    }
    if (reservation.kind !== held.kind || root.kind !== held.kind) {
      const error: HarnessError = {
        category: "protocol",
        message: "registerRoot: root kind does not match reservation",
      };
      this.poison(error);
      return { ok: false, error };
    }
    this.launches.delete(reservation.id);
    this.roots.push({ root, reaped: false, disposed: false });
    // A launch reservation just settled; a bounded quiesce wait may now be complete.
    this.maybeSignalQuiesce();
    return { ok: true };
  }

  /** Settle a launch reservation as aborted (nothing spawned). */
  cancelReservation(reservation: LaunchReservation): void {
    this.launches.delete(reservation.id);
    // A launch reservation just settled; a bounded quiesce wait may now be complete.
    this.maybeSignalQuiesce();
  }

  // --- callback reservations (idempotency bookkeeping only) -------------------

  reserveCallback(request: CallbackReservationRequest): CallbackReservationResult {
    const key = keyOf(request);
    const existing = this.callbacks.get(key);
    // An already-tracked key is handled BEFORE the admission/ceiling gates: it
    // enlarges nothing (so it cannot breach the ceiling) and a cached terminal
    // marker is an idempotent read that stays safe to return even once admission
    // has closed or the epoch was poisoned.
    if (existing) {
      if (existing.fingerprint !== request.fingerprint) {
        // Same tuple, changed payload/origin: replay/forgery. Deny AND poison — but
        // poison ONLY on the FIRST detection. Once the epoch is already poisoned, a
        // flood of changed-fingerprint reuse of the same tuple denies cheaply and
        // must NOT re-enter poison() (which would grow the poison accumulator once
        // per call — the availability regression this branch previously reopened).
        // Fail-closed is preserved: the registry stays poisoned and every such call
        // is still denied `changed_reuse`.
        if (!this.poisoned) {
          this.poison({
            category: "protocol",
            message: "reserveCallback: call-id reuse with changed payload/origin",
          });
        }
        return { kind: "denied", reason: "changed_reuse" };
      }
      if (existing.marker) return { kind: "replay", marker: existing.marker };
      // Same tuple, same fingerprint, still in flight: a concurrent duplicate. We
      // cannot hand back a terminal yet and MUST NOT run a second effect, so deny.
      return { kind: "denied", reason: "in_flight_duplicate" };
    }
    // A NEW distinct key: gated by admission, then by the per-run reservation
    // ceiling. Crossing the ceiling is a fail-closed fault — poison + deny — since
    // call-ids are model/worker-influenced and must not drive unbounded growth.
    if (!this.admissionOpen()) return { kind: "denied", reason: "admission_closed" };
    if (this.callbacks.size >= MAX_CALLBACK_RESERVATIONS) {
      this.poison({
        category: "protocol",
        message: `reserveCallback: reservation ceiling (${MAX_CALLBACK_RESERVATIONS}) exceeded`,
      });
      return { kind: "denied", reason: "reservation_ceiling" };
    }
    this.callbacks.set(key, { fingerprint: request.fingerprint });
    return { kind: "admitted", token: { key } };
  }

  /** Settle an admitted callback with a bounded terminal marker so a later replay
   *  of the identical tuple returns it rather than re-running the effect. */
  settleCallback(token: CallbackToken, outcome: "ok" | "error"): void {
    const rec = this.callbacks.get(token.key);
    if (!rec) {
      this.poison({ category: "protocol", message: "settleCallback: unknown callback token" });
      return;
    }
    rec.marker = { settled: true, outcome };
    // An admitted callback just settled; a bounded quiesce wait may now be complete.
    this.maybeSignalQuiesce();
  }

  // --- boundary transitions ---------------------------------------------------

  /**
   * `open -> closing -> closed`. Refuses new admissions, then BOUNDEDLY WAITS up to
   * `deadlineMs` for accepted work to settle — every launch reservation settled (via
   * {@link registerRoot}/{@link cancelReservation}) AND every admitted callback
   * settled (via {@link settleCallback}). It poisons ONLY at the deadline (work that
   * genuinely never settled) or on a real protocol fault (the epoch becoming poisoned
   * during the wait, e.g. a changed-fingerprint reuse). This honours the
   * settle-before-boundary contract: a cancel racing a normal in-flight callback that
   * would settle in time now completes quiescent instead of being poisoned on sight.
   *
   * Overlapping boundaries are serialized by the safety facade, so only ONE quiesce is
   * ever in flight at a time and a single {@link quiesceWaiter} suffices.
   *
   * Any still-unsettled reservation at the deadline returns `{ kind: "incomplete",
   * errors }` and POISONS the epoch. A clean pass returns `{ kind: "quiescent", epoch }`
   * with the frozen epoch.
   */
  async quiesceChildren(deadlineMs: number): Promise<ChildQuiescence> {
    if (this.currentState === "disposed") {
      return { kind: "incomplete", errors: [{ category: "protocol", message: "quiesceChildren: registry disposed" }] };
    }
    if (this.currentState === "poisoned") {
      return { kind: "incomplete", errors: this.snapshotPoison() };
    }
    // Already quiesced: idempotently report the frozen epoch, but DEFENSE-IN-DEPTH
    // re-assert emptiness first. A boundary_action root reserved through the trusted
    // post-close lane (or any callback still in flight) that never settled means we
    // are no longer quiescent; report incomplete + poison rather than blindly
    // reporting quiescent on a re-quiesce.
    if (this.currentState === "closed") {
      const reErrors: HarnessError[] = [];
      if (this.launches.size > 0) {
        reErrors.push({
          category: "protocol",
          message: `quiesceChildren: ${this.launches.size} launch reservation(s) unsettled at re-quiesce`,
        });
      }
      const reUnsettled = this.inFlightCallbackCount();
      if (reUnsettled > 0) {
        reErrors.push({
          category: "protocol",
          message: `quiesceChildren: ${reUnsettled} callback/child-turn reservation(s) unsettled at re-quiesce`,
        });
      }
      if (reErrors.length > 0) {
        this.poison(reErrors);
        return { kind: "incomplete", errors: reErrors };
      }
      return { kind: "quiescent", epoch: this.epochValue };
    }
    // Close admission, then wait bounded for accepted work to settle.
    this.currentState = "closing";
    const settled = (): boolean => this.launches.size === 0 && this.inFlightCallbackCount() === 0;
    if (!settled() && !this.poisoned && deadlineMs > 0) {
      await this.waitForSettlement(deadlineMs);
    }
    // The wait is over (settled, poisoned mid-wait, or the deadline elapsed): drop the
    // waiter so a late settle/poison signal after this point is a no-op.
    this.quiesceWaiter = undefined;
    // A real protocol fault during the wait poisons: report it, do not report clean.
    if (this.poisoned) {
      return { kind: "incomplete", errors: this.snapshotPoison() };
    }
    // Build errors for anything still unsettled at the deadline, using the SAME message
    // text as before so callers/tests keying on it are unaffected.
    const errors: HarnessError[] = [];
    if (this.launches.size > 0) {
      errors.push({
        category: "protocol",
        message: `quiesceChildren: ${this.launches.size} launch reservation(s) never settled`,
      });
    }
    const unsettled = this.inFlightCallbackCount();
    if (unsettled > 0) {
      errors.push({
        category: "protocol",
        message: `quiesceChildren: ${unsettled} callback/child-turn reservation(s) unsettled`,
      });
    }
    if (errors.length > 0) {
      this.poison(errors);
      return { kind: "incomplete", errors };
    }
    this.currentState = "closed";
    return { kind: "quiescent", epoch: this.epochValue };
  }

  /** Release the in-flight quiesce waiter once settlement (or poison) is reached while
   *  closing. Called at the end of the settle/cancel/register paths and (before the
   *  state flip) inside {@link poison}. A no-op unless a quiesce is actively waiting. */
  private maybeSignalQuiesce(): void {
    if (this.currentState !== "closing" || this.quiesceWaiter === undefined) return;
    if (this.poisoned || (this.launches.size === 0 && this.inFlightCallbackCount() === 0)) {
      const w = this.quiesceWaiter;
      this.quiesceWaiter = undefined;
      w();
    }
  }

  /** Bounded wait resolved either by {@link maybeSignalQuiesce} (all accepted work
   *  settled, or the epoch poisoned) or by the `deadlineMs` timer. The timer is
   *  `unref`'d so a pending quiesce never keeps the process alive. */
  private waitForSettlement(deadlineMs: number): Promise<void> {
    return new Promise<void>((resolve) => {
      let timer: ReturnType<typeof setTimeout> | undefined;
      this.quiesceWaiter = () => {
        if (timer) clearTimeout(timer);
        resolve();
      };
      if (deadlineMs > 0) {
        timer = setTimeout(() => {
          this.quiesceWaiter = undefined;
          resolve();
        }, deadlineMs);
        timer.unref?.();
      }
    });
  }

  /**
   * Aggregates `reap()` across EVERY registered root. Returns `observed_empty`
   * ONLY when `closedEpoch` matches the frozen epoch AND the registry is closed AND
   * every root reaped `{ ok: true }`. Any mismatch or any unreaped root returns
   * `{ kind: "incomplete", errors }` and poisons. One drained root is NOT a run.
   */
  async reapProcesses(deadlineMs: number, closedEpoch: number): Promise<ProcessReap> {
    if (this.currentState === "disposed") {
      return { kind: "incomplete", errors: [{ category: "protocol", message: "reapProcesses: registry disposed" }] };
    }
    if (this.currentState === "poisoned") {
      return { kind: "incomplete", errors: this.snapshotPoison() };
    }
    if (this.currentState !== "closed") {
      const err: HarnessError = {
        category: "protocol",
        message: "reapProcesses: registry not quiesced (must be closed first)",
      };
      this.poison(err);
      return { kind: "incomplete", errors: [err] };
    }
    if (closedEpoch !== this.epochValue) {
      // Stale/foreign epoch: the caller is holding a permit for a different epoch.
      const err: HarnessError = {
        category: "protocol",
        message: "reapProcesses: closedEpoch does not match the frozen local execution epoch",
      };
      this.poison(err);
      return { kind: "incomplete", errors: [err] };
    }
    const errors: HarnessError[] = [];
    // Reap every root; do not short-circuit, one incomplete root must not hide
    // another, and every root must be asked.
    const outcomes = await Promise.all(
      this.roots.map(async (rec) => ({ rec, outcome: await rec.root.reap(deadlineMs) })),
    );
    for (const { rec, outcome } of outcomes) {
      if (outcome.ok) {
        rec.reaped = true;
      } else {
        errors.push(outcome.error);
      }
    }
    if (errors.length > 0) {
      this.poison(errors);
      return { kind: "incomplete", errors };
    }
    return { kind: "observed_empty", evidence: "supervisor_echild", epoch: this.epochValue };
  }

  /**
   * Revokes admission, disposes every registered root, and drops handler/callback
   * references. Idempotent: a second call returns `{ kind: "disposed" }` without
   * re-disposing a root. A failing `dispose()` poisons and returns `incomplete`.
   */
  async disposeTools(deadlineMs: number): Promise<ToolDisposal> {
    if (this.currentState === "disposed") return { kind: "disposed" };
    const errors: HarnessError[] = [];
    for (const rec of this.roots) {
      if (rec.disposed) continue;
      try {
        await rec.root.dispose(deadlineMs);
        rec.disposed = true;
      } catch (e) {
        errors.push({
          category: "tool",
          message: `disposeTools: root dispose failed (${e instanceof Error ? e.name : "unknown"})`,
        });
      }
    }
    this.callbacks.clear();
    this.launches.clear();
    if (errors.length > 0) {
      this.poison(errors);
      return { kind: "incomplete", errors };
    }
    this.currentState = "disposed";
    return { kind: "disposed" };
  }

  private snapshotPoison(): readonly HarnessError[] {
    return this.poisonList.length > 0
      ? [...this.poisonList]
      : [{ category: "protocol", message: "registry poisoned" }];
  }
}
