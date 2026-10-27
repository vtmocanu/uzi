import { randomUUID } from "node:crypto";
import { normalizeCodexErrorInfo } from "./terminal-normalize.js";
import type { CodexNotification } from "./transport.js";

export interface AdmissionSignal {
  claim_generation: number;
  attempt_id: string;
  class: "rate_limit" | "overload";
  provider_thread_id?: string;
  provider_turn_id?: string;
}

interface Clock {
  now(): number;
  timer(fn: () => void, ms: number): () => void;
}
const clock: Clock = {
  now: () => performance.now(),
  timer: (fn, ms) => {
    const timer = setTimeout(fn, ms);
    timer.unref?.();
    return () => clearTimeout(timer);
  },
};
const object = (v: unknown): Record<string, unknown> | undefined =>
  v !== null && typeof v === "object" && !Array.isArray(v) ? v as Record<string, unknown> : undefined;
const coordinate = (v: string): boolean =>
  v.length > 0 && Buffer.byteLength(v, "utf8") <= 128 && !/[\p{Cc}\p{Cf}\p{Cs}]/u.test(v);

/** One generation's telemetry lane. Queue capacity includes its active send.
 * Two immediate HTTP attempts per observation; each has a 3s cap and the first
 * observation starts an absolute monotonic 10s budget, including queue time. A failure never blocks siblings.
 * The transport must honor abort; the race also bounds local waiting on a lost ACK.
 */
export class CodexAdmissionReporter {
  private queue: { body: AdmissionSignal; epoch: number; deadline: number }[] = [];
  private active?: AbortController;
  private draining = false;
  private epoch = 0;
  private closed = false;

  constructor(
    private readonly generation: number,
    private readonly enabled: () => boolean,
    private readonly send: (body: AdmissionSignal, signal: AbortSignal) => Promise<number>,
    private readonly time: Clock = clock,
  ) {}

  seal(close = false): void {
    this.epoch++;
    this.queue = [];
    if (close) this.active?.abort();
    this.closed ||= close;
  }

  /** Called synchronously immediately before the actual model dispatch. */
  begin(threadId: string): AdmissionAttempt {
    const enabledAtDispatch = this.enabled();
    if (!enabledAtDispatch) this.seal();
    return new AdmissionAttempt(randomUUID(), threadId, (id, turnId, kind) => {
      if (!enabledAtDispatch || this.closed || !this.enabled() || !Number.isSafeInteger(this.generation) || this.generation <= 0) return;
      if (this.queue.length + Number(this.active !== undefined) >= 32) return;
      const body: AdmissionSignal = { claim_generation: this.generation, attempt_id: id, class: kind,
        provider_thread_id: threadId, provider_turn_id: turnId };
      this.queue.push({ body, epoch: this.epoch, deadline: this.time.now() + 10_000 });
      if (!this.draining) {
        this.draining = true;
        // The provider handler never enters or awaits network I/O.
        queueMicrotask(() => { void this.drain(); });
      }
    });
  }

  private async drain(): Promise<void> {
    try {
      while (this.queue.length && !this.closed) {
        const entry = this.queue.shift()!;
        const deadline = entry.deadline;
        for (let attempt = 0; attempt < 2; attempt++) {
          if (entry.epoch !== this.epoch || this.closed || !this.enabled()) break;
          const remaining = deadline - this.time.now();
          if (remaining <= 0) break;
          const controller = new AbortController();
          this.active = controller;
          const cancelled = new Promise<number>(resolve => {
            controller.signal.addEventListener("abort", () => resolve(0), { once: true });
          });
          const cancelTimer = this.time.timer(() => controller.abort(), Math.min(3_000, remaining));
          let status: number;
          try {
            status = await Promise.race([
              Promise.resolve().then(() =>
                controller.signal.aborted || entry.epoch !== this.epoch || this.closed || !this.enabled()
                  ? 0 : this.send(entry.body, controller.signal)).catch(() => 0),
              cancelled,
            ]);
          } finally {
            cancelTimer();
            this.active = undefined;
          }
          if (status === 404 || status === 401 || status === 403) { this.seal(true); break; }
          // Only a transport failure, request timeout, or transient server response retries.
          if (status !== 0 && status !== 408 && status !== 429 && status < 500) break;
        }
      }
    } finally {
      this.draining = false;
    }
  }
}

/** Separate from final-cause selection: retrying notifications and terminal errors
 * are independent structured evidence, deduplicated by local attempt and class.
 */
export class AdmissionAttempt {
  private turnId?: string;
  private readonly seen = new Set<AdmissionSignal["class"]>();
  constructor(
    readonly id: string,
    private readonly threadId: string,
    private readonly emit: (id: string, turnId: string, kind: AdmissionSignal["class"]) => void,
  ) {}
  bind(turnId: string): void { this.turnId = turnId; }
  observe(note: CodexNotification): void {
    if (!this.turnId || !coordinate(this.threadId) || !coordinate(this.turnId) ||
        (note.kind !== "codex_error" && note.kind !== "turn_completed") ||
        note.threadId !== this.threadId || note.turnId !== this.turnId) return;
    const params = object(note.params);
    const raw = note.kind === "codex_error" ? object(params?.error)?.codexErrorInfo
      : object(object(params?.turn)?.error)?.codexErrorInfo;
    const c = normalizeCodexErrorInfo(raw);
    const kind = c?.classification === "serverOverloaded" ? "overload"
      : c?.classification === "rateLimitExceeded" || c?.httpStatus === 429 ? "rate_limit" : undefined;
    if (!kind || this.seen.has(kind)) return;
    this.seen.add(kind);
    this.emit(this.id, this.turnId, kind);
  }
}
