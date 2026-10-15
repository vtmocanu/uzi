import type { WorkerMemoryPressureResult } from "../../src/protocol.js";
import type { CalibrationBinding, CalibrationPlan } from "./worker-memory-calibration.js";

export type CommandIdentity = Readonly<{ pid: number; startIdentity: string }>;
type Enrollment = Pick<WorkerMemoryPressureResult,
  "run_id" | "worker_id" | "claim_generation" | "memory_episode" | "attempt_id" | "command_id" | "tool_id" | "command">;
export type CommandStep = Readonly<{ kind: "completed" } | { kind: "cancelled"; feedback: WorkerMemoryPressureResult }>;
export type EnrolledCalibrationProducer = Readonly<{
  treeId: string; identity: CommandIdentity; enrollment: Enrollment;
  step: (plan: CalibrationPlan, signal: AbortSignal) => Promise<CommandStep>;
  /** Reject on unexpected failure OR exit; expected monitor drain is separate. */
  unexpectedFailure: Promise<never>;
  proveLive: (saved: CommandIdentity) => Promise<CommandIdentity>;
  /** Exact owned handle only; each stop must join the command tree. */
  stop: () => Promise<void>;
  forceStop: () => Promise<void>;
}>;
export type CalibrationIntervention = Readonly<{
  binding: CalibrationBinding; selected: 0 | 1;
  feedback: Promise<WorkerMemoryPressureResult>;
  confirmedDrain: Promise<{ binding: CalibrationBinding; identity: CommandIdentity; confirmed: true }>;
}>;
export type CommandCalibrationPorts = Readonly<{
  producers: readonly [EnrolledCalibrationProducer, EnrolledCalibrationProducer];
  intervention: Promise<CalibrationIntervention>;
  signal?: AbortSignal;
}>;
const bindingKeys = ["reportId", "legId", "interventionId", "treeId", "toolId", "reservationId"] as const;
function matches(a: CalibrationBinding, b: CalibrationBinding): boolean {
  return !!a && !!b && bindingKeys.every((k) => typeof a[k] === "string" && !!a[k] && a[k] === b[k]);
}
function identityMatches(a: CommandIdentity, b: CommandIdentity): boolean {
  return !!b && a.pid === b.pid && a.startIdentity === b.startIdentity;
}
async function bounded<T>(work: Promise<T>, ms: number, message: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([work, new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new Error(message)), Math.max(0, ms));
    })]);
  } finally { clearTimeout(timer); }
}

/** Test-only DRIVER, never enrollment or cancellation authority. Future real
 * adapters must use the actual reader, monitor, broker enrollment and feedback.
 * Two already enrolled trees; at most plan.rounds steps each, <=30s load.
 * Cleanup has one 30s deadline: <=5s sibling proof, <=15s graceful stop,
 * then exact forced joins within the remaining deadline.
 * Independent stop failures never prevent the sibling stop/force attempt.
 */
export async function commandTreeCalibration(plan: CalibrationPlan, ports: CommandCalibrationPorts) {
  const pair = ports.producers;
  if (pair.length !== 2 || pair[0] === pair[1] || pair[0].treeId === pair[1].treeId
    || pair[0].identity.pid === pair[1].identity.pid
    || pair.some((p) => !Number.isSafeInteger(p.identity.pid) || p.identity.pid <= 0 || !p.identity.startIdentity)
    || !Number.isInteger(plan.windowMs) || plan.windowMs < 1 || plan.windowMs > 30000
    || plan.chunkBytes !== 1024 * 1024 || ![1024 ** 3, 2 * 1024 ** 3].includes(plan.limitBytes)
    || plan.rounds !== (plan.limitBytes === 1024 ** 3 ? 16 : 32)
    || plan.allocationBytes !== 8 * plan.rounds * plan.chunkBytes || plan.overheadBytes !== 64 * 1024 ** 2
    || plan.ceilingBytes !== plan.limitBytes / 2 || !Number.isSafeInteger(plan.baselineBytes) || plan.baselineBytes < 0
    || plan.baselineBytes + plan.allocationBytes + plan.overheadBytes > plan.ceilingBytes) {
    throw new Error("invalid enrolled pair or bounded plan");
  }
  const saved = pair.map((p) => Object.freeze({ ...p.identity }));
  const controller = new AbortController();
  let selected: CalibrationIntervention | undefined, halted = false;
  let failedIndex: number | undefined;
  const steps: Promise<CommandStep>[] = [];
  let current: Promise<CommandStep>[] = [];
  const failures = pair.map((p, index) => p.unexpectedFailure.catch((error: unknown) => {
    halted = true; failedIndex = index; throw error;
  }));
  const selectedEvent = ports.intervention.then((event) => {
    halted = true;
    selected = event;
    if (![0, 1].includes(event.selected) || !matches(event.binding, event.binding)
      || event.binding.treeId !== pair[event.selected].treeId
      || event.binding.toolId !== pair[event.selected].enrollment.tool_id) throw new Error("mismatched intervention");
    return event;
  });
  // Attach rejection handlers before any step or adapter callback runs.
  void selectedEvent.catch(() => {});
  for (const failure of failures) void failure.catch(() => {});
  let rejectCaller!: (error: Error) => void;
  const cancelled = new Promise<never>((_, reject) => { rejectCaller = reject; });
  void cancelled.catch(() => {});
  const onAbort = () => { halted = true; rejectCaller(new Error("calibration caller cancelled")); };
  ports.signal?.addEventListener("abort", onAbort, { once: true });
  let failure: unknown, result: { intervention: CalibrationBinding; sibling: CommandIdentity } | undefined;
  try {
    await bounded((async () => {
      if (ports.signal?.aborted) throw new Error("calibration caller cancelled");
      for (let round = 0; round < plan.rounds && !halted; round++) {
        current = pair.map((p, index) => {
          const step = Promise.resolve().then(() => {
            if (halted) throw new Error("step scheduling halted");
            return p.step(plan, controller.signal);
          }).then((value) => {
            if (!controller.signal.aborted && value.kind === "cancelled" && (!selected || selected.selected !== index)) {
              throw new Error("unexpected producer cancellation");
            }
            return value;
          }).catch((error: unknown) => {
            if (!controller.signal.aborted) { halted = true; failedIndex = index; }
            throw error;
          });
          steps.push(step);
          return step;
        });
        const completed = Promise.all(current).then((values) => {
          if (!selected && values.some((value) => value.kind === "cancelled")) throw new Error("unexpected producer cancellation");
          return null;
        });
        const event = await Promise.race([completed, selectedEvent, cancelled, ...failures]);
        if (event) break;
      }
      if (!selected) throw new Error("no selected intervention");
      const event = selected, producer = pair[event.selected];
      const selectedStep = current[event.selected];
      if (!selectedStep) throw new Error("selected producer has no in-flight step");
      const evidence = Promise.all([event.feedback, event.confirmedDrain, selectedStep]);
      const [feedback, drain, step] = await Promise.race([evidence, cancelled, ...failures]);
      if (Object.entries(producer.enrollment).some(([key, value]) => feedback[key as keyof WorkerMemoryPressureResult] !== value)
        || feedback.intervention_id !== event.binding.interventionId || feedback.code !== "worker_memory_pressure"
        || feedback.outcome !== "confirmed_drained" || feedback.cancellation_confirmed !== true || feedback.retry_blocked !== true
        || feedback.limit_bytes !== plan.limitBytes
        || ![feedback.observed_total_bytes, feedback.tree_rss_bytes_approx].every((n) => Number.isSafeInteger(n) && n >= 0)
        || feedback.guidance !== "Use smaller or serial commands after recovery."
        || !matches(event.binding, drain.binding) || drain.confirmed !== true
        || !identityMatches(saved[event.selected]!, drain.identity)
        || step.kind !== "cancelled"
        || Object.keys(feedback).some((key) => step.feedback[key as keyof WorkerMemoryPressureResult] !== feedback[key as keyof WorkerMemoryPressureResult])) {
        throw new Error("mismatched actual pressure feedback or unconfirmed drain");
      }
    })(), plan.windowMs, "calibration load window expired");
  } catch (error) { halted = true; failure = error; }
  finally { ports.signal?.removeEventListener("abort", onAbort); }

  // Prove the saved sibling identity before global cleanup. Proof failure
  // invalidates acceptance but still requires stopping both owned handles.
  const cleanupDeadline = Date.now() + 30000;
  const siblingIndex = selected ? 1 - selected.selected : failedIndex === 1 ? 0 : 1;
  const sibling = saved[siblingIndex]!;
  try {
    const live = await bounded(pair[siblingIndex]!.proveLive(sibling), 5000, "sibling proof timeout");
    if (!identityMatches(sibling, live)) throw new Error("sibling identity not live");
    if (selected) result = { intervention: selected.binding, sibling };
  } catch (error) { failure = new AggregateError([...(failure ? [failure] : []), error], "sibling proof failed; no next leg"); }
  controller.abort();
  const cleanup = async () => {
    const errors: unknown[] = [];
    const stops = pair.map((p) => Promise.resolve().then(() => p.stop()));
    try {
      const gracefulJoins = Promise.allSettled(stops).then(async (settled) => {
        await Promise.allSettled(steps);
        return settled;
      });
      const settled = await bounded(gracefulJoins, 15000, "graceful cleanup timeout");
      errors.push(...settled.flatMap((r) => r.status === "rejected" ? [r.reason] : []));
    } catch (error) { errors.push(error); }
    if (errors.length) {
      const forced = await Promise.allSettled(pair.map((p) => Promise.resolve().then(() => p.forceStop())));
      errors.push(...forced.flatMap((r) => r.status === "rejected" ? [r.reason] : []));
    }
    const finalStops = await Promise.allSettled(stops);
    errors.push(...finalStops.flatMap((r) => r.status === "rejected" ? [r.reason] : []));
    await Promise.allSettled(steps);
    if (errors.length) throw new AggregateError(errors, "calibration cleanup failed");
  };
  try { await bounded(cleanup(), cleanupDeadline - Date.now(), "calibration forced cleanup/join timeout"); }
  catch (error) { throw new AggregateError([...(failure ? [failure] : []), error], "calibration cleanup failed; no next leg"); }
  // unexpected failures during evidence/proof also fail acceptance.
  if (failedIndex !== undefined && !failure) failure = new Error("unexpected producer failure or exit");
  if (ports.signal?.aborted && !failure) failure = new Error("calibration caller cancelled");
  if (failure) throw failure;
  if (!result) throw new Error("missing intervention result");
  return result;
}
