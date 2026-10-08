import { performance } from "node:perf_hooks";
import type { WorkerClient } from "./client.js";
import type { Config } from "./config.js";
import type { Logger } from "./log.js";
import type { WorkerMemoryPressureResult } from "./protocol.js";
import { WorkerMemoryCommands } from "./worker-memory-commands.js";
import { WorkerMemoryMonitor } from "./worker-memory-guard.js";
import { WorkerMemoryReader } from "./worker-memory-reader.js";

type Context = ReturnType<WorkerMemoryCommands["begin"]>;
type Sinks = Readonly<{
  feedback: (result: WorkerMemoryPressureResult, context: Context) => Promise<void>;
  preserve: (context: Context) => Promise<void>;
}>;

/** One worker-owned reader, registry and sampler. Adapters must bind real sinks
 * before enrolling commands; sinks belong to the captured context, never a run-ID lookup.
 * Claude has no enrollment adapter. No command is enrolled by constructing this runtime.
 */
export class WorkerMemoryRuntime {
  readonly commands: WorkerMemoryCommands;
  private readonly sinks = new WeakMap<Context, Sinks>();
  private readonly monitor: WorkerMemoryMonitor;

  constructor(
    settings: Extract<NonNullable<Config["memoryGuard"]>, { enabled: true }>,
    private readonly client: Pick<WorkerClient, "revokeMemoryAuthority" | "memoryIncarnation" | "subscribeMemoryInvalidation" | "reserveMemoryIntervention" | "reportMemoryInterventionOutcome">,
    log: Pick<Logger, "error">,
    options: { reader?: Pick<WorkerMemoryReader, "sample">; now?: () => number; secrets?: readonly string[] } = {},
  ) {
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
      subscribeInvalidation: (callback) => client.subscribeMemoryInvalidation(callback),
      reserve: (request, signal, ms) => client.reserveMemoryIntervention(request, signal, ms),
      report: (request, signal, ms) => client.reportMemoryInterventionOutcome(request, signal, ms),
      feedback: (result, context) => sink(context).feedback(result, context),
      preserve: (context) => sink(context).preserve(context),
    });
  }

  /** M3/M5 seam: callbacks must use this flight's real tool delivery and batcher/
   * preservation path. Historical feedback remains attached to this exact context.
   */
  begin(input: Parameters<WorkerMemoryCommands["begin"]>[0], sinks: Sinks): Context {
    if (typeof sinks?.feedback !== "function" || typeof sinks?.preserve !== "function") {
      throw new Error("memory adapter requires feedback and preservation");
    }
    const context = this.commands.begin(input);
    this.sinks.set(context, Object.freeze({ ...sinks }));
    return context;
  }

  run(): Promise<void> { return this.monitor.run(); }
  // Worker invokes stop before awaiting any runner cancellation. Monitor.stop
  // invalidates the registry synchronously, then joins its bounded work.
  stop(): Promise<void> {
    this.client.revokeMemoryAuthority();
    return this.monitor.stop();
  }
}
