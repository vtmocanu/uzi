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
  type StateAck,
  type StateRequest,
  type UserInput,
  type InputsResponse,
  type WorkerProposal,
  type WorkerRunDetail,
  type WorkerRunListItem,
  type WorkerRunMessage,
  type JudgeTraceResponse,
  type ReviewRequest,
  type WorkerStats,
  type SaveMemoryRequest,
  type MemoryEntry,
  type MemoryListResponse,
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

  async register(
    name: string,
    template?: string,
    maxConcurrentRuns?: number,
    capabilities?: string[],
  ): Promise<RegisterResponse> {
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
    // Self-reported REACHABLE capabilities (PRD #83 Q1). Only send when non-empty
    // (mirrors `template`): a worker with no daemon wired omits it entirely, so the
    // register wire stays byte-identical to today. The api declares-and-ignores it in
    // M1 (accept-and-ignore); #84 owns storage + the claim-time match predicate.
    if (capabilities?.length) body.capabilities = capabilities;
    return (await this.postJSON(`${WORKER_API_PREFIX}/register`, body)) as RegisterResponse;
  }

  async heartbeat(stats?: WorkerStats): Promise<void> {
    const body: HeartbeatRequest = { version: this.version };
    // Only attach stats when the collector produced a sample (PRD #49): an absent
    // field is the same wire shape as today, so a pre-#49 server ignores the extra
    // bytes and a collector-less tick is indistinguishable from an old worker.
    if (stats) body.stats = stats;
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
   * Report a run state and return what the server made of it. Every report —
   * terminal (completed/failed) and non-terminal (running/awaiting_approval/
   * limit_wait) alike — is retried with bounded backoff on transient failures: the
   * server transition is idempotent, so a retried non-terminal report is safe
   * (PRD: "/state is retried with backoff"), and a terminal report *must* reach the
   * server. An "already terminal" server response (409, or a 4xx body mentioning
   * terminal) does not throw (multica) so a lost ack / duplicate replay is safe. A
   * 4xx is otherwise fatal.
   *
   * The RESULT used to be discarded. It is now returned, because `limit_wait`
   * (PRD #35) is the first report whose consequence is that the caller SKIPS
   * filesystem cleanup — so "did this actually park?" stopped being rhetorical.
   * Read the warning on StateAck before branching on it: the answer is
   * `status === "limit_wait"`, never `applied`.
   *
   * 200 and 409 are both read for their body rather than routed through
   * postJSON/RequestError. That is deliberate: RequestError truncates bodies at
   * 4096 chars (toError), and a RunDTO carrying plan_md exceeds that comfortably,
   * so parsing a 409's status back out of the error text would work in tests and
   * fail on real runs.
   */
  async reportState(runId: string, body: StateRequest): Promise<StateAck> {
    const path = `${WORKER_API_PREFIX}/runs/${runId}/state`;
    for (let attempt = 0; ; attempt++) {
      try {
        const res = await this.fetchRaw("POST", path, body);
        // 409 = the server declined the transition and is telling us the run's real
        // status. Not an error, and not a retry: the server has moved on.
        if (res.status === 200 || res.status === 409) {
          const ack: StateAck = { applied: res.status === 200, status: await readRunStatus(res) };
          if (!ack.applied) {
            this.log.info("state report not applied server-side", {
              run_id: runId,
              reported: body.status,
              server_status: ack.status ?? "unknown",
            });
          }
          return ack;
        }
        if (res.status >= 400) throw await this.toError("POST", path, res);
        // A 2xx we do not model (e.g. 204 from an older server): the report landed,
        // but no status came back. Undefined reads as "not parked", which is safe.
        return { applied: true, status: undefined };
      } catch (err) {
        if (this.isAlreadyTerminal(err)) {
          // A 4xx whose TEXT says terminal. The status is not recoverable here, and
          // an absent status is the safe answer for every caller.
          this.log.info("state already terminal server-side, treating as success", { run_id: runId, status: body.status });
          return { applied: false, status: undefined };
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

  // ── Cross-run agent memory (PRD #90) ───────────────────────────────────────
  // Per-(user, repo), server-derived from the run claim. The worker NEVER sends
  // user/repo ids (its join token is not user-scoped). save_memory is the WRITE
  // seam the lead's in-process MCP tool calls; getMemory is the READ seam the
  // runner composes into the lead's plan prompt as inert, nonce-fenced context.

  /** Persist a cross-run learning for this run's (user, repo) (POST /worker/runs/:id/
   *  memory). Returns the stored entry. Errors are meaningful: 409 (repo-less run),
   *  400 (empty/oversize), 429 (per-run write cap) — the caller surfaces each as a
   *  non-fatal tool message. */
  async saveMemory(runId: string, body: SaveMemoryRequest): Promise<MemoryEntry> {
    return (await this.postJSON(`${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/memory`, body)) as MemoryEntry;
  }

  /** Fetch this run's (user, repo) memory, newest first (GET /worker/runs/:id/
   *  memory). Returned entries are UNTRUSTED — the runner wraps them in a nonce
   *  fence before they reach the lead. */
  async getMemory(runId: string): Promise<MemoryEntry[]> {
    const res = (await this.getJSON(`${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/memory`)) as MemoryListResponse;
    return res.memories ?? [];
  }

  /** A 4xx body mentioning "terminal": the server already finalized the run, so the
   *  report does not throw.
   *
   *  This used to say "a 409, or a 4xx body mentioning terminal … an idempotent
   *  success for any state report", and both halves are now false. reportState
   *  handles 409 inline (it needs the body, and a 409 never reaches here any more),
   *  and "success" is no longer the whole story: since PRD #35 a declined report is
   *  reported to the caller as `applied: false` rather than being flattened into the
   *  success path, because a caller that skips filesystem cleanup on the strength of
   *  a park must be able to tell that the park did not happen. */
  private isAlreadyTerminal(err: unknown): boolean {
    if (!(err instanceof RequestError)) return false;
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
    // `body` is SET CONDITIONALLY rather than passed as `body: undefined` (PRD #103
    // M3, oxlint unicorn/no-invalid-fetch-options, which flags a `body` key on a
    // call whose method can be "GET"). Behaviourally identical — fetch treats an
    // undefined body as no body — but the static shape now says so, and a real
    // GET-with-a-body would become visible instead of being masked by a key that is
    // always present.
    const init: RequestInit = {
      method,
      headers,
      signal: AbortSignal.timeout(this.httpTimeoutMs),
    };
    if (body !== undefined) init.body = JSON.stringify(body);
    return fetch(this.baseUrl + path, init);
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

/**
 * Pull `run.status` out of a /state response body (`{"run": <RunDTO>}`, the shape
 * both the 200 and the 409 path return).
 *
 * TOTAL by construction: every failure — an unreadable stream, malformed JSON, a
 * body with no run, a non-string status, an older server that sent nothing — yields
 * `undefined` rather than throwing. That is not defensiveness for its own sake: the
 * caller's rule is a POSITIVE test for one literal, so `undefined` means "not
 * parked", which is the safe answer to every one of those failures. A throw here
 * would instead surface as a state report that appears to have failed, and would be
 * retried against a server that already applied it.
 */
async function readRunStatus(res: Response): Promise<string | undefined> {
  try {
    const text = await res.text();
    if (!text) return undefined;
    const parsed = JSON.parse(text) as { run?: { status?: unknown } };
    const status = parsed?.run?.status;
    return typeof status === "string" ? status : undefined;
  } catch {
    return undefined;
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
