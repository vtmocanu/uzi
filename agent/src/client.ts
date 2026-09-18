import type { Readable } from "node:stream";
import type { Logger } from "./log.js";
import {
  WORKER_API_PREFIX,
  type ActiveSnapshot,
  type ClaimRequest,
  type PublishResponse,
  type PublishResult,
  type ChatClaimResponse,
  type ClaimResponse,
  type CreateProposalRequest,
  type CompletionAttemptRequest,
  type CompletionAttemptResponse,
  type CompletionPermitRequest,
  type CompletionPermitResponse,
  type ReportFindingRequest,
  type HeartbeatRequest,
  type MessagesRequest,
  type OutboxHeartbeatEntry,
  type OutgoingMessage,
  type RegisterRequest,
  type RegisterResponse,
  type StateAck,
  type StateRequest,
  type UserInput,
  type InputsResponse,
  type RunOwnershipResponse,
  type RunOrphanClassificationResponse,
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
  type CodexReleaseRequest,
  type CodexReleaseResponse,
  type CodexRefreshRequest,
  type CodexRefreshResponse,
  type RecoveryReserveRequest,
  type RecoveryReserveResponse,
  type RecoveryUploadManifest,
  type RecoveryCaptureStatusResponse,
  type RecoveryReleaseResponse,
  type RecoveryReleaseRequest,
  type RecoveryHoldsResponse,
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
  /** Codex external-auth worker→API slice. Kept below app-server's fixed 10s callback
   *  deadline; the API finishes within 7.5s, leaving response-delivery margin. */
  codexHTTPTimeoutMs?: number;
}

export type CodexReleaseExpectation =
  | { authMode: "subscription"; chatgptAccountId: string; minimumGeneration: number }
  | { authMode: "api_key" };

export interface CodexRefreshExpectation {
  authMode: "subscription";
  chatgptAccountId: string;
}

const CODEX_REFRESH_OUTCOMES = new Set<CodexRefreshResponse["outcome"]>([
  "advanced",
  "replayed",
  "reconciled",
]);

function codexResponseError(): Error {
  return new Error("invalid codex credential response");
}

function responseRecord(raw: unknown): Record<string, unknown> {
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) throw codexResponseError();
  return raw as Record<string, unknown>;
}

function hasExactKeys(record: Record<string, unknown>, expected: readonly string[]): boolean {
  const actual = Object.keys(record).sort();
  const wanted = [...expected].sort();
  return actual.length === wanted.length && actual.every((key, i) => key === wanted[i]);
}

function validGeneration(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
}

function validRefreshOutcome(value: unknown): value is CodexRefreshResponse["outcome"] {
  return typeof value === "string" && CODEX_REFRESH_OUTCOMES.has(value as CodexRefreshResponse["outcome"]);
}

function validateCodexReleaseResponse(
  raw: unknown,
  expected: CodexReleaseExpectation,
): CodexReleaseResponse {
  const body = responseRecord(raw);
  if (expected.authMode === "api_key") {
    if (
      !hasExactKeys(body, ["auth_mode", "access_token"]) ||
      body.auth_mode !== "api_key" ||
      typeof body.access_token !== "string" ||
      body.access_token.length === 0
    ) {
      throw codexResponseError();
    }
    return { auth_mode: "api_key", access_token: body.access_token };
  }

  if (!validGeneration(expected.minimumGeneration) || expected.chatgptAccountId.length === 0) {
    throw codexResponseError();
  }
  if (
    !hasExactKeys(body, [
      "auth_mode",
      "access_token",
      "generation",
      "chatgpt_account_id",
      "chatgpt_plan_type",
    ]) ||
    body.auth_mode !== "subscription" ||
    typeof body.access_token !== "string" ||
    body.access_token.length === 0 ||
    !validGeneration(body.generation) ||
    body.generation < expected.minimumGeneration ||
    body.chatgpt_account_id !== expected.chatgptAccountId ||
    body.chatgpt_plan_type !== null
  ) {
    throw codexResponseError();
  }
  return {
    auth_mode: "subscription",
    access_token: body.access_token,
    generation: body.generation,
    chatgpt_account_id: body.chatgpt_account_id,
    chatgpt_plan_type: null,
  };
}

function validateCodexRefreshResponse(
  raw: unknown,
  expected: CodexRefreshExpectation,
  observedGeneration: number,
): CodexRefreshResponse {
  const body = responseRecord(raw);
  if (
    !hasExactKeys(body, [
      "auth_mode",
      "access_token",
      "generation",
      "chatgpt_account_id",
      "chatgpt_plan_type",
      "outcome",
    ]) ||
    body.auth_mode !== expected.authMode ||
    typeof body.access_token !== "string" ||
    body.access_token.length === 0 ||
    !validRefreshOutcome(body.outcome) ||
    !validGeneration(body.generation) ||
    body.generation <= observedGeneration ||
    (body.outcome === "advanced" && body.generation !== observedGeneration + 1) ||
    body.chatgpt_account_id !== expected.chatgptAccountId ||
    body.chatgpt_plan_type !== null
  ) {
    throw codexResponseError();
  }
  return {
    auth_mode: "subscription",
    access_token: body.access_token,
    generation: body.generation,
    chatgpt_account_id: body.chatgpt_account_id,
    chatgpt_plan_type: null,
    outcome: body.outcome,
  };
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

/** A completion permit as the worker sees it (PRD #1226 M4, D5), mapped from the wire
 *  CompletionPermitResponse.permit. Carries no secret and no forge coordinate. */
export interface CompletionPermit {
  id: string;
  contractRevision: number;
  branch: string;
  head: string;
  issuedAt: string;
  findingIds: string[];
}

/** The completion-permit endpoint's decision (PRD #1226 M4, D5), mapped from
 *  CompletionPermitResponse. `granted` true carries `permit`; `granted` false carries a
 *  `denyReason` from the server taxonomy (and `unmet` for missing_milestones). Every
 *  denial is NON-TERMINAL — the run keeps its status and the caller acts on the reason. */
export interface CompletionPermitResult {
  granted: boolean;
  denyReason?: string;
  unmet?: string[];
  permit?: CompletionPermit;
}

/** Transport for the worker→API control plane (PRD §Worker protocol). */
export class WorkerClient {
  private readonly sleep: (ms: number) => Promise<void>;
  private readonly terminalRetrySchedule: number[];
  private readonly httpTimeoutMs: number;
  private readonly codexHTTPTimeoutMs: number;
  /**
   * The PROTOCOL FEATURES the server advertised on the last successful register. ONE
   * shared negotiation wire (PRD #1392 D7 / #1391 D8): `register()` REPLACES the set from
   * `RegisterResponse.protocol_features`, and a strict-decode rollback fallback CLEARS it
   * (so nothing negotiated is sent again until process restart). Empty until the first
   * register, so a client that has not registered sends today's byte-identical wire.
   */
  private serverFeatures = new Set<string>();

  /**
   * Whether this image advertised the `credential_switch_v1` protocol capability at
   * register (PRD #1247 fix round). A capability worker's runs are fenced server-side,
   * so it stamps `claim_generation` OPTIMISTICALLY — independent of the negotiated
   * `claim_generation_fence` feature, which a one-shot register may have missed under
   * rollout skew. Captured from `register()`'s `protocolCapabilities` argument; false
   * until (and unless) a register advertises it, so a non-capability worker keeps the
   * feature gate. Never cleared by the strict-decode fallback (a capability worker stays
   * optimistic so the next request self-recovers once the api rolls forward).
   */
  private hasCredentialSwitchCapability = false;

  /**
   * The per-registration nonce the api minted on the last successful register (PRD #1390
   * M2a), captured beside `serverFeatures`. The client stamps it onto every ActiveSnapshot
   * it SENDS (heartbeat + claim), so the api can trust the snapshot's epoch across a worker
   * OR api restart. Undefined until an api that advertises `active_run_snapshot` registers;
   * an older api omits it. NOT cleared by the strict-decode fallback — clearing the feature
   * set already stops the snapshot from being sent, and the nonce is re-minted on the next
   * register (process restart) regardless.
   */
  private registerNonce: string | undefined;

  /** The advertised features as an array (PRD #1392 D7): the runner reads this to pick a
   *  capability-aware degradation for a pre-clone forge-unreachable park. Backed by
   *  `serverFeatures` so the two views never diverge and a rollback clear empties both. The
   *  setter routes through the same set (a test that skips register can seed it directly). */
  get protocolFeatures(): string[] {
    return [...this.serverFeatures];
  }
  set protocolFeatures(v: string[]) {
    this.serverFeatures = new Set(v);
  }

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
    this.codexHTTPTimeoutMs = opts.codexHTTPTimeoutMs ?? 8_000;
  }

  async register(
    name: string,
    template?: string,
    maxConcurrentRuns?: number,
    capabilities?: string[],
    protocolCapabilities?: string[],
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
    // Self-reported PROTOCOL capabilities (PRD #1226 M1, D2): what protocols this image
    // implements (today ["completion_interlock_v1"]). Only send when non-empty (mirrors
    // `capabilities`), so an image that implements no protocol keeps the register wire
    // byte-identical to today. The server stores it in workers.protocol_capabilities,
    // SEPARATE from `capabilities`, and the ClaimRun hard clause reads it there.
    if (protocolCapabilities?.length) body.protocol_capabilities = protocolCapabilities;
    const res = (await this.postJSON(`${WORKER_API_PREFIX}/register`, body)) as RegisterResponse;
    // Capture the negotiated protocol features (PRD #1392 D7 / #1391 D8): REPLACE the set
    // from this register's advertisement so a re-register after a rollout reflects the
    // server's current features. Filter to strings (defensive), and treat an absent field
    // as an empty set (older server) ⇒ no wire extension is ever sent.
    this.serverFeatures = new Set(
      Array.isArray(res.protocol_features)
        ? res.protocol_features.filter((f): f is string => typeof f === "string")
        : [],
    );
    // PRD #1247 fix round: remember whether THIS image advertised the credential_switch_v1
    // capability, so the send-gate can stamp the claim generation optimistically (its runs
    // are fenced server-side) even when a one-shot register missed the negotiated feature.
    this.hasCredentialSwitchCapability = protocolCapabilities?.includes("credential_switch_v1") ?? false;
    // PRD #1390 M2a: capture the per-registration nonce the api minted, stamped onto every
    // ActiveSnapshot the worker sends. A string only (defensive); absent on an older api ⇒
    // undefined, in which case no snapshot is sent either (the feature gate is off too).
    this.registerNonce = typeof res.register_nonce === "string" ? res.register_nonce : undefined;
    return res;
  }

  /** Whether the server advertised protocol feature `f` on the last register (PRD
   *  #1391 D8). The gate on every optional wire extension. */
  hasFeature(f: string): boolean {
    return this.serverFeatures.has(f);
  }

  /** Drop the whole negotiated feature set (PRD #1391 M5): after a strict-decode
   *  rollback fallback succeeds, nothing negotiated is sent again until the process
   *  restarts (a re-register would re-populate it). */
  clearFeatures(): void {
    this.serverFeatures.clear();
  }

  async heartbeat(
    stats?: WorkerStats,
    outbox?: OutboxHeartbeatEntry[],
    activeSnapshot?: ActiveSnapshot,
  ): Promise<void> {
    const body: HeartbeatRequest = { version: this.version };
    // Only attach stats when the collector produced a sample (PRD #49): an absent
    // field is the same wire shape as today, so a pre-#49 server ignores the extra
    // bytes and a collector-less tick is indistinguishable from an old worker.
    if (stats) body.stats = stats;
    // PRD #1391 M5: attach the outbox depth ONLY when the server negotiated
    // `heartbeat_outbox` AND there is something to report, so an api that never
    // advertised it sees a byte-identical heartbeat.
    const includeOutbox = this.hasFeature("heartbeat_outbox") && outbox !== undefined && outbox.length > 0;
    if (includeOutbox) body.outbox = outbox;
    // PRD #1390 M2a: attach the active-run snapshot ONLY when the server negotiated
    // `active_run_snapshot` AND the worker built one, stamped with the register nonce so the
    // api can trust its epoch across a restart. Same "byte-identical wire on an old api" shape
    // as `outbox`.
    const includeSnapshot = this.hasFeature("active_run_snapshot") && activeSnapshot !== undefined;
    if (includeSnapshot) body.active_snapshot = { ...activeSnapshot, register_nonce: this.registerNonce };
    const includeExtension = includeOutbox || includeSnapshot;
    try {
      await this.postJSON(`${WORKER_API_PREFIX}/heartbeat`, body);
    } catch (err) {
      // Rollback fallback (PRD #1391 M5, extended by #1390 M2a): a rolled-back api that no
      // longer knows a negotiated heartbeat extension strict-decodes it as an unknown field
      // and answers a generic `invalid request body` 400. Retry the SAME heartbeat ONCE with
      // EVERY negotiated extension stripped (both `outbox` AND `active_snapshot`); if the
      // stripped retry SUCCEEDS, clear the WHOLE cached feature set so nothing negotiated is
      // sent again until process restart. A heartbeat must NEVER be lost to a rolled-back api,
      // so a stripped success is the outcome, not the original 400. Deliberately WHOLE-SET, not
      // surgical: the sticky `credential_switch_v1` capability lives separately from
      // `serverFeatures` and is NOT cleared, so `includeClaimGeneration` keeps stamping
      // `claim_generation` for a capability worker after this clear.
      if (!includeExtension || !isStrictDecodeError(err)) throw err;
      const stripped: HeartbeatRequest = { version: this.version };
      if (stats) stripped.stats = stats;
      await this.postJSON(`${WORKER_API_PREFIX}/heartbeat`, stripped);
      this.clearFeatures();
      this.log.warn(
        "heartbeat extension rejected by a rolled-back api; retried stripped and cleared the negotiated feature set",
        {},
      );
    }
  }

  /** Claim the oldest queued run for this worker's user (the RUN lane — no lane
   *  param, back-compat with older servers). Returns null on 204.
   *
   *  PRD #1390 M2a: carries the worker's {@link ActiveSnapshot} in the claim body when
   *  `active_run_snapshot` is negotiated AND the worker built one (nonce-stamped), so the
   *  api's pre-claim dedupe sees the runs this worker is already executing BEFORE the first
   *  post-outage heartbeat lands. If not negotiated the claim posts an empty body `{}`, which
   *  an old api ignores (harmless — the run-lane claim was bodyless before this). */
  async claimRun(activeSnapshot?: ActiveSnapshot): Promise<ClaimResponse | null> {
    const body: ClaimRequest = {};
    if (this.hasFeature("active_run_snapshot") && activeSnapshot !== undefined) {
      body.active_snapshot = { ...activeSnapshot, register_nonce: this.registerNonce };
    }
    const res = await this.fetchRaw("POST", `${WORKER_API_PREFIX}/runs/claim`, body);
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

  /**
   * The send-gate for `claim_generation` (PRD #1247 fix round). `0` is chat's legacy
   * sentinel and is NEVER sent (the batcher passes `this.generation`, default 0, and
   * ChatRunner sets `generation: 0`; any CLAIMED work run is generation >= 1). A
   * `credential_switch_v1` capability worker stamps OPTIMISTICALLY — its runs are fenced
   * server-side, and a one-shot register may have missed the negotiated feature under
   * rollout skew — while a non-capability (#1391-era) worker keeps the feature gate.
   */
  private includeClaimGeneration(generation?: number): boolean {
    return (
      generation !== undefined &&
      generation > 0 &&
      (this.hasCredentialSwitchCapability || this.hasFeature("claim_generation_fence"))
    );
  }

  /**
   * Run `send(includeField)` with the shared skew-safe fallback (PRD #1247 fix round).
   * REUSABLE: a later milestone drives the completion RPCs through it too. When `included`
   * is true and the api answers the EXACT strict-decode 400 (a rolled-back api that
   * strict-decodes `claim_generation` as an unknown field, `isStrictDecodeError`), retry
   * ONCE with the field stripped and classify only that second response; a capability
   * worker stays OPTIMISTIC (features NOT cleared) so the next request self-recovers once
   * the api rolls forward, while a non-capability worker calls clearFeatures() (today's
   * sticky behavior). A genuine (non-strict-decode) 400 — e.g. an unstorable/invalid
   * message — propagates unchanged, so it is never mistaken for a rollback and the field
   * is never stripped off a genuine-poison request.
   */
  private async withGenerationFallback<T>(
    included: boolean,
    send: (includeField: boolean) => Promise<T>,
  ): Promise<T> {
    try {
      return await send(included);
    } catch (err) {
      if (!included || !isStrictDecodeError(err)) throw err;
      if (!this.hasCredentialSwitchCapability) this.clearFeatures();
      return await send(false);
    }
  }

  async postMessages(
    runId: string,
    messages: OutgoingMessage[],
    generation?: number,
    signal?: AbortSignal,
  ): Promise<void> {
    if (messages.length === 0) return;
    const path = `${WORKER_API_PREFIX}/runs/${runId}/messages`;
    // PRD #1247 fix round: stamp the claim generation through the shared send-gate. A
    // credential_switch_v1 capability worker stamps OPTIMISTICALLY (its runs are fenced
    // server-side, so a register that missed the negotiated `claim_generation_fence`
    // feature under rollout skew must not silently drop the fence); a non-capability
    // (#1391-era) worker keeps the feature gate. `0` is chat's legacy sentinel and is
    // never sent. On the EXACT strict-decode 400 from a rolled-back api the field is
    // stripped and the identical batch retried ONCE — the batcher classifies only that
    // second response, so it sees exactly one outcome. A capability worker does NOT clear
    // its features on the fallback (it stays optimistic, so the next batch self-recovers
    // once the api rolls forward); a non-capability worker clears them (sticky, as today).
    // A genuine unstorable/invalid-message 400 has a DIFFERENT body and propagates
    // unchanged, so the batcher's bisection still runs and the generation is never
    // stripped off a genuine-poison batch.
    const included = this.includeClaimGeneration(generation);
    await this.withGenerationFallback(included, (includeField) => {
      const body: MessagesRequest = { messages };
      if (includeField) body.claim_generation = generation;
      return this.postJSON(path, body, this.httpTimeoutMs, signal);
    });
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
   * A durability-boundary caller supplies its permit signal. It cancels the
   * in-flight request and any retry backoff, so the boundary awaits real request
   * settlement without multiplying its own deadline by this retry schedule.
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
  async reportState(runId: string, body: StateRequest, signal?: AbortSignal): Promise<StateAck> {
    const path = `${WORKER_API_PREFIX}/runs/${runId}/state`;
    // PRD #1247 fix round (E): /state carries claim_generation on EVERY mutating report (M5b's
    // reportState closure stamps it), but /state DisallowUnknownFields-decodes, so a rolled-back api
    // that predates the field 400s the report and — force-roll off — a busy newer worker wedges every
    // run. Route the report through the SAME send-gate + skew-safe fallback as postMessages and the
    // completion RPCs: stamp the field ONLY for a generation>0 capability/feature worker, and on the
    // EXACT strict-decode 400 strip it and retry the identical report ONCE (capability non-sticky so
    // it self-recovers on roll-forward; non-capability sticky, as today). This also makes a <=v0.82
    // claim (generation ?? 0) OMIT the key rather than send 0. The transient-retry loop, AbortSignal,
    // 200/409 single-body ACK parse, already-terminal handling and logging are preserved per variant
    // in reportStateOnce, so the fallback only toggles whether claim_generation is on the wire.
    const included = this.includeClaimGeneration(body.claim_generation);
    return this.withGenerationFallback(included, (includeField) =>
      this.reportStateOnce(runId, path, includeField ? body : { ...body, claim_generation: undefined }, signal),
    );
  }

  /** One /state POST WITH the given body, including the transient-retry loop, AbortSignal
   *  handling, 200/409 single-body ACK parse, already-terminal handling and logging. Split out of
   *  reportState (PRD #1247 fix round E) so the claim_generation send-gate + strict-decode
   *  strip-and-retry can drive it through withGenerationFallback, exactly as postMessages. */
  private async reportStateOnce(runId: string, path: string, body: StateRequest, signal?: AbortSignal): Promise<StateAck> {
    for (let attempt = 0; ; attempt++) {
      signal?.throwIfAborted();
      try {
        const res = await this.fetchRaw("POST", path, body, this.httpTimeoutMs, signal);
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
          // PRD #1189 M1 (D6): the served TOTAL wall (frozen budget + extension) rides the same
          // ACK so the sdk-executor can re-arm its hard wall upward when an owner extends a run
          // already executing.
          if (fields.budgetTotalSeconds !== undefined)
            ack.budgetTotalSeconds = fields.budgetTotalSeconds;
          // PRD #634 M2: the scope ceiling + fresh completed count ride the same ACK so m3's
          // loop-top honor gate can read them.
          if (fields.scopeCeiling !== undefined)
            ack.scopeCeiling = fields.scopeCeiling;
          if (fields.completedCount !== undefined)
            ack.completedCount = fields.completedCount;
          // PRD #1190 M2: pass the server-decided pause boundary through so the reportIteration
          // closure can fold it into the IterationBudget the loop-top pause branch reads.
          if (fields.pauseRequested !== undefined)
            ack.pauseRequested = fields.pauseRequested;
          // PRD #1226 M4 (D3): the server-decided one-shot budget_exhausted steer rides the same
          // ACK beside pauseRequested, so a live post-attempt run learns it must enter the
          // completion hold.
          if (fields.budgetExhausted !== undefined)
            ack.budgetExhausted = fields.budgetExhausted;
          // PRD #1392 M2 (D10): the top-level disposition reason and the RunDTO's
          // recovery_retry_not_before stamp ride the same {run, reason?} ack so the pre-clone
          // forge-park dispatch can tell stale_claim/custody_unsettled apart and quote the retry.
          if (fields.reason !== undefined) ack.reason = fields.reason;
          if (fields.recoveryRetryNotBefore !== undefined)
            ack.recoveryRetryNotBefore = fields.recoveryRetryNotBefore;
          // PRD #1247 M5b: fold the top-level held-state credential-switch signal and the
          // stale-claim disposition off the SAME single-use body. A stale-claim 409 is still
          // returned as {applied:false, staleClaim:true}; the runner's reportState closure throws
          // StaleClaimError on that flag to stop the superseded flight.
          if (fields.credentialSwitch !== undefined)
            ack.credentialSwitch = fields.credentialSwitch;
          if (fields.staleClaim !== undefined)
            ack.staleClaim = fields.staleClaim;
          if (fields.credentialSwitchReleased !== undefined)
            ack.credentialSwitchReleased = fields.credentialSwitchReleased;
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
        if (signal?.aborted) throw err;
        if (this.isAlreadyTerminal(err)) {
          // A 4xx whose TEXT says terminal. The status is not recoverable here, and
          // an absent status is the safe answer for every caller.
          this.log.info("state already terminal server-side, treating as success", { run_id: runId, status: body.status });
          return { applied: false, status: undefined };
        }
        if (!isTransient(err) || attempt >= this.terminalRetrySchedule.length) throw err;
        const delay = this.terminalRetrySchedule[attempt] ?? 0;
        this.log.warn("state report failed, retrying", { run_id: runId, status: body.status, attempt, delay_ms: delay });
        await abortableSleep(this.sleep, delay, signal);
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
   * Best-effort by contract: a publish must NEVER fail the run. But the OUTCOME is no
   * longer discarded (issue #1030): rather than collapse every non-2xx to an
   * indistinguishable `null`, this returns a {@link PublishResult} discriminated union so
   * the caller (runner.ts publishCheckpointBestEffort) can name the HTTP status on the run
   * feed. ANY non-2xx (404/413/400/500) returns `{ ok: false, httpStatus: res.status }`
   * rather than throwing, and a 2xx with an empty body returns `{ ok: false, httpStatus }`
   * too (the publish could not be confirmed). A 2xx with a parseable body returns
   * `{ ok: true, body }` — which the caller further inspects for a best-effort skip
   * (`published: false`). The bearer/version header assembly mirrors fetchRaw; the same
   * per-request timeout is used — generous enough for a real pack, and an abort is just
   * another best-effort miss (it throws, caught by the caller's warn-and-swallow).
   */
  async publishCheckpoint(
    runId: string,
    tipOid: string,
    pack: Readable,
    boundarySignal?: AbortSignal,
  ): Promise<PublishResult> {
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
      signal: boundarySignal
        ? AbortSignal.any([boundarySignal, AbortSignal.timeout(this.httpTimeoutMs)])
        : AbortSignal.timeout(this.httpTimeoutMs),
    };
    const res = await fetch(this.baseUrl + path, init);
    if (res.status < 200 || res.status >= 300) return { ok: false, httpStatus: res.status };
    const text = await res.text();
    // A 2xx with no body cannot confirm the publish landed; treat it as a non-confirming
    // outcome carrying the 2xx code rather than fabricating a body.
    if (!text) return { ok: false, httpStatus: res.status };
    return { ok: true, body: JSON.parse(text) as PublishResponse };
  }

  // ── Durable run recovery archive RPC (PRD #1296 M3, D3/D5) ─────────────────────
  // The Bearer-authenticated worker↔API archive endpoints. Shapes are the M1-frozen
  // protocol types (agent/src/protocol.ts, mirrored from apitypes/recovery.go). The bundle
  // bytes NEVER travel in JSON (D6): the upload streams the raw bundle as the request body
  // and carries its manifest in the X-Uzi-Recovery-Manifest header. reserve/status/release
  // are ordinary JSON. All four are idempotent by the M1 contract (a lost ACK re-reserves
  // the same capture; a retry re-uploads from zero; release is idempotent once none remain).

  /** Reserve (or idempotently re-reserve) a capture under the run's open custody hold.
   *  JSON POST → capture_id + lifecycle state. Throws RequestError on 4xx/5xx so the
   *  caller retries within the model-free window. */
  async reserveRecoveryCapture(
    runId: string,
    req: RecoveryReserveRequest,
  ): Promise<RecoveryReserveResponse> {
    return (await this.postJSON(
      `${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/archives/reserve`,
      req,
    )) as RecoveryReserveResponse;
  }

  /** By-id status poll of a capture (handles lost ACKs; on restart tells whether the byte
   *  manifest is already bound). JSON GET. Throws RequestError on 4xx/5xx. */
  async getRecoveryCaptureStatus(
    runId: string,
    captureId: string,
  ): Promise<RecoveryCaptureStatusResponse> {
    return (await this.getJSON(
      `${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/archives/${encodeURIComponent(captureId)}`,
    )) as RecoveryCaptureStatusResponse;
  }

  /**
   * Bind the byte manifest (compare-and-set) and stream the verified bundle in ONE request
   * (D4: one streaming binary request per upload, the server splits it into encrypted
   * chunks). Mirrors publishCheckpoint's transport: the raw bundle is the
   * `application/octet-stream` body (a Readable, so `duplex: "half"` is required by undici),
   * and the manifest rides the `X-Uzi-Recovery-Manifest` JSON header rather than a JSON
   * field. A non-2xx throws RequestError (the caller keeps the source pinned and retries);
   * a 2xx returns the capture's post-upload status. The per-request timeout matches the
   * other terminal-boundary RPCs.
   */
  async uploadRecoveryBundle(
    runId: string,
    captureId: string,
    manifest: RecoveryUploadManifest,
    bundle: Readable,
    boundarySignal?: AbortSignal,
  ): Promise<RecoveryCaptureStatusResponse> {
    const path = `${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/archives/${encodeURIComponent(captureId)}/upload`;
    const init: RequestInit = {
      method: "POST",
      headers: {
        Authorization: `Bearer ${this.token}`,
        "X-Client-Version": this.version,
        "Content-Type": "application/octet-stream",
        "X-Uzi-Recovery-Manifest": JSON.stringify(manifest),
      },
      body: bundle,
      duplex: "half",
      signal: boundarySignal
        ? AbortSignal.any([boundarySignal, AbortSignal.timeout(this.httpTimeoutMs)])
        : AbortSignal.timeout(this.httpTimeoutMs),
    };
    const res = await fetch(this.baseUrl + path, init);
    if (res.status >= 400) throw await this.toError("POST", path, res);
    const text = await res.text();
    return (text ? JSON.parse(text) : {}) as RecoveryCaptureStatusResponse;
  }

  /** Release the run's custody after a successful full publication (D3). JSON POST with an
   *  empty body; idempotent (holds_released is 0 once none remain open). Throws
   *  RequestError on 4xx/5xx. */
  async releaseRecoveryCustody(
    runId: string,
    generation?: number,
    releaseEvidence?: string,
  ): Promise<RecoveryReleaseResponse> {
    // Backward-compatible: an omitted generation posts an empty body (the v1 settle-by-
    // run+worker path); a v2 caller names the exact generation (PRD #1349 M1, D1/D2).
    const body: RecoveryReleaseRequest = generation !== undefined ? { generation } : {};
    // PRD #1392 M1/M2 (fact 9): stamp the release's evidence class when the caller knows it
    // ({publication, forge_no_output}); an omitted class is allowed (the api stores NULL).
    if (releaseEvidence !== undefined) body.release_evidence = releaseEvidence;
    return (await this.postJSON(
      `${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/archives/release`,
      body,
    )) as RecoveryReleaseResponse;
  }

  /** listRecoveryHolds returns this worker's own open custody holds on a run — the
   *  post-clone generation-exact inventory (PRD #1349 M1, D3). holds is always an array. */
  async listRecoveryHolds(runId: string): Promise<RecoveryHoldsResponse> {
    return (await this.getJSON(
      `${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/recovery-holds`,
    )) as RecoveryHoldsResponse;
  }

  async getInputs(runId: string): Promise<{ inputs: UserInput[]; credentialSwitch?: { generation: number } }> {
    const res = (await this.getJSON(`${WORKER_API_PREFIX}/runs/${runId}/inputs`)) as InputsResponse;
    // PRD #1247 M5b: the held-state credential-switch signal rides EVERY inputs response (including
    // an empty-inputs poll), surfaced beside the inputs. The runner's poll trips the switch off it
    // (steering.maybeTripCredentialSwitch); a legacy caller that reads only `.inputs` is unaffected.
    return { inputs: res.inputs ?? [], credentialSwitch: res.credential_switch };
  }

  /** issue #559: lightweight read-only ownership/terminality probe for the interactive
   *  park-SKIP path. Returns the run's current status. Throws a RequestError on 4xx/5xx —
   *  the caller distinguishes a DEFINITIVE 404 (run not owned / reclaimed) from a transient
   *  error via `err.status`. Reuses GetRunOwnedByWorker server-side; no new query. */
  async getRunOwnership(runId: string): Promise<RunOwnershipResponse> {
    return (await this.getJSON(`${WORKER_API_PREFIX}/runs/${runId}/ownership`)) as RunOwnershipResponse;
  }

  /** issue #1319 — the orphan-classification read: authoritative identity of a clone-orphan
   *  OWNER run, for the runner's owner-derived reclaim validation. `{id}` is the CLAIMANT run
   *  this worker holds (authz anchor); `owner` is the orphan owner's run id. A 404 (owner not
   *  in this owner+repo scope) throws RequestError, which the caller treats as fail-closed. */
  async getRunOrphanClassification(claimantRunId: string, ownerRunId: string): Promise<RunOrphanClassificationResponse> {
    return (await this.getJSON(
      `${WORKER_API_PREFIX}/runs/${claimantRunId}/orphan-classification?owner=${encodeURIComponent(ownerRunId)}`,
    )) as RunOrphanClassificationResponse;
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

  /** Record ONE same-session structural completion attempt (POST /worker/runs/:id/completion/
   *  attempt, PRD #1226 M3). The server union-merges the lead's declaration into
   *  milestones_completed, recomputes the unmet structural criteria over the merged set, and
   *  records a bounded attempt — all server-authoritative, in one call (this collapses the M2
   *  "persist declaration THEN attempt" ordering hazard). Returns the recomputed unmet set (the
   *  executor reworks against THIS, never its own belief) and the new attempt count. Throws
   *  RequestError on non-2xx (409 stale claim, 400 not interlocked) — the caller decides. */
  async recordCompletionAttempt(
    runId: string,
    args: {
      milestonesCompleted: string[];
      head: string | null;
      worktreeFingerprint: string | null;
      claimGeneration?: number;
    },
  ): Promise<{ unmet: string[]; attemptCount: number }> {
    const path = `${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/completion/attempt`;
    // PRD #1247 fix round: stamp the claim-lane generation through the shared send-gate so a
    // completion attempt racing a rollback recovers instead of hard-400ing. A
    // credential_switch_v1 capability worker stamps OPTIMISTICALLY (its runs are fenced
    // server-side, so a register that missed the negotiated `claim_generation_fence` feature
    // under rollout skew must not silently drop the fence); a non-capability (#1391-era) worker
    // keeps the feature gate; `0`/unset omits the field. On the EXACT strict-decode 400 from a
    // rolled-back api the field is stripped and the identical attempt retried ONCE — the
    // recompute+attempt is server-authoritative in ONE call, so the first (rejected) request
    // records nothing and the stripped retry applies exactly once. A capability worker stays
    // optimistic (features NOT cleared, self-recovers once the api rolls forward); a
    // non-capability worker clears them (sticky, as today). A genuine business 400 (e.g. "not
    // interlocked") is NOT a strict-decode 400 and propagates unchanged — no strip, no retry.
    const included = this.includeClaimGeneration(args.claimGeneration);
    const res = (await this.withGenerationFallback(included, (includeField) => {
      const body: CompletionAttemptRequest = {
        milestones_completed: args.milestonesCompleted,
        head: args.head,
        worktree_fingerprint: args.worktreeFingerprint,
      };
      if (includeField) body.claim_generation = args.claimGeneration;
      return this.postJSON(path, body);
    })) as CompletionAttemptResponse;
    return { unmet: res.unmet ?? [], attemptCount: res.attempt_count ?? 0 };
  }

  /** Ask the server to issue a completion permit for the exact-head verify (POST /worker/
   *  runs/:id/completion/permit, PRD #1226 M4, D5). The server recomputes its OWNED structural
   *  predicates and, on a clean recompute, issues (idempotently) a permit bound to (run,
   *  contract_revision, branch, head). Every rejection is a NON-TERMINAL structured denial —
   *  `granted:false` with a `denyReason` (and `unmet` for missing_milestones) — so the caller
   *  reworks/holds/stops rather than treating it as an error. Throws RequestError only on a
   *  transport/HTTP error (the denial is a 200 body, not a 4xx). */
  async requestCompletionPermit(
    runId: string,
    args: { contractRevision: number; branch: string; head: string; claimGeneration?: number },
  ): Promise<CompletionPermitResult> {
    const path = `${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/completion/permit`;
    // PRD #1247 fix round: stamp the claim-lane generation through the shared send-gate so a
    // permit request racing a rollback recovers instead of hard-400ing. A credential_switch_v1
    // capability worker stamps OPTIMISTICALLY (its runs are fenced server-side, so a register
    // that missed the negotiated `claim_generation_fence` feature under rollout skew must not
    // silently drop the fence); a non-capability (#1391-era) worker keeps the feature gate;
    // `0`/unset omits the field. On the EXACT strict-decode 400 from a rolled-back api the field
    // is stripped and the identical request retried ONCE — permit ISSUANCE is idempotent
    // server-side, and the first (rejected) request issues nothing, so the stripped retry issues
    // exactly once. A capability worker stays optimistic (features NOT cleared, self-recovers
    // once the api rolls forward); a non-capability worker clears them (sticky, as today). A
    // granted:false denial is a 200 body, not an error, so it never reaches the fallback; a
    // genuine transport/HTTP 400 that is NOT a strict-decode 400 propagates unchanged.
    const included = this.includeClaimGeneration(args.claimGeneration);
    const res = (await this.withGenerationFallback(included, (includeField) => {
      const body: CompletionPermitRequest = {
        contract_revision: args.contractRevision,
        branch: args.branch,
        head: args.head,
      };
      if (includeField) body.claim_generation = args.claimGeneration;
      return this.postJSON(path, body);
    })) as CompletionPermitResponse;
    const result: CompletionPermitResult = { granted: res.granted ?? false };
    if (res.deny_reason !== undefined) result.denyReason = res.deny_reason;
    if (res.unmet !== undefined) result.unmet = res.unmet;
    if (res.permit) {
      result.permit = {
        id: res.permit.id,
        contractRevision: res.permit.contract_revision,
        branch: res.permit.branch,
        head: res.permit.head,
        issuedAt: res.permit.issued_at,
        findingIds: res.permit.finding_ids ?? [],
      };
    }
    return result;
  }

  /** Ask the server to park an interlocked run on the completion HOLD (POST /worker/runs/:id/
   *  completion/hold, PRD #1226 M4, D6). The server returns `{run: RunDTO}` on BOTH 200 (hold
   *  landed, status becomes "paused") AND 409 (hold refused, RETAIN the run) — a 409 is NOT an
   *  error here. CRITICAL: the worker's park order keys off the RETURNED run.status being
   *  literally "paused", so both statuses are read off the body (like reportState) rather than a
   *  409 throwing through postJSON. Returns `{ status }` = the run's status from the response
   *  body (paused on a landed hold, the real status on a refusal); an unmodelled 2xx or an
   *  unreadable body yields "" (never "paused" ⇒ retain, the safe default). A genuine transport
   *  error or a non-200/409 4xx/5xx still throws RequestError. */
  async requestCompletionHold(
    runId: string,
    args: { head: string; claimGeneration?: number },
  ): Promise<{ status: string }> {
    const path = `${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/completion/hold`;
    // PRD #1247 fix round: stamp the claim-lane generation through the shared send-gate so a
    // hold request racing a rollback recovers instead of hard-400ing. A credential_switch_v1
    // capability worker stamps OPTIMISTICALLY (its runs are fenced server-side, so a register
    // that missed the negotiated `claim_generation_fence` feature under rollout skew must not
    // silently drop the fence); a non-capability (#1391-era) worker keeps the feature gate;
    // `0`/unset omits the field. The whole fetchRaw + 200/409 readRunAck read runs INSIDE the
    // closure, so a stripped retry re-runs the identical request and preserves the 200/409
    // semantics (a 409 is a refusal that RETAINS the run, NOT an error — it returns the run's
    // real status). Only a genuine non-200/409 throw reaches the fallback; a rolled-back api
    // answers the completion fence field with a strict-decode 400 (NOT a 409), which the fallback
    // catches and retries stripped ONCE — the first (rejected) request parks nothing, so the
    // retry applies exactly once. A capability worker stays optimistic (features NOT cleared,
    // self-recovers once the api rolls forward); a non-capability worker clears them (sticky, as
    // today). A genuine non-200/409 4xx/5xx that is NOT a strict-decode 400 propagates unchanged.
    const included = this.includeClaimGeneration(args.claimGeneration);
    return this.withGenerationFallback(included, async (includeField) => {
      const body: { head: string; claim_generation?: number } = { head: args.head };
      if (includeField) body.claim_generation = args.claimGeneration;
      const res = await this.fetchRaw("POST", path, body);
      if (res.status === 200 || res.status === 409) {
        const fields = await readRunAck(res);
        return { status: fields.status ?? "" };
      }
      if (res.status >= 400) throw await this.toError("POST", path, res);
      // A 2xx we do not model (e.g. 204 from an older server): no status came back, which is
      // not "paused" and so retains the run.
      return { status: "" };
    });
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

  // ── Codex credential bridge (PRD #1171 M1), ships DARK ─────────────────────
  // Bearer-only, run-scoped worker→API routes over the API's coordinated-refresh service.
  // The run id is the URL path — NEVER the body. Consumed by the Codex executor
  // composition (a later milestone, m3); an api_key run only ever calls releaseCodex.
  // Both responses are Cache-Control: no-store and secret-bearing (an access token), so the
  // caller must treat the result like a claim secret — never log or persist it beyond use.

  /** Re-fetch the run's currently-committed Codex auth (POST /worker/runs/:id/codex/release).
   *  `expected` is the immutable claim binding: a cross-mode/account response, stale
   *  generation, non-null plan, or API-key subscription field fails closed. `signal` is
   *  combined with the fixed Codex HTTP deadline. */
  async releaseCodex(
    runId: string,
    req: CodexReleaseRequest,
    expected: CodexReleaseExpectation,
    signal?: AbortSignal,
  ): Promise<CodexReleaseResponse> {
    const raw = await this.postCodexJSON(
      `${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/codex/release`,
      req,
      signal,
    );
    return validateCodexReleaseResponse(raw, expected);
  }

  /** Run the coordinated subscription refresh and release the freshly-committed auth
   *  token (POST /worker/runs/:id/codex/refresh). `req.operation_id` MUST be generated ONCE
   *  per logical refresh and RETAINED by the caller across retries: on an HTTP timeout or a
   *  lost reply, re-call with the SAME operation_id so the server replays its prior result
   *  rather than starting a second provider exchange (the worker never begins a second
   *  exchange blindly). Subscription runs only. The response must match the immutable
   *  account, advance the observed generation, carry a null plan and use a closed outcome;
   *  caller cancellation is combined with the fixed Codex HTTP deadline. */
  async refreshCodex(
    runId: string,
    req: CodexRefreshRequest,
    expected: CodexRefreshExpectation,
    signal?: AbortSignal,
  ): Promise<CodexRefreshResponse> {
    if (
      !validGeneration(req.observed_generation) ||
      req.observed_generation === Number.MAX_SAFE_INTEGER ||
      expected.chatgptAccountId.length === 0
    ) {
      throw codexResponseError();
    }
    const raw = await this.postCodexJSON(
      `${WORKER_API_PREFIX}/runs/${encodeURIComponent(runId)}/codex/refresh`,
      req,
      signal,
    );
    return validateCodexRefreshResponse(raw, expected, req.observed_generation);
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

  private async postJSON(
    path: string,
    body: unknown,
    timeoutMs = this.httpTimeoutMs,
    callerSignal?: AbortSignal,
  ): Promise<unknown> {
    const res = await this.fetchRaw("POST", path, body, timeoutMs, callerSignal);
    if (res.status >= 400) throw await this.toError("POST", path, res);
    if (res.status === 204) return undefined;
    const text = await res.text();
    return text ? JSON.parse(text) : undefined;
  }

  private async postCodexJSON(path: string, body: unknown, signal?: AbortSignal): Promise<unknown> {
    const res = await this.fetchRaw("POST", path, body, this.codexHTTPTimeoutMs, signal);
    if (res.status >= 400) throw await this.toError("POST", path, res);
    const text = await res.text();
    if (!text) throw codexResponseError();
    try {
      return JSON.parse(text) as unknown;
    } catch {
      throw codexResponseError();
    }
  }

  private async getJSON(path: string): Promise<unknown> {
    const res = await this.fetchRaw("GET", path, undefined);
    if (res.status >= 400) throw await this.toError("GET", path, res);
    const text = await res.text();
    return text ? JSON.parse(text) : undefined;
  }

  private async fetchRaw(
    method: "GET" | "POST",
    path: string,
    body: unknown,
    timeoutMs = this.httpTimeoutMs,
    callerSignal?: AbortSignal,
  ): Promise<Response> {
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
      signal: callerSignal
        ? AbortSignal.any([callerSignal, AbortSignal.timeout(timeoutMs)])
        : AbortSignal.timeout(timeoutMs),
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
  budgetTotalSeconds?: number;
  scopeCeiling?: number;
  completedCount?: number;
  pauseRequested?: boolean;
  budgetExhausted?: boolean;
  reason?: string;
  recoveryRetryNotBefore?: string;
  credentialSwitch?: { generation: number };
  staleClaim?: boolean;
  credentialSwitchReleased?: boolean;
}> {
  try {
    const text = await res.text();
    if (!text) return {};
    const parsed = JSON.parse(text) as {
      // PRD #1392 M2 (D10): `reason` is TOP-LEVEL on the {run, reason?} ack body, NOT under run.
      reason?: unknown;
      run?: {
        status?: unknown;
        budget_max_iterations?: unknown;
        budget_wall_seconds?: unknown;
        budget_total_seconds?: unknown;
        scope_ceiling?: unknown;
        milestones_completed?: unknown;
        pause_requested?: unknown;
        completion_budget_exhausted?: unknown;
        // PRD #1392 M2 (D10): the retry-not-before stamp is a FIELD ON the RunDTO.
        recovery_retry_not_before?: unknown;
      };
      // PRD #1247 M5b: the held-state signal + stale-claim disposition ride TOP-LEVEL,
      // beside `run`, not inside it.
      credential_switch?: unknown;
      disposition?: unknown;
    };
    const run = parsed?.run;
    const out: {
      status?: string;
      budgetMaxIterations?: number;
      budgetWallSeconds?: number;
      budgetTotalSeconds?: number;
      scopeCeiling?: number;
      completedCount?: number;
      pauseRequested?: boolean;
      budgetExhausted?: boolean;
      reason?: string;
      recoveryRetryNotBefore?: string;
      credentialSwitch?: { generation: number };
      staleClaim?: boolean;
      credentialSwitchReleased?: boolean;
    } = {};
    if (typeof run?.status === "string") out.status = run.status;
    if (typeof run?.budget_max_iterations === "number")
      out.budgetMaxIterations = run.budget_max_iterations;
    if (typeof run?.budget_wall_seconds === "number")
      out.budgetWallSeconds = run.budget_wall_seconds;
    // PRD #1189 M1 (D6): the served TOTAL wall (frozen budget + extension). null (a run with no
    // wall deadline) or absent (older server) leaves it undefined, and the sdk-executor falls
    // back to budgetWallSeconds.
    if (typeof run?.budget_total_seconds === "number")
      out.budgetTotalSeconds = run.budget_total_seconds;
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
    // PRD #1190 M2: the server-decided pause boundary rides the same {run: RunDTO} body. A
    // boolean only — a non-boolean (older server that omits it, unparseable value) leaves it
    // absent, which the loop-top pause branch reads as "no pause requested".
    if (typeof run?.pause_requested === "boolean")
      out.pauseRequested = run.pause_requested;
    // PRD #1226 M4 (D3): the server-decided one-shot budget_exhausted steer rides the same
    // {run: RunDTO} body beside pause_requested. A boolean only — a non-boolean (older server
    // that omits it, unparseable value) leaves it absent, which the completion-hold branch
    // reads as "no budget-exhausted steer".
    if (typeof run?.completion_budget_exhausted === "boolean")
      out.budgetExhausted = run.completion_budget_exhausted;
    // PRD #1392 M2 (D10): the TOP-LEVEL disposition reason ("stale_claim"|"custody_unsettled")
    // and the RunDTO's `recovery_retry_not_before` stamp for a forge-unreachable park ack. Both
    // string-only; a non-string (older server, unparseable value) leaves them absent, which the
    // forge-park dispatch reads as "no reason" / "retry after backoff" — the safe defaults.
    if (typeof parsed?.reason === "string") out.reason = parsed.reason;
    if (typeof run?.recovery_retry_not_before === "string")
      out.recoveryRetryNotBefore = run.recovery_retry_not_before;
    // PRD #1247 M5b: the held-state credential-switch signal and the stale-claim disposition ride
    // the SAME single-use body, TOP-LEVEL beside `run` (present on the 200 ack, the ordinary 409,
    // and — for stale_claim — the superseded/released 409). Both additive + optional and
    // total-by-construction like the rest of this parse: a malformed/absent value leaves the field
    // undefined. M5b acts on them — MINOR-7 trips the switch off credentialSwitch, and the
    // reportState closure throws StaleClaimError off staleClaim.
    const cs = parsed?.credential_switch;
    if (typeof cs === "object" && cs !== null && typeof (cs as { generation?: unknown }).generation === "number")
      out.credentialSwitch = { generation: (cs as { generation: number }).generation };
    if (parsed?.disposition === "stale_claim") out.staleClaim = true;
    // PRD #1247 M5b (BLOCKING-2 rework): the held-state credential-switch RELEASE applied. The
    // server sets it on the 200 ack for BOTH a fresh requeue (status 'queued') and an idempotent
    // release after a reclaim (status 'running'), so enterCredentialSwitch accepts the release off
    // this flag rather than off status === 'queued' (which missed the idempotent case).
    if (parsed?.disposition === "released") out.credentialSwitchReleased = true;
    return out;
  } catch {
    return {};
  }
}

/**
 * The generic strict-decode signal (PRD #1391 D9): a 400 whose body is the api's
 * generic `invalid request body` answer — what a rolled-back api returns when it
 * strict-decodes an unknown field (a negotiated wire extension it no longer knows).
 * Keyed ONLY on that body, so the dedicated unstorable-message 400 (a DIFFERENT
 * body/reason from the messages route) and the invalid-message 400 never match — they
 * must propagate so the batcher's bisection still runs. A typed code is preferred if
 * the api gains one; until then the body string is the discriminator the PRD names.
 */
export function isStrictDecodeError(err: unknown): boolean {
  return err instanceof RequestError && err.status === 400 && /invalid request body/i.test(err.body);
}

/** Retryable: transport failures, 5xx, and 408/429; permanent otherwise. */
export function isTransient(err: unknown): boolean {
  if (err instanceof RequestError) {
    return err.status >= 500 || err.status === 408 || err.status === 429;
  }
  // Network error / timeout (AbortError) / non-HTTP failure.
  return true;
}

/** Sleep through an injected or real timer without letting a durability-boundary
 * cancellation strand reportState in its retry backoff. */
function abortableSleep(
  sleeper: (ms: number) => Promise<void>,
  ms: number,
  signal: AbortSignal | undefined,
): Promise<void> {
  if (!signal) return sleeper(ms);
  signal.throwIfAborted();
  return new Promise<void>((resolve, reject) => {
    const onAbort = (): void => {
      signal.removeEventListener("abort", onAbort);
      reject(signal.reason);
    };
    signal.addEventListener("abort", onAbort, { once: true });
    sleeper(ms).then(
      () => {
        signal.removeEventListener("abort", onAbort);
        resolve();
      },
      (error: unknown) => {
        signal.removeEventListener("abort", onAbort);
        reject(error);
      },
    );
  });
}
