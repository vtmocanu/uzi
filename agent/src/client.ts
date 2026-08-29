import type { Readable } from "node:stream";
import type { Logger } from "./log.js";
import {
  WORKER_API_PREFIX,
  type PublishResponse,
  type ChatClaimResponse,
  type ClaimResponse,
  type CreateProposalRequest,
  type ReportFindingRequest,
  type HeartbeatRequest,
  type MessagesRequest,
  type OutgoingMessage,
  type RegisterRequest,
  type RegisterResponse,
  type StateAck,
  type StateRequest,
  type UserInput,
  type InputsResponse,
  type RunOwnershipResponse,
  type WorkerProposal,
  type WorkerRunDetail,
  type WorkerRunListItem,
  type WorkerRunMessage,
  type JudgeTraceResponse,
  type ReviewRequest,
  type TaskReviewRequest,
  type WorkerStats,
  type SaveMemoryRequest,
  type MemoryEntry,
  type MemoryListResponse,
  type IssueDTO,
  type IssueListDTO,
  type LabelEventListDTO,
  type MergeRequestDTO,
  type JobListDTO,
  type LatestPipelineDTO,
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
 * completed/failed run). The schedule is N sleeps → N+1 attempts.
 */
export const DEFAULT_TERMINAL_RETRY_SCHEDULE = [1_000, 2_000, 4_000, 8_000, 16_000];

export interface ClientOptions {
  sleep?: (ms: number) => Promise<void>;
  terminalRetrySchedule?: number[];
  httpTimeoutMs?: number;
}

/** Response of the MR-thread reply write (PRD #700 M4). `replied` is true when the
 *  driver posted the reply. */
export interface ReplyMRThreadDTO {
  replied: boolean;
}

/** Response of the MR-thread resolve write (PRD #700 M4). `resolved` is true when the
 *  driver resolved the thread; false is the tolerated Forgejo no-op (the forge has no
 *  resolvable-thread concept, so the reply stands alone — reply-only is the documented
 *  Forgejo contract). */
export interface ResolveMRThreadDTO {
  resolved: boolean;
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
   * terminal) does not throw, so a lost ack / duplicate replay is safe. A
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
          // Read the `{run: RunDTO}` body ONCE (a Response body is single-use) and pull
          // status + the PRD #122 M2 effective budget off it together. The budget rides
          // the ACK so a FRESH run — frozen mid-run at plan-approval, after its claim was
          // already issued — learns its scaled cap here (a resume gets it on the claim
          // config instead). Absent/non-numeric ⇒ left undefined = "no budget update".
          const fields = await readRunAck(res);
          const ack: StateAck = {
            applied: res.status === 200,
            status: fields.status,
          };
          if (fields.budgetMaxIterations !== undefined)
            ack.budgetMaxIterations = fields.budgetMaxIterations;
          if (fields.budgetWallSeconds !== undefined)
            ack.budgetWallSeconds = fields.budgetWallSeconds;
          // PRD #634 M2: the scope ceiling + fresh completed count ride the same ACK so m3's
          // loop-top honor gate can read them.
          if (fields.scopeCeiling !== undefined)
            ack.scopeCeiling = fields.scopeCeiling;
          if (fields.completedCount !== undefined)
            ack.completedCount = fields.completedCount;
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

  /**
   * PRD #122 M8 — brokered origin publish of a checkpoint pack, BEST-EFFORT. Unlike every
   * other worker RPC this is NOT JSON: the packfile is the raw `application/octet-stream`
   * body (streamed — a pack can be large, so `pack` is a Readable and `duplex: "half"` is
   * required by undici for a streamed request body), and the checkpoint tip OID rides the
   * `X-Uzi-Checkpoint-Tip` header rather than a JSON field.
   *
   * Best-effort by contract: a publish must NEVER fail the run, so ANY non-2xx
   * (404/413/400/500) returns null rather than throwing, and a 2xx with an empty body reads
   * as null too. The caller (runner.ts publishCheckpointBestEffort) additionally wraps this
   * in a warn-and-swallow. The bearer/version header assembly mirrors fetchRaw; the same
   * per-request timeout is used — generous enough for a real pack, and an abort is just
   * another best-effort miss.
   */
  async publishCheckpoint(
    runId: string,
    tipOid: string,
    pack: Readable,
  ): Promise<PublishResponse | null> {
    const path = `${WORKER_API_PREFIX}/runs/${runId}/publish`;
    const init: RequestInit = {
      method: "POST",
      headers: {
        Authorization: `Bearer ${this.token}`,
        "X-Client-Version": this.version,
        "Content-Type": "application/octet-stream",
        "X-Uzi-Checkpoint-Tip": tipOid,
      },
      body: pack,
      // undici requires `duplex: "half"` for a streamed (Readable) request body; without it
      // the fetch throws before sending. (oxlint no-invalid-fetch-options accepts this shape.)
      duplex: "half",
      signal: AbortSignal.timeout(this.httpTimeoutMs),
    };
    const res = await fetch(this.baseUrl + path, init);
    if (res.status < 200 || res.status >= 300) return null;
    const text = await res.text();
    return text ? (JSON.parse(text) as PublishResponse) : null;
  }

  async getInputs(runId: string): Promise<UserInput[]> {
    const res = (await this.getJSON(`${WORKER_API_PREFIX}/runs/${runId}/inputs`)) as InputsResponse;
    return res.inputs ?? [];
  }

  /** issue #559: lightweight read-only ownership/terminality probe for the interactive
   *  park-SKIP path. Returns the run's current status. Throws a RequestError on 4xx/5xx —
   *  the caller distinguishes a DEFINITIVE 404 (run not owned / reclaimed) from a transient
   *  error via `err.status`. Reuses GetRunOwnedByWorker server-side; no new query. */
  async getRunOwnership(runId: string): Promise<RunOwnershipResponse> {
    return (await this.getJSON(`${WORKER_API_PREFIX}/runs/${runId}/ownership`)) as RunOwnershipResponse;
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

  /** Post a diff-review's structured findings (POST /worker/runs/:id/task-review, PRD
   *  #400 M4b). `targetRunId` is the reviewed task run (claim.review_target_run_id). The
   *  server caps/scrubs the findings and validates `severity`; report-only — nothing is
   *  pushed. Mirrors postReview. */
  async postTaskReview(targetRunId: string, review: TaskReviewRequest): Promise<void> {
    await this.postJSON(`${WORKER_API_PREFIX}/runs/${encodeURIComponent(targetRunId)}/task-review`, review);
  }

  /** Create a PENDING issue proposal on a chat run (POST /worker/runs/:id/proposals).
   *  NEVER writes the forge — the browser confirm does. Returns the created proposal. */
  async createProposal(runId: string, body: CreateProposalRequest): Promise<WorkerProposal> {
    const res = (await this.postJSON(`${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/proposals`, body)) as {
      proposal: WorkerProposal;
    };
    return res.proposal;
  }

  /** Record an INCIDENTAL FINDING for a run (POST /worker/runs/:id/findings, PRD #333
   *  M2). The api derives (user_id, repo_id) from the claimed run (never a client id),
   *  sanitises + canonicalises the text, and persists an evidence row + an `open`
   *  coordinate. NEVER writes the forge — filing is human-gated later (D2/D4). Returns
   *  the created finding's id so the emitted `finding` card can act on it. Throws
   *  RequestError on non-2xx (429 = the per-run cap; the tool catches it and soft-acks). */
  async reportFinding(runId: string, body: ReportFindingRequest): Promise<string> {
    const res = (await this.postJSON(`${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/findings`, body)) as {
      id: string;
    };
    return res.id;
  }

  // ── Inline run summaries (PRD #362 M3c) ────────────────────────────────────
  // Two thin POSTs mirroring reportFinding/saveMemory: the api derives (user, repo)
  // from the CLAIMED run and re-validates + sanitises everything (it, not the worker,
  // is the security boundary). Both throw RequestError on >=400 (the executor hook
  // wraps them in try/catch — a 409 stale-plan and a 400 bad-deltas are expected-
  // benign, and a summary is ADVISORY so it must NEVER fail the run).

  /** Persist the run's INTENT summary (POST /worker/runs/:id/summary/intent). The
   *  server is idempotent-on-set: a second post for a run that already has one is a
   *  no-op success (Decision 3). Throws RequestError on non-2xx. */
  async postIntentSummary(runId: string, summary: string): Promise<void> {
    await this.postJSON(`${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/summary/intent`, { summary });
  }

  /** Persist the run's PLAN summary + deltas (POST /worker/runs/:id/summary/plan).
   *  `plan_md` is the stale-write guard value (Decision 3): the server writes ONLY if
   *  it still matches runs.plan_md, else 409 (a superseded plan, not a run failure).
   *  Invalid deltas are a 400. Throws RequestError on non-2xx. */
  async postPlanSummary(
    runId: string,
    body: { summary: string; deltas: { kind: "added" | "changed" | "dropped"; text: string }[]; plan_md: string },
  ): Promise<void> {
    await this.postJSON(`${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/summary/plan`, body);
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

  // ── Forge read surface (PRD #158) ──────────────────────────────────────────
  // Six worker-mediated, run-scoped forge READ endpoints the run-lane forge MCP
  // server calls. The agent holds NO credential — the API reads the forge on the
  // run's behalf, authed by the join token and scoped by {runId} in the path. All
  // GET, no body. Returned DTOs are UNTRUSTED — the forge server nonce-fences them.
  // A run with no repository is 409; an unknown run/item is 404 (mapped to
  // non-fatal tool text by forge-tools.ts).

  /** One issue's detail (GET /worker/runs/:id/forge/issues/:iid). */
  async getForgeIssue(runId: string, iid: number): Promise<IssueDTO> {
    return (await this.getJSON(
      `${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/forge/issues/${encodeURIComponent(String(iid))}`,
    )) as IssueDTO;
  }

  /** A bounded list of issues (GET /worker/runs/:id/forge/issues?state=&labels=&updated_after=).
   *  All filters optional: `labels` is comma-joined; `updatedAfter` is RFC3339. */
  async listForgeIssues(
    runId: string,
    opts: { state?: "opened" | "closed"; labels?: string[]; updatedAfter?: string },
  ): Promise<IssueListDTO> {
    const params = new URLSearchParams();
    if (opts.state) params.set("state", opts.state);
    if (opts.labels && opts.labels.length > 0) params.set("labels", opts.labels.join(","));
    if (opts.updatedAfter) params.set("updated_after", opts.updatedAfter);
    const q = params.toString() ? `?${params.toString()}` : "";
    return (await this.getJSON(
      `${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/forge/issues${q}`,
    )) as IssueListDTO;
  }

  /** An issue's label add/remove events (GET /worker/runs/:id/forge/issues/:iid/label-events). */
  async listForgeIssueLabelEvents(runId: string, iid: number): Promise<LabelEventListDTO> {
    return (await this.getJSON(
      `${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/forge/issues/${encodeURIComponent(String(iid))}/label-events`,
    )) as LabelEventListDTO;
  }

  /** One merge request's state (GET /worker/runs/:id/forge/merge-requests/:iid). */
  async getForgeMergeRequest(runId: string, iid: number): Promise<MergeRequestDTO> {
    return (await this.getJSON(
      `${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/forge/merge-requests/${encodeURIComponent(String(iid))}`,
    )) as MergeRequestDTO;
  }

  /** A pipeline's jobs (GET /worker/runs/:id/forge/pipelines/:pipelineId/jobs). */
  async getForgePipelineJobs(runId: string, pipelineId: number): Promise<JobListDTO> {
    return (await this.getJSON(
      `${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/forge/pipelines/${encodeURIComponent(String(pipelineId))}/jobs`,
    )) as JobListDTO;
  }

  /** The latest pipeline for exactly one selector (GET /worker/runs/:id/forge/
   *  latest-pipeline?ref=... OR ?mr_iid=...). `pipeline` is null when none matches. */
  async getForgeLatestPipeline(runId: string, sel: { ref?: string; mrIid?: number }): Promise<LatestPipelineDTO> {
    const params = new URLSearchParams();
    if (sel.ref !== undefined) params.set("ref", sel.ref);
    else if (sel.mrIid !== undefined) params.set("mr_iid", String(sel.mrIid));
    const q = params.toString() ? `?${params.toString()}` : "";
    return (await this.getJSON(
      `${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/forge/latest-pipeline${q}`,
    )) as LatestPipelineDTO;
  }

  // Forge WRITE surface (PRD #700 M4): the only worker-mediated forge WRITES besides
  // git push + MR create + label — an MR-thread reply and resolve, for the mr_rework
  // run's write-back. Both derive (repo, connection, project id, mr_iid) from the OWNED
  // run inside the API, and the API enforces the Decision-11 scope check server-side
  // (the reply/resolve id MUST belong to a thread in THIS run's review snapshot for
  // THIS run's mr_iid). A rejected id is a 4xx (mapped to non-fatal tool text by
  // forge-tools.ts). Neither touches `main`.

  /** Reply in the MR review thread keyed on replyId, scoped to this run
   *  (POST /worker/runs/:id/forge/mr-threads/reply). */
  async replyMRThread(runId: string, replyId: string, body: string): Promise<ReplyMRThreadDTO> {
    return (await this.postJSON(
      `${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/forge/mr-threads/reply`,
      { reply_id: replyId, body },
    )) as ReplyMRThreadDTO;
  }

  /** Resolve the MR review thread keyed on resolveId, scoped to this run
   *  (POST /worker/runs/:id/forge/mr-threads/resolve). `resolved:false` is the
   *  tolerated Forgejo no-op (the forge cannot resolve; the reply still stands). */
  async resolveMRThread(runId: string, resolveId: string): Promise<ResolveMRThreadDTO> {
    return (await this.postJSON(
      `${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/forge/mr-threads/resolve`,
      { resolve_id: resolveId },
    )) as ResolveMRThreadDTO;
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
 * Pull `run.status` and the PRD #122 M2 effective budget out of a /state response body
 * (`{"run": <RunDTO>}`, the shape both the 200 and the 409 path return). Reads the body
 * ONCE — a Response body is single-use — so status and both budget numbers must come out
 * of one parse.
 *
 * TOTAL by construction: every failure — an unreadable stream, malformed JSON, a
 * body with no run, a non-string status, a non-numeric budget, an older server that sent
 * nothing — yields the field absent rather than throwing. That is not defensiveness for
 * its own sake: the status caller's rule is a POSITIVE test for one literal, so a missing
 * status means "not parked", and a missing budget means "no budget update" — both the safe
 * answer to every one of those failures. A throw here would instead surface as a state
 * report that appears to have failed, and would be retried against a server that already
 * applied it.
 */
export async function readRunAck(res: Response): Promise<{
  status?: string;
  budgetMaxIterations?: number;
  budgetWallSeconds?: number;
  scopeCeiling?: number;
  completedCount?: number;
}> {
  try {
    const text = await res.text();
    if (!text) return {};
    const parsed = JSON.parse(text) as {
      run?: {
        status?: unknown;
        budget_max_iterations?: unknown;
        budget_wall_seconds?: unknown;
        scope_ceiling?: unknown;
        milestones_completed?: unknown;
      };
    };
    const run = parsed?.run;
    const out: {
      status?: string;
      budgetMaxIterations?: number;
      budgetWallSeconds?: number;
      scopeCeiling?: number;
      completedCount?: number;
    } = {};
    if (typeof run?.status === "string") out.status = run.status;
    if (typeof run?.budget_max_iterations === "number")
      out.budgetMaxIterations = run.budget_max_iterations;
    if (typeof run?.budget_wall_seconds === "number")
      out.budgetWallSeconds = run.budget_wall_seconds;
    // PRD #634 M2: the operator scope ceiling (control channel) and the server's fresh
    // completed-milestone count, both off the same {run: RunDTO} body. scope_ceiling is
    // null (unbounded) unless a scope directive was written. A run whose lead never reported
    // progress reads milestones_completed: null (a nil Go slice marshals to null), which is
    // treated as a completed count of 0 so the ceiling-0 honor gate still fires; otherwise
    // the array's length is the fresh completed count.
    if (typeof run?.scope_ceiling === "number")
      out.scopeCeiling = run.scope_ceiling;
    out.completedCount = Array.isArray(run?.milestones_completed)
      ? run.milestones_completed.length
      : 0;
    return out;
  } catch {
    return {};
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
