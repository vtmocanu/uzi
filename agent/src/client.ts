import type { Logger } from "./log.js";
import {
  WORKER_API_PREFIX,
  type ChatClaimResponse,
  type ClaimResponse,
  type CreateProposalRequest,
  type HeartbeatRequest,
  type MessagesRequest,
  type OutgoingMessage,
  type RegisterRequest,
  type RegisterResponse,
  type StateRequest,
  type UserInput,
  type InputsResponse,
  type WorkerProposal,
  type WorkerRunDetail,
  type WorkerRunListItem,
  type WorkerRunMessage,
  type JudgeTraceResponse,
  type ReviewRequest,
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

  async register(name: string, template?: string, maxConcurrentRuns?: number): Promise<RegisterResponse> {
    const body: RegisterRequest = { name, version: this.version };
    // Only send the field when known: an image without ENV WORKER_TEMPLATE reports
    // no template, and the server stores NULL (PRD #18). The server's decoder
    // rejects unknown fields but accepts an absent optional one.
    if (template) body.template = template;
    // Advertise the RUN-lane concurrency cap (PRD #42 Decision 3): observability the
    // server records (and clamps to [1,256]) and renders as "N/M runs", never
    // enforced. Sent whenever the caller knows it (the worker always does — default
    // 1); M3a's register handler accepts it and a pre-#42 server ignores it. Distinct
    // from the chat lane's WORKER_CHAT_SESSIONS — this bounds issue/ci_fix runs only.
    if (typeof maxConcurrentRuns === "number") body.max_concurrent_runs = maxConcurrentRuns;
    return (await this.postJSON(`${WORKER_API_PREFIX}/register`, body)) as RegisterResponse;
  }

  async heartbeat(): Promise<void> {
    const body: HeartbeatRequest = { version: this.version };
    await this.postJSON(`${WORKER_API_PREFIX}/heartbeat`, body);
  }

  /** Claim the oldest queued run for this worker's user (the RUN lane — no lane
   *  param, back-compat with older servers). Returns null on 204. */
  async claimRun(): Promise<ClaimResponse | null> {
    const res = await this.fetchRaw("POST", `${WORKER_API_PREFIX}/runs/claim`, {});
    if (res.status === 204) return null;
    if (res.status >= 400) throw await this.toError("POST", `${WORKER_API_PREFIX}/runs/claim`, res);
    return (await res.json()) as ClaimResponse;
  }

  /** Claim the oldest queued CHAT run for this worker's user (the disjoint chat
   *  lane, PRD #39 Decision 4). Returns null on 204 (chat queue idle). Runs as an
   *  independent loop concurrently with claimRun. */
  async claimChat(): Promise<ChatClaimResponse | null> {
    const path = `${WORKER_API_PREFIX}/runs/claim?lane=chat`;
    const res = await this.fetchRaw("POST", path, {});
    if (res.status === 204) return null;
    if (res.status >= 400) throw await this.toError("POST", path, res);
    return (await res.json()) as ChatClaimResponse;
  }

  async postMessages(runId: string, messages: OutgoingMessage[]): Promise<void> {
    if (messages.length === 0) return;
    const body: MessagesRequest = { messages };
    await this.postJSON(`${WORKER_API_PREFIX}/runs/${runId}/messages`, body);
  }

  /**
   * Report a run state. Every report — terminal (completed/failed) and
   * non-terminal (running/awaiting_approval) alike — is retried with bounded
   * backoff on transient failures: the server transition is idempotent, so a
   * retried non-terminal report is safe (PRD: "/state is retried with backoff"),
   * and a terminal report *must* reach the server. An "already terminal" server
   * response (409, or a 4xx body mentioning terminal) is treated as success
   * (multica) so a lost ack / duplicate replay is safe. A 4xx is otherwise fatal.
   */
  async reportState(runId: string, body: StateRequest): Promise<void> {
    const path = `${WORKER_API_PREFIX}/runs/${runId}/state`;
    for (let attempt = 0; ; attempt++) {
      try {
        await this.postJSON(path, body);
        return;
      } catch (err) {
        if (this.isAlreadyTerminal(err)) {
          this.log.info("state already terminal server-side, treating as success", { run_id: runId, status: body.status });
          return;
        }
        if (!isTransient(err) || attempt >= this.terminalRetrySchedule.length) throw err;
        const delay = this.terminalRetrySchedule[attempt] ?? 0;
        this.log.warn("state report failed, retrying", { run_id: runId, status: body.status, attempt, delay_ms: delay });
        await this.sleep(delay);
      }
    }
  }

  async getInputs(runId: string): Promise<UserInput[]> {
    const res = (await this.getJSON(`${WORKER_API_PREFIX}/runs/${runId}/inputs`)) as InputsResponse;
    return res.inputs ?? [];
  }

  // ── Chat agent read surface (PRD #39 M3) ───────────────────────────────────
  // User-scoped by the worker's join token server-side; a foreign run id is 404.
  // Returned text is UNTRUSTED — the uzi-tools MCP server wraps it as evidence.

  /** List the worker user's runs, newest first (GET /worker/chat/runs?limit). */
  async listChatRuns(limit?: number): Promise<WorkerRunListItem[]> {
    const q = typeof limit === "number" ? `?limit=${encodeURIComponent(String(limit))}` : "";
    const res = (await this.getJSON(`${WORKER_API_PREFIX}/chat/runs${q}`)) as { runs?: WorkerRunListItem[] };
    return res.runs ?? [];
  }

  /** One run's detail (GET /worker/chat/runs/:id). Throws RequestError(404) for a
   *  run that is not the worker user's. */
  async getChatRun(runId: string): Promise<WorkerRunDetail> {
    const res = (await this.getJSON(`${WORKER_API_PREFIX}/chat/runs/${encodeURIComponent(runId)}`)) as { run: WorkerRunDetail };
    return res.run;
  }

  /** A bounded page of a run's messages (GET /worker/chat/runs/:id/messages). */
  async getChatRunMessages(runId: string, after?: number, limit?: number): Promise<WorkerRunMessage[]> {
    const params = new URLSearchParams();
    if (typeof after === "number") params.set("after", String(after));
    if (typeof limit === "number") params.set("limit", String(limit));
    const q = params.toString() ? `?${params.toString()}` : "";
    const res = (await this.getJSON(`${WORKER_API_PREFIX}/chat/runs/${encodeURIComponent(runId)}/messages${q}`)) as {
      messages?: WorkerRunMessage[];
    };
    return res.messages ?? [];
  }

  /** A page of the reviewed run's trace for a JUDGE run (GET /worker/runs/:id/trace).
   *  `targetRunId` is the reviewed run; authz is judge-run-scoped server-side (PRD
   *  #46 Decision 3). Messages are UNTRUSTED tool output. */
  async getTrace(targetRunId: string, after?: number, limit?: number): Promise<JudgeTraceResponse> {
    const params = new URLSearchParams();
    if (typeof after === "number") params.set("after", String(after));
    if (typeof limit === "number") params.set("limit", String(limit));
    const q = params.toString() ? `?${params.toString()}` : "";
    return (await this.getJSON(
      `${WORKER_API_PREFIX}/runs/${encodeURIComponent(targetRunId)}/trace${q}`,
    )) as JudgeTraceResponse;
  }

  /** Post the judge's verdict + recommendations (POST /worker/runs/:id/review). The
   *  server validates + scrubs; a bad enum is a 400. `targetRunId` is the reviewed run. */
  async postReview(targetRunId: string, review: ReviewRequest): Promise<void> {
    await this.postJSON(`${WORKER_API_PREFIX}/runs/${encodeURIComponent(targetRunId)}/review`, review);
  }

  /** Create a PENDING issue proposal on a chat run (POST /worker/runs/:id/proposals).
   *  NEVER writes the forge — the browser confirm does. Returns the created proposal. */
  async createProposal(runId: string, body: CreateProposalRequest): Promise<WorkerProposal> {
    const res = (await this.postJSON(`${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/proposals`, body)) as {
      proposal: WorkerProposal;
    };
    return res.proposal;
  }

  /** A 409, or a 4xx body mentioning "terminal", means the server already
   *  finalized the run — an idempotent success for any state report. */
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
