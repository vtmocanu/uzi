import type { Logger } from "./log.js";
import {
  WORKER_API_PREFIX,
  TERMINAL_STATES,
  type ClaimResponse,
  type HeartbeatRequest,
  type MessagesRequest,
  type OutgoingMessage,
  type RegisterRequest,
  type RegisterResponse,
  type StateRequest,
  type UserInput,
  type InputsResponse,
} from "./protocol.js";

/** Error carrying the server's HTTP status + (truncated) body for retry logic. */
export class RequestError extends Error {
  constructor(
    readonly method: string,
    readonly path: string,
    readonly status: number,
    readonly body: string,
  ) {
    super(`${method} ${path} returned ${status}: ${body}`);
    this.name = "RequestError";
  }
}

const sleepReal = (ms: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, ms));

/**
 * Backoff for the "must reach the server" terminal callback (reportState of a
 * completed/failed run). Mirrors multica's schedule: N sleeps → N+1 attempts.
 */
export const DEFAULT_TERMINAL_RETRY_SCHEDULE = [1_000, 2_000, 4_000, 8_000, 16_000];

export interface ClientOptions {
  sleep?: (ms: number) => Promise<void>;
  terminalRetrySchedule?: number[];
  httpTimeoutMs?: number;
}

/** Transport for the worker→API control plane (PRD §Worker protocol). */
export class WorkerClient {
  private readonly sleep: (ms: number) => Promise<void>;
  private readonly terminalRetrySchedule: number[];
  private readonly httpTimeoutMs: number;

  constructor(
    private readonly baseUrl: string,
    private readonly token: string,
    private readonly version: string,
    private readonly log: Logger,
    opts: ClientOptions = {},
  ) {
    this.sleep = opts.sleep ?? sleepReal;
    this.terminalRetrySchedule = opts.terminalRetrySchedule ?? DEFAULT_TERMINAL_RETRY_SCHEDULE;
    this.httpTimeoutMs = opts.httpTimeoutMs ?? 30_000;
  }

  async register(name: string): Promise<RegisterResponse> {
    const body: RegisterRequest = { name, version: this.version };
    return (await this.postJSON(`${WORKER_API_PREFIX}/register`, body)) as RegisterResponse;
  }

  async heartbeat(): Promise<void> {
    const body: HeartbeatRequest = { version: this.version };
    await this.postJSON(`${WORKER_API_PREFIX}/heartbeat`, body);
  }

  /** Claim the oldest queued run for this worker's user. Returns null on 204. */
  async claimRun(): Promise<ClaimResponse | null> {
    const res = await this.fetchRaw("POST", `${WORKER_API_PREFIX}/runs/claim`, {});
    if (res.status === 204) return null;
    if (res.status >= 400) throw await this.toError("POST", `${WORKER_API_PREFIX}/runs/claim`, res);
    return (await res.json()) as ClaimResponse;
  }

  async postMessages(runId: string, messages: OutgoingMessage[]): Promise<void> {
    if (messages.length === 0) return;
    const body: MessagesRequest = { messages };
    await this.postJSON(`${WORKER_API_PREFIX}/runs/${runId}/messages`, body);
  }

  /**
   * Report a run state. Terminal reports (completed/failed) are retried with
   * bounded backoff and treat an "already terminal" server response as success
   * (multica) so a lost ack / duplicate replay is safe. Non-terminal reports
   * are single-shot.
   */
  async reportState(runId: string, body: StateRequest): Promise<void> {
    const path = `${WORKER_API_PREFIX}/runs/${runId}/state`;
    const terminal = TERMINAL_STATES.has(body.status);
    if (!terminal) {
      await this.postJSON(path, body);
      return;
    }

    let lastErr: unknown;
    for (let attempt = 0; ; attempt++) {
      try {
        await this.postJSON(path, body);
        return;
      } catch (err) {
        if (this.isAlreadyTerminal(err)) {
          this.log.info("state already terminal server-side, treating as success", { run_id: runId, status: body.status });
          return;
        }
        lastErr = err;
        if (!isTransient(err) || attempt >= this.terminalRetrySchedule.length) throw err;
        const delay = this.terminalRetrySchedule[attempt] ?? 0;
        this.log.warn("state report failed, retrying", { run_id: runId, status: body.status, attempt, delay_ms: delay });
        await this.sleep(delay);
      }
    }
    // Unreachable: the loop either returns or throws.
    void lastErr;
  }

  async getInputs(runId: string): Promise<UserInput[]> {
    const res = (await this.getJSON(`${WORKER_API_PREFIX}/runs/${runId}/inputs`)) as InputsResponse;
    return res.inputs ?? [];
  }

  /** A 409, or a 4xx body mentioning "terminal", means the server already
   *  finalized the run — an idempotent success for a terminal report. */
  private isAlreadyTerminal(err: unknown): boolean {
    if (!(err instanceof RequestError)) return false;
    if (err.status === 409) return true;
    return err.status < 500 && /terminal/i.test(err.body);
  }

  private async postJSON(path: string, body: unknown): Promise<unknown> {
    const res = await this.fetchRaw("POST", path, body);
    if (res.status >= 400) throw await this.toError("POST", path, res);
    if (res.status === 204) return undefined;
    const text = await res.text();
    return text ? JSON.parse(text) : undefined;
  }

  private async getJSON(path: string): Promise<unknown> {
    const res = await this.fetchRaw("GET", path, undefined);
    if (res.status >= 400) throw await this.toError("GET", path, res);
    const text = await res.text();
    return text ? JSON.parse(text) : undefined;
  }

  private async fetchRaw(method: "GET" | "POST", path: string, body: unknown): Promise<Response> {
    const headers: Record<string, string> = {
      Authorization: `Bearer ${this.token}`,
      "X-Client-Version": this.version,
    };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    return fetch(this.baseUrl + path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(this.httpTimeoutMs),
    });
  }

  private async toError(method: string, path: string, res: Response): Promise<RequestError> {
    let text = "";
    try {
      text = (await res.text()).slice(0, 4096).trim();
    } catch {
      // ignore body read failures — the status is the signal that matters.
    }
    return new RequestError(method, path, res.status, text);
  }
}

/** Retryable: transport failures, 5xx, and 408/429; permanent otherwise. */
export function isTransient(err: unknown): boolean {
  if (err instanceof RequestError) {
    return err.status >= 500 || err.status === 408 || err.status === 429;
  }
  // Network error / timeout (AbortError) / non-HTTP failure.
  return true;
}
