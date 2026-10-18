import { performance } from "node:perf_hooks";
import type { WorkerClient } from "./client.js";
import type { Config } from "./config.js";
import type { Logger } from "./log.js";
import type { MemoryPolicy, WorkerMemoryPressureResult } from "./protocol.js";
import { WorkerMemoryCommands } from "./worker-memory-commands.js";
import { WorkerMemoryMonitor } from "./worker-memory-guard.js";
import { WorkerMemoryReader } from "./worker-memory-reader.js";

type Context = ReturnType<WorkerMemoryCommands["begin"]>;
type Sinks = Readonly<{
  feedback: (result: WorkerMemoryPressureResult, context: Context, signal: AbortSignal, remainingMs: number) => Promise<void>;
  /** Retain every outcome before bounded delivery or tool completion can be skipped. */
  prepareFeedback: (context: Context, result: WorkerMemoryPressureResult) => void;
  preserve: (context: Context, signal: AbortSignal, remainingMs: number) => Promise<void>;
  /** Must synchronously retain this flight's clone/session intent and revoke admission. */
  preparePreservation: (context: Context, result: WorkerMemoryPressureResult) => void;
  preservationBudgetMs: number;
}>;

/** One worker-owned reader, registry and sampler. Adapters must bind real sinks
 * before enrolling commands; sinks belong to the captured context, never a run-ID lookup.
 * Claude has no enrollment adapter. No command is enrolled by constructing this runtime.
 */
export class WorkerMemoryRuntime {
  readonly commands: WorkerMemoryCommands;
  readonly configuredMemoryPolicy: Readonly<MemoryPolicy>;
  private readonly sinks = new WeakMap<Context, Sinks>();
  private readonly monitor: WorkerMemoryMonitor;
  private readonly invalidationListeners = new Set<() => void>();

  subscribeInvalidation(callback: () => void): () => void {
    this.invalidationListeners.add(callback);
    return () => { this.invalidationListeners.delete(callback); };
  }

  private readonly receipts = new Set<Readonly<{ context: Context; kind: "feedback" | "preserve" | "report"; completion: Promise<void> }>>();

  /** Unsettled owner operations, including callbacks that ignored deadline/abort.
   * The bound runner must reconcile these before releasing its clone/session.
   */
  get sinkReceipts() { return Object.freeze([...this.receipts]); }

  private ownedSink<T>(context: Context, kind: "feedback" | "preserve" | "report", work: () => Promise<T>): Promise<T> {
    let producer: Promise<T>;
    try { producer = work(); }
    catch (error) { producer = Promise.reject(error); }
    const completion = producer.then(() => {});
    const receipt = Object.freeze({ context, kind, completion });
    this.receipts.add(receipt);
    // Exactly one completion observer per invocation; no retry or new operation.
    void completion.then(() => this.receipts.delete(receipt), () => this.receipts.delete(receipt));
    return producer;
  }

  constructor(
    settings: Extract<NonNullable<Config["memoryGuard"]>, { enabled: true }>,
    private readonly client: Pick<WorkerClient, "revokeMemoryAuthority" | "memoryIncarnation" | "subscribeMemoryInvalidation" | "reserveMemoryIntervention" | "reportMemoryInterventionOutcome">,
    log: Pick<Logger, "error">,
    options: { reader?: Pick<WorkerMemoryReader, "sample">; now?: () => number; secrets?: readonly string[] } = {},
  ) {
    if (!Number.isSafeInteger(settings.maxInterventions) || settings.maxInterventions < 1
      || settings.maxInterventions > 9999) throw new Error("invalid memory guard settings");
    this.configuredMemoryPolicy = Object.freeze({ version: 1, max_interventions: settings.maxInterventions });
    const reader = options.reader ?? new WorkerMemoryReader();
    this.commands = new WorkerMemoryCommands(options.secrets);
    const sink = (context: Context): Sinks => {
      const bound = this.sinks.get(context);
      if (!bound) {
        log.error("memory adapter has no captured-context sinks");
        throw new Error("memory adapter has no captured-context sinks");
      }
      return bound;
    };
    this.monitor = new WorkerMemoryMonitor(settings, {
      sample: () => reader.sample(),
      now: options.now ?? (() => performance.now()),
      commands: this.commands,
      incarnation: () => client.memoryIncarnation,
      subscribeInvalidation: (callback) => client.subscribeMemoryInvalidation(() => {
        callback();
        for (const listener of this.invalidationListeners) listener();
      }),
      reserve: (request, signal, ms) => client.reserveMemoryIntervention(request, signal, ms),
      report: (request, signal, ms, context) => this.ownedSink(context, "report",
        () => client.reportMemoryInterventionOutcome(request, signal, ms)),
      feedback: (result, context, signal, ms) => {
        const bound = sink(context);
        return this.ownedSink(context, "feedback", () => bound.feedback(result, context, signal, ms));
      },
      preserve: (context, signal, ms) => {
        const bound = sink(context);
        return this.ownedSink(context, "preserve", () => bound.preserve(context, signal, ms));
      },
      prepareFeedback: (context, result) => sink(context).prepareFeedback(context, result),
      preparePreservation: (context, result) => sink(context).preparePreservation(context, result),
      preservationBudgetMs: (context) => sink(context).preservationBudgetMs,
    });
  }

  /** M3/M5 seam: callbacks must use this flight's real tool delivery and batcher/
   * preservation path. Historical feedback remains attached to this exact context.
   */
  begin(input: Parameters<WorkerMemoryCommands["begin"]>[0], sinks: Sinks): Context {
    if (typeof sinks?.feedback !== "function" || typeof sinks?.preserve !== "function"
      || typeof sinks?.prepareFeedback !== "function"
      || typeof sinks?.preparePreservation !== "function"
      || !Number.isSafeInteger(sinks?.preservationBudgetMs) || sinks.preservationBudgetMs < 1
      || sinks.preservationBudgetMs > 2147483647) {
      throw new Error("memory adapter requires feedback, synchronous preservation preparation and owner preservation budget");
    }
    const context = this.commands.begin(input);
    this.sinks.set(context, Object.freeze({ ...sinks }));
    return context;
  }

  run(): Promise<void> { return this.monitor.run(); }
  // Worker invokes stop before awaiting any runner cancellation. Monitor.stop
  // invalidates the registry synchronously, then joins its bounded work. Unsettled
  // sink receipts remain owned here after stop; abort is advisory, not settlement.
  // RunRunner reconciles these captured-flight receipts before clone/session release.
  stop(): Promise<void> {
    this.client.revokeMemoryAuthority();
    return this.monitor.stop();
  }
}
