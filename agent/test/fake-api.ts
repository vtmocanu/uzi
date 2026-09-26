import http from "node:http";
import { randomUUID } from "node:crypto";
import type { AddressInfo } from "node:net";
import type {
  ClaimResponse,
  OutgoingMessage,
  RunOrphanClassificationResponse,
  StateRequest,
  UserInput,
  WorkerRunDetail,
  WorkerRunListItem,
  WorkerRunMessage,
} from "../src/protocol.js";

interface RecordedRegister {
  name: string;
  version: string;
  /** The self-reported worker template (PRD #18), or undefined when the worker
   *  sends no `template` field (older image). */
  template?: string;
  /** The self-reported capability set (PRD #83 Q1), or undefined when the worker
   *  sends no `capabilities` field (no daemon wired / older image). */
  capabilities?: string[];
  /** The self-reported PROTOCOL capability set (PRD #1226 M1, D2), or undefined when
   *  the worker sends no `protocol_capabilities` field (an older image). */
  protocol_capabilities?: string[];
  authorized: boolean;
}

/**
 * In-process HTTP server that speaks the worker protocol (PRD §Worker
 * protocol). Records every request for assertions and supports failure
 * injection for the backoff / terminal-idempotency paths. Not a fake of the
 * real server's behavior beyond the wire contract the worker depends on.
 */
export class FakeApi {
  private readonly server: http.Server;

  // --- control -------------------------------------------------------------
  private readonly claimQueue: ClaimResponse[] = [];
  private readonly inputsByRun = new Map<string, UserInput[]>();
  private readonly ackedByRun = new Map<string, Set<number>>();
  private readonly appliedByRun = new Map<string, Set<number>>();
  private readonly receiptGeneration = new Map<string, number>();
  private readonly lostReceiptReplies = new Map<string, number>();
  private readonly seenInputIds = new Map<string, Set<number>>();
  private readonly receiptFenceReason = new Map<string, string>();
  private readonly failingReceipts = new Map<"ack" | "applied", number>();
  private readonly failingReceiptReasons = new Map<"ack" | "applied", string>();
  /** Runs whose credential switch becomes pending right after the next successful ACK. */
  private readonly switchAfterAck = new Set<string>();
  /** Runs with a pending switch: GET drains nothing (the server fence) and APPLIED is refused. */
  private readonly switchPendingRuns = new Set<string>();
  private readonly delayedReceipts = new Map<"ack" | "applied", number>();
  /** Issue #1604: a pending switch whose `credential_switch {generation}` signal rides every GET
   *  (the real server's transport). Absent keeps the #1673 shape: no signal on the GET. */
  private readonly switchSignalGeneration = new Map<string, number>();
  /** Issue #1604: the claim generation that ACKed each input id, per run, so the server's
   *  own-revise APPLIED exemption under switch_pending (D6) can be mirrored. */
  private readonly ackGenerationByRun = new Map<string, Map<number, number>>();
  /** Issue #1604: count-limited receipt failures (consumed before the persistent failInputReceipts). */
  private readonly failingReceiptsRemaining = new Map<"ack" | "applied", { times: number; status: number }>();
  /** Issue #1604: receipts held BEFORE evaluation until the test releases them (a late receipt). */
  private readonly receiptHolds = new Map<"ack" | "applied", Promise<void>[]>();
  /** Issue #1604: GET /inputs failure, delay and raw-body injection, per run. */
  private readonly inputGetFailures = new Map<string, { times: number; status: number }>();
  private readonly inputGetDelays = new Map<string, { times: number; ms: number }>();
  private readonly inputGetRaw = new Map<string, { times: number; body: unknown }>();
  /** Issue #1604: GET /inputs reads per run (answered or not), so a test can prove polling continued. */
  readonly inputGets = new Map<string, number>();
  /** Issue #1604: /state reports this run drops (connection destroyed, nothing recorded). */
  private readonly droppedStates = new Map<string, (body: StateRequest) => boolean>();
  /** Issue #1604: the last recorded /state status per run, the fake's view of the run row. */
  private readonly lastRecordedStatus = new Map<string, string>();
  /** Issue #1604: mirror SetRunRecoveryWait's guard: a recovery_wait report applies only while the
   *  run is `running`; otherwise it is refused 409 with the run's real status. Off by default. */
  recoveryWaitRequiresRunning = false;
  /** Issue #1604: one ordered log of state records and receipt calls/replies, per request, so a test
   *  can pin the order of an APPLIED against the report that made it due. */
  readonly timeline: Array<
    | { type: "state"; runId: string; status: string; plan_md?: string }
    | { type: "receipt_call"; runId: string; kind: "ack" | "applied"; ids: number[]; generation: number }
    | { type: "receipt_reply"; runId: string; kind: "ack" | "applied"; ids: number[]; httpStatus: number }
  > = [];
  /** Issue #1673: receipt tests set this so a receipt for a run with no explicit claim
   *  generation fails loudly instead of being accepted as the only claim. */
  strictReceiptGenerations = false;
  /** Issue #1673: answer GET /inputs like an older api pod: consume on read, no receipt marker. */
  legacyConsumeOnRead = false;
  private nextSyntheticInputId = 1_000_000;
  readonly inputReceiptCalls: Array<{ runId: string; kind: "ack" | "applied"; ids: number[]; generation: number }> = [];
  /** Receipts answered 200, in reply order (a delayed reply lands here only once it is sent). */
  readonly inputReceiptReplies: Array<{ runId: string; kind: "ack" | "applied" }> = [];
  // Issue #1660: the follow_up inputs /inputs has already drained, per run, oldest first — what
  // the real server's GET /runs/{id}/follow-ups (ListConsumedFollowUpInputsForRun) returns.
  private readonly consumedFollowUpsByRun = new Map<string, UserInput[]>();
  /** Issue #1660: the number of GET /runs/{id}/follow-ups reads, per run. */
  readonly followUpReads = new Map<string, number>();
  /** Issue #1660: a raw (status, body) answer for GET /runs/{id}/follow-ups, overriding the
   *  drained-follow-ups default, so a test can model a persistent failure or a malformed 200. */
  private readonly followUpsOverride = new Map<string, { status: number; body: unknown }[]>();
  private stateFailRemaining = 0;
  private stateFailStatus = 503;
  // PRD #1247 fix round E: model an rc.5-shaped api that strict-decodes the unknown
  // claim_generation field on /state with the EXACT "invalid request body" 400.
  private strictDecodeStateRemaining = 0;
  private msgFailRemaining = 0;
  private msgFailStatus = 503;
  private readonly alreadyTerminal = new Set<string>();
  private readonly stateStatusOverride = new Map<string, string>();
  // #1539: runs whose 200 /state ACK OMITS run.status entirely (a "statusless" ack). The real
  // server can 200-ack a report without echoing a run status the client can branch on; the worker
  // then falls through to its next ownership read for the terminal signal. A test arms this to
  // reach handleRecoveryExhausted's terminal-ownership early-return settle branch, which a normal
  // status-bearing ack skips (it settles on the ack itself instead).
  private readonly omitStateAckStatusRuns = new Set<string>();
  // PRD #1190 M2: runs whose running-report ACK carries `pause_requested: true` (the
  // server-decided pause boundary). The real server sets it on the RunDTO the ACK wraps; the
  // worker reads it off the same body (readRunAck). A resumed run whose pause columns survived a
  // requeue answers true here WITHOUT any pause input being delivered.
  private readonly pauseRequestedRuns = new Set<string>();
  // PRD #1247 M5b (MINOR-7): runs whose /state ACK carries a TOP-LEVEL credential_switch signal
  // (the SECONDARY transport beside /inputs). The real server (workerStateAck) sets it when a
  // held-state switch is pending for the run's current claim; a test arms it to prove the state-ack
  // transport triggers the switch through the reportState closure, independent of /inputs.
  private readonly stateAckCredentialSwitch = new Map<string, number>();
  // Issue #1626: runs whose 200 /state ACK carries `run.completion_revision` (the RunDTO's frozen
  // completion-contract revision). The real server reports it once the contract froze (plan
  // approval / the autopilot plan report); the worker reads it off the same body (readRunAck) and
  // binds its completion permit to it when the claim carried no contract_revision.
  private readonly stateAckCompletionRevision = new Map<string, unknown>();
  private readonly stateRawOverride = new Map<
    string,
    { status: number; body: string }
  >();
  private readonly refuseAllStates = new Set<string>();
  // m2 (#1197): fail the FIRST /state report for a run that matches a predicate,
  // leaving every other report handled normally — so a test can knock out ONE
  // specific report (e.g. the autopilot running report carrying plan_md,
  // which persists plan_md via SetRunAutopilotPlan) without also 409ing the ordinary
  // heartbeats the way refuseAllStates does (which would stop the run reaching the
  // gate). Fires once (`fired`), so a retried transient status falls through to the
  // normal handler on the next attempt.
  private readonly stateFailWhen = new Map<
    string,
    {
      matchesState: (body: StateRequest) => boolean;
      httpStatus: number;
      runStatus?: string;
      // PRD #1247 M5b: when set, a 409 refusal carries this TOP-LEVEL disposition (e.g.
      // "stale_claim"), the shape the server produces when a held-state switch released this
      // claim or a reclaim superseded it. readRunAck reads it into StateAck.staleClaim.
      disposition?: string;
      // PRD #1497 M2: when set, the 409 refusal's run DTO carries this hold_reason (e.g.
      // "budget_exhausted"), the shape the server produces on a SERVER-side wall park — paired with
      // runStatus:"paused" + disposition:"stale_claim" it drives the ServerWallParkedError path.
      holdReason?: string;
      // Issue #1626: when set, the 409 refusal's run DTO carries this completion_revision, so a test
      // can prove a STALE ack's revision is never adopted.
      completionRevision?: unknown;
      fired: boolean;
    }
  >();
  // issue #559 M3: the read-only ownership probe (GET /runs/{id}/ownership). A run
  // with no override answers 200 {status:"running"} — the "still ours, keep going"
  // default the skip path proceeds on. An override expresses a terminal status, a
  // 404 not-owned, or a transient 5xx.
  private readonly ownershipByRun = new Map<
    string,
    { httpStatus: number; status?: string; generation?: number }
  >();
  // issue #1319: the owner-scoped orphan-classification read. Keyed by OWNER run id (the
  // fake trusts the test for the claimant/authz; the runner's predicate logic is what's under
  // test). No override for an owner => 404 (fail closed), matching the server's not-found.
  private readonly orphanByRun = new Map<string, { httpStatus: number; identity?: RunOrphanClassificationResponse }>();
  private codexDelayMs = 0;
  private codexResponseOverride: unknown | undefined;

  // --- records -------------------------------------------------------------
  readonly registers: RecordedRegister[] = [];
  heartbeats = 0;
  unauthorized = 0;
  stateAttempts = 0;
  readonly states: Array<{ runId: string; body: StateRequest }> = [];
  // PRD #1247 M5b: a GLOBAL, ordered log of the mutating requests as they land, so a test can pin
  // RELATIVE ORDER across the two endpoints the single `states`/`messageBatches` arrays cannot show
  // (e.g. that a message batch DRAINED before the credential_switch state report). Each accepted
  // /messages batch appends "messages"; each recorded /state report appends `state:<status>`.
  readonly requestLog: string[] = [];
  private readonly stateHooks = new Map<string, (body: StateRequest) => void>();
  // PRD #1247 M5b: the per-batch MessagesRequest wrapper as it landed (the runMatch handler
  // otherwise keeps only the flattened `messages[]`, discarding the top-level claim_generation
  // a capability worker stamps). Additive record; lets a runner test assert the batcher was
  // wired from the claim.
  readonly messageBatches: Array<{ runId: string; claim_generation?: number; count: number }> = [];
  private readonly messagesByRun = new Map<string, OutgoingMessage[]>();
  private readonly seenSeqByRun = new Map<string, Set<number>>();

  // --- chat read surface (PRD #39 M3) --------------------------------------
  private chatRunsList: WorkerRunListItem[] = [];
  private readonly chatRunDetails = new Map<string, WorkerRunDetail>();
  private readonly chatMessagesByRun = new Map<string, WorkerRunMessage[]>();
  readonly chatListLimits: (string | null)[] = [];
  readonly chatMessageQueries: Array<{
    runId: string;
    after: string | null;
    limit: string | null;
  }> = [];
  readonly proposalRequests: Array<{
    runId: string;
    body: Record<string, unknown>;
  }> = [];
  readonly codexRequests: Array<{
    runId: string;
    operation: "release" | "refresh";
    body: Record<string, unknown>;
  }> = [];
  // PRD #1226 M4 (D6): the completion-hold endpoint. Records each request (run + body) and
  // answers a configurable {run:{status}} at a configurable HTTP status — the worker keys its
  // park order off the RETURNED status (paused ⇒ held), reading it off both a 200 and a 409.
  readonly completionHoldRequests: Array<{
    runId: string;
    body: Record<string, unknown>;
  }> = [];
  private completionHoldStatus = "paused";
  private completionHoldHttpStatus = 200;
  // PRD #1497 M2: the wall-park endpoint. Records each request (run + body: head, published,
  // claim_generation) and answers a configurable {run:{status}} at a configurable HTTP status — the
  // worker keys its park order off the RETURNED status (paused ⇒ parked), reading it off both a 200
  // and a 409. A 404/5xx httpStatus makes the client THROW (an UNDELIVERABLE park, D17).
  readonly wallParkRequests: Array<{
    runId: string;
    body: Record<string, unknown>;
  }> = [];
  private wallParkStatus = "paused";
  private wallParkHttpStatus = 200;
  // Issue #1600: the budget_total_seconds / budget_used_seconds the wall-park RunDTO carries.
  private wallParkBudget: { total?: number; used?: number } | undefined;
  // PRD #1226 M4 (D5): the completion-permit endpoint. Records each request (run + body) and answers
  // a configurable decision — {granted:true} by default, or {granted:false, deny_reason} — at a
  // configurable HTTP status (a non-200 models a transport/HTTP error the client THROWS on, distinct
  // from the granted:false denial which is a normal 200 body).
  readonly completionPermitRequests: Array<{
    runId: string;
    body: Record<string, unknown>;
  }> = [];
  private completionPermitGranted = true;
  private completionPermitDenyReason: string | undefined;
  private completionPermitHttpStatus = 200;
  /** Leading permit requests answered with completionPermitHttpStatus before the normal answer
   *  (undefined = every request). Drives the outage-then-recovery retry path. */
  private completionPermitFailTimes: number | undefined;
  // PRD #1392 M2 (D7): the RAW `protocol_features` the register endpoint returns beside
  // worker_id. Left `undefined` ⇒ the response OMITS the field entirely (an older api that
  // returns only {worker_id}). Set to an arbitrary value (an array, a non-array, or an array
  // with non-string entries) so a test can drive register()'s Array.isArray + string-filter guard.
  private registerProtocolFeatures: unknown = undefined;

  constructor(private readonly token: string) {
    this.server = http.createServer((req, res) => {
      this.handle(req, res).catch((err) => {
        res.writeHead(500);
        res.end(String(err));
      });
    });
  }

  async listen(): Promise<string> {
    await new Promise<void>((resolve) =>
      this.server.listen(0, "127.0.0.1", resolve),
    );
    // unref the listening handle so it never keeps the test PROCESS alive on its own.
    // Every test drives this server through an awaited client call, so the run's own
    // pending work holds the loop open while a test is in flight; once the tests
    // finish, the server must not be what keeps the file wrapper from draining.
    // Without this, node's server handle lingered after close() on musl/alpine (CI,
    // node:22-alpine, root) and the whole runner.test.ts file timed out at the
    // per-file --test-timeout — even though every subtest passed (a leaked-handle
    // hang, not a slow test). afterEach still close()s each server; this is the
    // belt-and-braces that makes draining deterministic across platforms.
    //
    // (runner.test.ts was split into runner-*.test.ts on 2026-08-03; the hazard is
    // unchanged and now applies to each of those files, which all share this fake
    // through test/runner-harness.ts. Smaller files make a leaked handle CHEAPER to
    // hit the cap with, not rarer: the per-file budget is spent by whichever file
    // holds the handle.)
    this.server.unref();
    const { port } = this.server.address() as AddressInfo;
    return `http://127.0.0.1:${port}`;
  }

  async close(): Promise<void> {
    await new Promise<void>((resolve, reject) =>
      this.server.close((err) => (err ? reject(err) : resolve())),
    );
  }

  enqueueClaim(claim: ClaimResponse): void {
    this.receiptGeneration.set(claim.run_id, claim.claim_generation ?? 0);
    this.claimQueue.push(claim);
  }

  /** Make the next `times` /state calls fail with `status` before succeeding. */
  failStateNext(times: number, status = 503): void {
    this.stateFailRemaining = times;
    this.stateFailStatus = status;
  }

  /** PRD #1247 fix round E: make the next `times` /state calls answer the EXACT strict-decode 400
   *  ("invalid request body") a rolled-back api produces for the unknown claim_generation field,
   *  then succeed — so a runner test can prove reportState's strip-and-retry survives an rc.5-shaped
   *  decoder. Distinct from failStateNext (whose {error:"injected failure"} body is NOT strict-decode
   *  shaped, so isStrictDecodeError is false and the client would not strip). */
  failStateStrictDecodeNext(times: number): void {
    this.strictDecodeStateRemaining = times;
  }

  /** Make the next `times` /messages calls fail with `status` before succeeding. */
  failMessagesNext(times: number, status = 503): void {
    this.msgFailRemaining = times;
    this.msgFailStatus = status;
  }

  /** Make /state answer 409 for this run's terminal reports (already finalized). */
  markAlreadyTerminal(runId: string): void {
    this.alreadyTerminal.add(runId);
  }

  /**
   * Make /state answer 200 while reporting a DIFFERENT status than the one asked
   * for — the shape the real server produces when it declines a park on one of its
   * designed paths (retry budget exhausted, RUN_LIMIT_MAX_PARK clamp exceeded, or
   * the wait_on_limit=false coercion) and fails the run instead.
   *
   * This is the case an `applied`-keyed implementation gets wrong, because the
   * server applied a transition — just not the requested one — so `applied` is
   * true (PRD #35's park acknowledgement contract). Without this hook the fake can
   * only express "applied" and "409", and a test suite built on those two passes
   * against the broken implementation.
   */
  overrideStateStatus(runId: string, status: string): void {
    this.stateStatusOverride.set(runId, status);
  }

  /** #1539: 200-ack this run's /state reports WITHOUT a run.status field (a statusless ack), so the
   *  client's StateAck.status is undefined and a terminal report's ack-terminal branch is skipped —
   *  the worker falls through to its next ownership read for the terminal signal. */
  omitStateAckStatus(runId: string, omit = true): void {
    if (omit) this.omitStateAckStatusRuns.add(runId);
    else this.omitStateAckStatusRuns.delete(runId);
  }

  /** Answer /state for this run with a verbatim body — for the malformed and
   *  older-server shapes a JSON-typed helper cannot express. */
  sendRawState(runId: string, status: number, body: string): void {
    this.stateRawOverride.set(runId, { status, body });
  }

  /** Refuse EVERY /state report for this run with a 409, whatever its status.
   *  markAlreadyTerminal only fires for terminal reports, so it cannot express a
   *  non-terminal report (limit_wait) racing a server-side cancel — which is
   *  precisely the case PRD #35's park has to survive. */
  /** Refuse every NON-running /state report for this run with a 409 carrying `status:"cancelled"`
   *  (the run moved on under the worker). The initial `running` report still SUCCEEDS, so the run
   *  reaches its park path (recovery_wait / limit_wait) where the refusal is the point — and so the
   *  PRD #1391 Run B M4 phaseClone guard (which stops on a running report refused with a TERMINAL
   *  status) does not short-circuit before the park path under test. */
  refuseStateWith409(runId: string): void {
    this.refuseAllStates.add(runId);
  }

  /**
   * m2 (#1197): make the FIRST /state report for `runId` whose body matches `match`
   * fail, then handle every subsequent report normally. Targets ONE specific report
   * (unlike refuseStateWith409, which refuses every report). Two shapes:
   *  - a 409 (the default) carrying `{ run: { status: runStatus } }` models the run
   *    moving on concurrently — the client reads it as `{ applied: false, status }`.
   *  - any other 4xx (e.g. 400) models a transport/refusal the client throws on
   *    (isTransient/isAlreadyTerminal both false → RequestError propagates).
   */
  failStateWhen(
    runId: string,
    matchesState: (body: StateRequest) => boolean,
    opts: {
      httpStatus?: number;
      runStatus?: string;
      disposition?: string;
      holdReason?: string;
      completionRevision?: unknown;
    } = {},
  ): void {
    this.stateFailWhen.set(runId, {
      matchesState,
      httpStatus: opts.httpStatus ?? 409,
      runStatus: opts.runStatus,
      disposition: opts.disposition,
      holdReason: opts.holdReason,
      completionRevision: opts.completionRevision,
      fired: false,
    });
  }

  /** Issue #1660: answer GET /runs/{id}/follow-ups with this status and body on every read. */
  overrideFollowUps(runId: string, status: number, body: unknown): void {
    this.followUpsOverride.set(runId, [{ status, body }]);
  }

  /** Issue #1660: answer successive GET /runs/{id}/follow-ups reads with these responses in
   *  order; the last one repeats. */
  overrideFollowUpsSequence(runId: string, responses: { status: number; body: unknown }[]): void {
    this.followUpsOverride.set(runId, [...responses]);
  }

  setInputs(runId: string, inputs: UserInput[]): void {
    // Issue #1673: a real row id is never reused, and receipts are keyed by id. Many older
    // fixtures send every input as id 1; a row whose id this run has already seen gets a fresh
    // one (also within one batch), so a later input is a new row rather than a replay.
    const seen = this.seenInputIds.get(runId) ?? new Set<number>();
    inputs = inputs.map((row) => {
      const fresh = seen.has(row.id) ? { ...row, id: this.nextSyntheticInputId++ } : row;
      seen.add(fresh.id);
      return fresh;
    });
    this.seenInputIds.set(runId, seen);
    this.inputsByRun.set(runId, inputs);
  }

  setInputClaimGeneration(runId: string, generation: number): void {
    this.receiptGeneration.set(runId, generation);
  }

  /** Why the run's claim is inactive once its generation moves on (default "stale"). */
  setInputFenceReason(runId: string, reason: "switch_pending" | "released" | "stale"): void {
    this.receiptFenceReason.set(runId, reason);
  }

  /** Answer every `kind` receipt with `status` (undefined restores normal replies). */
  failInputReceipts(kind: "ack" | "applied", status: number | undefined, reason?: string): void {
    if (status === undefined) this.failingReceipts.delete(kind);
    else this.failingReceipts.set(kind, status);
    if (reason === undefined) this.failingReceiptReasons.delete(kind);
    else this.failingReceiptReasons.set(kind, reason);
  }

  /** Hold every `kind` receipt reply for `ms` before answering (0 restores immediate replies). */
  delayInputReceipts(kind: "ack" | "applied", ms: number): void {
    this.delayedReceipts.set(kind, ms);
  }

  /** Issue #1673: a credential switch becomes pending right after this run's next successful
   *  ACK, so the routed batch's APPLIED is refused with 409 reason switch_pending, and GET, fenced
   *  like the server's ConsumeInputs, returns no rows. No switch signal rides the GET, so only the
   *  APPLIED refusal can reveal the switch. */
  pendSwitchAfterNextAck(runId: string, signalGeneration?: number): void {
    this.switchAfterAck.add(runId);
    if (signalGeneration !== undefined) this.switchSignalGeneration.set(runId, signalGeneration);
  }

  /** Issue #1604: a credential switch is pending NOW for the claim at `generation`: GET drains nothing
   *  and carries `credential_switch {generation}` (the server's transport), and APPLIED is refused
   *  switch_pending except for the claim's own ACKed revise_plan rows (D6). The worker's
   *  credential_switch (or credential_switch_failed) report clears it, as the server's release does. */
  requestCredentialSwitch(runId: string, generation: number): void {
    this.switchPendingRuns.add(runId);
    this.switchSignalGeneration.set(runId, generation);
  }

  /** Issue #1604: drop every /state report for this run matching `matches` (the connection is
   *  destroyed before anything is recorded), modelling a worker that dies before the report lands.
   *  Returns a function that stops dropping. */
  dropStatesWhen(runId: string, matches: (body: StateRequest) => boolean): () => void {
    this.droppedStates.set(runId, matches);
    return () => {
      if (this.droppedStates.get(runId) === matches) this.droppedStates.delete(runId);
    };
  }

  /** Issue #1604: withdraw a pending switch (the owner's re-request cleared server-side). */
  clearCredentialSwitch(runId: string): void {
    this.switchPendingRuns.delete(runId);
    this.switchAfterAck.delete(runId);
    this.switchSignalGeneration.delete(runId);
  }

  /** Issue #1604: append rows to the run's input queue (a real server keeps unapplied rows), with
   *  setInputs' fresh-id rule. */
  appendInputs(runId: string, inputs: UserInput[]): void {
    const existing = this.inputsByRun.get(runId) ?? [];
    this.setInputs(runId, inputs);
    this.inputsByRun.set(runId, [...existing, ...(this.inputsByRun.get(runId) ?? [])]);
  }

  /** Issue #1604: the run's input rows as the server holds them (applied or not). */
  inputRows(runId: string): UserInput[] {
    return [...(this.inputsByRun.get(runId) ?? [])];
  }

  isAcked(runId: string, id: number): boolean {
    return this.ackedByRun.get(runId)?.has(id) ?? false;
  }

  isApplied(runId: string, id: number): boolean {
    return this.appliedByRun.get(runId)?.has(id) ?? false;
  }

  /** Issue #1604: answer the next `times` `kind` receipts with `status`, then normally. */
  failInputReceiptsTimes(kind: "ack" | "applied", times: number, status = 503): void {
    this.failingReceiptsRemaining.set(kind, { times, status });
  }

  /** Issue #1604: hold the next `kind` receipt before the fake evaluates it (claim generation and
   *  row state are read only once released), modelling a receipt that lands late. */
  holdNextInputReceipt(kind: "ack" | "applied"): () => void {
    let release!: () => void;
    const held = new Promise<void>((r) => (release = r));
    const holds = this.receiptHolds.get(kind) ?? [];
    holds.push(held);
    this.receiptHolds.set(kind, holds);
    return release;
  }

  /** Issue #1604: answer the next `times` GET /inputs for this run with `status` (Infinity = every
   *  read). A 5xx is transient to the worker. */
  failInputGets(runId: string, times: number, status = 503): void {
    this.inputGetFailures.set(runId, { times, status });
  }

  /** Issue #1604: hold the next `times` GET /inputs for this run `ms` before answering. */
  delayInputGets(runId: string, ms: number, times = 1): void {
    this.inputGetDelays.set(runId, { times, ms });
  }

  /** Issue #1604: answer the next `times` GET /inputs with this 200 body verbatim (a malformed body
   *  is a definitive protocol failure). */
  rawInputGets(runId: string, body: unknown, times = Infinity): void {
    this.inputGetRaw.set(runId, { times, body });
  }

  loseNextInputReceiptReply(kind: "ack" | "applied"): void {
    this.lostReceiptReplies.set(kind, (this.lostReceiptReplies.get(kind) ?? 0) + 1);
  }

  /** PRD #1247 M5b (MINOR-7): arm a TOP-LEVEL credential_switch signal on every /state ACK for a
   *  run, WITHOUT delivering it through /inputs — so a test can prove the state-ack transport trips
   *  the switch on its own. */
  armStateAckCredentialSwitch(runId: string, generation: number): void {
    this.stateAckCredentialSwitch.set(runId, generation);
  }

  /** issue #559 M3: answer the ownership probe for this run with 200 {status}. Use a
   *  terminal status (completed/failed/cancelled) to drive the skip-path terminal throw.
   *  PRD #1391 Run B M4: an optional `generation` rides the 200 as `claim_generation`, so a test
   *  can model the queued-duplicate router seeing a DIFFERENT generation own the run. */
  setOwnershipStatus(runId: string, status: string, generation?: number): void {
    this.ownershipByRun.set(runId, { httpStatus: 200, status, generation });
  }

  /** issue #559 M3: answer the ownership probe with 404 — the DEFINITIVE not-owned
   *  (reclaimed) signal that makes the skip path throw REASON_FOLLOWUP_NOT_PARKED. */
  setOwnershipNotOwned(runId: string): void {
    this.ownershipByRun.set(runId, { httpStatus: 404 });
  }

  /** issue #559 M3: answer the ownership probe with a TRANSIENT error (default 503) —
   *  neither a 404 nor a terminal status, so the skip path logs and PROCEEDS. */
  failOwnership(runId: string, httpStatus = 503): void {
    this.ownershipByRun.set(runId, { httpStatus });
  }

  /** issue #1319: answer the orphan-classification read for OWNER runId with 200 + identity. */
  setOrphanClassification(ownerRunId: string, identity: RunOrphanClassificationResponse): void {
    this.orphanByRun.set(ownerRunId, { httpStatus: 200, identity });
  }
  /** issue #1319: answer with 404 (owner not in this owner+repo scope) — the fail-closed signal. */
  setOrphanNotFound(ownerRunId: string): void {
    this.orphanByRun.set(ownerRunId, { httpStatus: 404 });
  }
  /** issue #1319: answer with a TRANSIENT transport error (default 503). */
  failOrphanClassification(ownerRunId: string, httpStatus = 503): void {
    this.orphanByRun.set(ownerRunId, { httpStatus });
  }

  /** PRD #1190 M2: make this run's running-report ACK carry `pause_requested: true` — the
   *  server-decided pause boundary the worker's loop-top pause branch honours. Set (or clear)
   *  it to drive a run into the pause-park path with no pause input delivered, exactly as a
   *  resumed run whose pause columns survived a requeue would. */
  setPauseRequestedAck(runId: string, requested = true): void {
    if (requested) this.pauseRequestedRuns.add(runId);
    else this.pauseRequestedRuns.delete(runId);
  }

  /** Issue #1626: make this run's 200 /state ACKs carry `run.completion_revision: rev` (the RunDTO's
   *  frozen completion-contract revision). Any value is passed through verbatim so a test can also
   *  model a malformed one. */
  setStateAckCompletionRevision(runId: string, rev: unknown): void {
    this.stateAckCompletionRevision.set(runId, rev);
  }

  /** PRD #1392 M2 (D7): set the raw `protocol_features` the register endpoint returns beside
   *  worker_id. Never called ⇒ the field is omitted (an older api). Accepts an arbitrary value
   *  (a non-array / non-string entries) so a test can exercise register()'s parsing guard. */
  setRegisterProtocolFeatures(features: unknown): void {
    this.registerProtocolFeatures = features;
  }

  /** PRD #1226 M4 (D6): set the {run:{status}} the completion-hold endpoint answers, and the HTTP
   *  status it answers with (default 200; 409 models a refusal the client must NOT throw on). */
  setCompletionHoldResponse(status: string, httpStatus = 200): void {
    this.completionHoldStatus = status;
    this.completionHoldHttpStatus = httpStatus;
  }

  /** PRD #1497 M2: set the {run:{status}} the wall-park endpoint answers, and the HTTP status it
   *  answers with (default 200/"paused" = a landed park; 409 with a non-"paused" status models a
   *  REFUSED park the client must NOT throw on; a 404/5xx models an UNDELIVERABLE report the client
   *  THROWS on, D17). */
  setWallParkResponse(status: string, httpStatus = 200, budget?: { total?: number; used?: number }): void {
    this.wallParkStatus = status;
    this.wallParkHttpStatus = httpStatus;
    this.wallParkBudget = budget;
  }

  /** PRD #1226 M4 (D5): set the completion-permit decision. `granted:true` is the issued permit;
   *  `granted:false` (with an optional `denyReason`) is the NON-TERMINAL denial (a 200 body the
   *  client does NOT throw on); `httpStatus` other than 200 models a transport/HTTP error the client
   *  DOES throw on. */
  setCompletionPermitResponse(
    granted: boolean,
    opts: { denyReason?: string; httpStatus?: number; failTimes?: number } = {},
  ): void {
    this.completionPermitGranted = granted;
    this.completionPermitDenyReason = opts.denyReason;
    this.completionPermitHttpStatus = opts.httpStatus ?? 200;
    this.completionPermitFailTimes = opts.failTimes;
  }

  /** Observe each /state report as it lands, so a test can react to a value the
   *  WORKER minted rather than one the test guessed. PRD #88 needs this: the answer
   *  to a clarification must name the question id the runner generated at the park,
   *  and that id is only knowable from the report itself. */
  onState(runId: string, fn: (body: StateRequest) => void): void {
    this.stateHooks.set(runId, fn);
  }

  setChatRuns(runs: WorkerRunListItem[]): void {
    this.chatRunsList = runs;
  }
  setChatRunDetail(id: string, detail: WorkerRunDetail): void {
    this.chatRunDetails.set(id, detail);
  }
  setChatMessages(id: string, msgs: WorkerRunMessage[]): void {
    this.chatMessagesByRun.set(id, msgs);
  }

  delayCodexResponses(ms: number): void {
    this.codexDelayMs = ms;
  }

  overrideNextCodexResponse(body: unknown): void {
    this.codexResponseOverride = body;
  }

  messages(runId: string): OutgoingMessage[] {
    return this.messagesByRun.get(runId) ?? [];
  }

  // --- routing -------------------------------------------------------------
  private async handle(
    req: http.IncomingMessage,
    res: http.ServerResponse,
  ): Promise<void> {
    if (req.headers.authorization !== `Bearer ${this.token}`) {
      this.unauthorized++;
      return send(res, 401, { error: "unauthorized" });
    }
    const url = new URL(req.url ?? "/", "http://fake");
    const body = await readBody(req);
    const json: Record<string, unknown> = body ? JSON.parse(body) : {};
    const p = url.pathname;

    if (req.method === "POST" && p === "/api/worker/register") {
      const rec: RecordedRegister = {
        name: String(json.name),
        version: String(json.version),
        authorized: true,
      };
      // Only record the key when the worker actually sent one, so an old-style
      // {name,version} register stays byte-for-byte that shape (PRD #18).
      if (json.template !== undefined) rec.template = String(json.template);
      // Capabilities (PRD #83 Q1): recorded only when present, so a daemon-less worker's
      // register wire stays byte-identical. Mirrors the api's accept-and-ignore.
      if (json.capabilities !== undefined)
        rec.capabilities = (json.capabilities as unknown[]).map(String);
      // Protocol capabilities (PRD #1226 M1, D2): recorded only when present, so an old
      // image's register wire stays byte-identical. Mirrors the api's Filter-and-store.
      if (json.protocol_capabilities !== undefined)
        rec.protocol_capabilities = (json.protocol_capabilities as unknown[]).map(String);
      this.registers.push(rec);
      // PRD #1392 M2 (D7): the api advertises its protocol features here. Omit the field unless a
      // test configured one, so an unconfigured register wire stays byte-identical to an older api.
      const registerBody: Record<string, unknown> = { worker_id: randomUUID() };
      if (this.registerProtocolFeatures !== undefined)
        registerBody.protocol_features = this.registerProtocolFeatures;
      return send(res, 200, registerBody);
    }
    if (req.method === "POST" && p === "/api/worker/heartbeat") {
      this.heartbeats++;
      return send(res, 200, {});
    }
    if (req.method === "POST" && p === "/api/worker/runs/claim") {
      const claim = this.claimQueue.shift();
      if (!claim) return sendEmpty(res, 204);
      return send(res, 200, claim);
    }

    const codexMatch = /^\/api\/worker\/runs\/([^/]+)\/codex\/(release|refresh)$/.exec(p);
    if (req.method === "POST" && codexMatch) {
      const runId = codexMatch[1] as string;
      const operation = codexMatch[2] as "release" | "refresh";
      this.codexRequests.push({ runId, operation, body: json });
      if (this.codexDelayMs > 0) {
        await new Promise<void>((resolve) => setTimeout(resolve, this.codexDelayMs));
      }
      if (this.codexResponseOverride !== undefined) {
        const override = this.codexResponseOverride;
        this.codexResponseOverride = undefined;
        return send(res, 200, override);
      }
      if (operation === "release" && runId === "api-key") {
        return send(res, 200, { auth_mode: "api_key", access_token: "api-key-access" });
      }
      const observed = json.observed_generation;
      if (
        operation === "refresh"
        && (typeof observed !== "number" || !Number.isSafeInteger(observed) || observed < 0)
      ) {
        return send(res, 400, { error: "invalid observed_generation" });
      }
      let generation = 3;
      if (operation === "refresh") generation = observed as number + 1;
      return send(res, 200, {
        auth_mode: "subscription",
        access_token: "subscription-access",
        generation,
        chatgpt_account_id: "verified-account",
        chatgpt_plan_type: null,
        ...(operation === "refresh" ? { outcome: "advanced" } : {}),
      });
    }

    // Chat read surface (PRD #39 M3), user-scoped server-side. Records query params
    // and proposal bodies for assertions.
    if (req.method === "GET" && p === "/api/worker/chat/runs") {
      this.chatListLimits.push(url.searchParams.get("limit"));
      return send(res, 200, { runs: this.chatRunsList });
    }
    const chatMsgs = /^\/api\/worker\/chat\/runs\/([^/]+)\/messages$/.exec(p);
    if (req.method === "GET" && chatMsgs) {
      const id = chatMsgs[1] as string;
      this.chatMessageQueries.push({
        runId: id,
        after: url.searchParams.get("after"),
        limit: url.searchParams.get("limit"),
      });
      return send(res, 200, { messages: this.chatMessagesByRun.get(id) ?? [] });
    }
    const chatDetail = /^\/api\/worker\/chat\/runs\/([^/]+)$/.exec(p);
    if (req.method === "GET" && chatDetail) {
      const detail = this.chatRunDetails.get(chatDetail[1] as string);
      if (!detail) return send(res, 404, { error: "run not found" });
      return send(res, 200, { run: detail });
    }
    const propMatch = /^\/api\/worker\/runs\/([^/]+)\/proposals$/.exec(p);
    if (req.method === "POST" && propMatch) {
      const runId = propMatch[1] as string;
      this.proposalRequests.push({ runId, body: json });
      const labels = Array.isArray(json.labels)
        ? (json.labels as string[])
        : [];
      return send(res, 201, {
        proposal: {
          id: "prop-1",
          run_id: runId,
          title: String(json.title ?? ""),
          description: String(json.description ?? ""),
          labels,
          status: "pending",
          created_at: "2026-07-10T00:00:00Z",
        },
      });
    }

    const receiptMatch = /^\/api\/worker\/runs\/([^/]+)\/inputs\/(ack|applied)$/.exec(p);
    if (req.method === "POST" && receiptMatch) {
      const runId = receiptMatch[1]!;
      const kind = receiptMatch[2] as "ack" | "applied";
      const ids = json.ids as number[];
      const generation = json.claim_generation as number;
      this.inputReceiptCalls.push({ runId, kind, ids: [...ids], generation });
      this.timeline.push({ type: "receipt_call", runId, kind, ids: [...(Array.isArray(ids) ? ids : [])], generation });
      const hold = this.receiptHolds.get(kind)?.shift();
      if (hold) await hold;
      const sendReceipt = (status: number, payload: unknown): void => {
        this.timeline.push({ type: "receipt_reply", runId, kind, ids: [...(Array.isArray(ids) ? ids : [])], httpStatus: status });
        send(res, status, payload);
      };
      // A run whose claim generation the test never set (it ran a claim without enqueueClaim)
      // accepts any generation, as that claim is the only one, unless the test asked for strict
      // generations: then an unset one is a fixture error, so a fencing regression cannot hide.
      const current = this.receiptGeneration.get(runId);
      if (current === undefined && this.strictReceiptGenerations)
        return sendReceipt(500, { error: `fake: no claim generation set for run ${runId}` });
      const active = current === undefined || generation === current;
      // The claim's inactive reason, as the server names it (switch_pending | released | stale).
      const reason = active ? undefined : (this.receiptFenceReason.get(runId) ?? "stale");
      const rows = this.inputsByRun.get(runId) ?? [];
      // Issue #1604 (D6): while a switch is pending, the server still accepts APPLIED for the claim's
      // OWN ACKed revise_plan rows (the revision was persisted before the release); anything else
      // is refused switch_pending.
      const ackGens = this.ackGenerationByRun.get(runId);
      const ownReviseOnly =
        active && Array.isArray(ids) && ids.length > 0 &&
        ids.every((id) => rows.some((row) => row.id === id && row.kind === "revise_plan") && ackGens?.get(id) === generation);
      if (kind === "applied" && this.switchPendingRuns.has(runId) && !ownReviseOnly)
        return sendReceipt(409, { error: "input receipt conflicts with claim", reason: "switch_pending" });
      const delay = this.delayedReceipts.get(kind) ?? 0;
      if (delay > 0) await new Promise((r) => setTimeout(r, delay));
      const limited = this.failingReceiptsRemaining.get(kind);
      if (limited && limited.times > 0) {
        limited.times--;
        return sendReceipt(limited.status, { error: "fake: receipt failure" });
      }
      const failing = this.failingReceipts.get(kind);
      if (failing !== undefined)
        return sendReceipt(failing, { error: "fake: receipt failure", reason: this.failingReceiptReasons.get(kind) });
      const acked = this.ackedByRun.get(runId) ?? new Set<number>();
      const applied = this.appliedByRun.get(runId) ?? new Set<number>();
      if (!Array.isArray(ids) || ids.length === 0 || ids.some((id) => !rows.some((row) => row.id === id)))
        return sendReceipt(400, { error: "invalid input ids" });
      if (kind === "ack" && !active && ids.some((id) => !acked.has(id)))
        return sendReceipt(409, { error: "inactive claim", reason });
      // Like the server: a retried applied for rows already applied succeeds even after the
      // claim was fenced; an unapplied row needs the active claim.
      if (kind === "applied" && ids.some((id) => !acked.has(id) || (!active && !applied.has(id))))
        return sendReceipt(409, { error: "inactive or unacked", reason: reason ?? "" });
      if (kind === "ack") {
        for (const id of ids) acked.add(id);
        this.ackedByRun.set(runId, acked);
        if (active) {
          const gens = this.ackGenerationByRun.get(runId) ?? new Map<number, number>();
          for (const id of ids) if (!gens.has(id) || !applied.has(id)) gens.set(id, generation);
          this.ackGenerationByRun.set(runId, gens);
        }
        if (this.switchAfterAck.delete(runId)) this.switchPendingRuns.add(runId);
        // GET /follow-ups returns RECEIVED follow-ups, applied or not (ListConsumedFollowUpInputsForRun).
        const consumed = this.consumedFollowUpsByRun.get(runId) ?? [];
        for (const row of rows.filter((row) => ids.includes(row.id) && row.kind === "follow_up"))
          if (!consumed.some((old) => old.id === row.id)) consumed.push(row);
        consumed.sort((a, b) => a.id - b.id);
        this.consumedFollowUpsByRun.set(runId, consumed);
      } else {
        for (const id of ids) applied.add(id);
        this.appliedByRun.set(runId, applied);
      }
      const remaining = this.lostReceiptReplies.get(kind) ?? 0;
      if (remaining > 0) {
        this.lostReceiptReplies.set(kind, remaining - 1);
        this.timeline.push({ type: "receipt_reply", runId, kind, ids: [...ids], httpStatus: 0 });
        res.destroy();
        return;
      }
      this.inputReceiptReplies.push({ runId, kind });
      return sendReceipt(200, { inputs: rows.filter((row) => ids.includes(row.id)).sort((a, b) => a.id - b.id), active, reason });
    }

    const runMatch =
      /^\/api\/worker\/runs\/([^/]+)\/(messages|state|inputs)$/.exec(p);
    if (runMatch) {
      const runId = runMatch[1] as string;
      const kind = runMatch[2] as string;
      if (req.method === "POST" && kind === "messages")
        return this.handleMessages(res, runId, json);
      if (req.method === "POST" && kind === "state")
        return this.handleState(res, runId, json);
      if (req.method === "GET" && kind === "inputs") {
        this.inputGets.set(runId, (this.inputGets.get(runId) ?? 0) + 1);
        // Issue #1604: delivery-failure injection (delay, transient status, raw body), consumed per read.
        const delay = this.inputGetDelays.get(runId);
        if (delay && delay.times > 0) {
          delay.times--;
          await new Promise((r) => setTimeout(r, delay.ms));
        }
        const failure = this.inputGetFailures.get(runId);
        if (failure && failure.times > 0) {
          failure.times--;
          return send(res, failure.status, { error: "fake: input GET failure" });
        }
        const raw = this.inputGetRaw.get(runId);
        if (raw && raw.times > 0) {
          raw.times--;
          return send(res, 200, raw.body);
        }
        const rows = this.inputsByRun.get(runId) ?? [];
        const applied = this.appliedByRun.get(runId) ?? new Set<number>();
        const switchPending = this.switchPendingRuns.has(runId);
        const pending = switchPending
          ? []
          : rows.filter((row) => !applied.has(row.id)).sort((a, b) => a.id - b.id);
        if (this.legacyConsumeOnRead) {
          // An older api pod: consume on read (mark applied now) and send no receipt marker.
          for (const row of pending) applied.add(row.id);
          this.appliedByRun.set(runId, applied);
          return send(res, 200, { inputs: pending });
        }
        const signal = switchPending ? this.switchSignalGeneration.get(runId) : undefined;
        return send(res, 200, {
          inputs: pending,
          receipts: true,
          ...(signal !== undefined ? { credential_switch: { generation: signal } } : {}),
        });
      }
    }

    // Issue #1660: the read-only rehydrate of a run's already-consumed follow-ups.
    const followUpsMatch = /^\/api\/worker\/runs\/([^/]+)\/follow-ups$/.exec(p);
    if (req.method === "GET" && followUpsMatch) {
      const runId = followUpsMatch[1] as string;
      this.followUpReads.set(runId, (this.followUpReads.get(runId) ?? 0) + 1);
      const queue = this.followUpsOverride.get(runId);
      if (queue && queue.length > 0) {
        const next = queue.length > 1 ? queue.shift()! : queue[0]!;
        return send(res, next.status, next.body);
      }
      return send(res, 200, { inputs: this.consumedFollowUpsByRun.get(runId) ?? [] });
    }

    // issue #559 M3: the read-only ownership probe. Not part of the runMatch
    // alternation above (it is a GET-only read of a distinct segment), so it is
    // routed here. No override ⇒ 200 {status:"running"} (still ours, keep going).
    const ownMatch = /^\/api\/worker\/runs\/([^/]+)\/ownership$/.exec(p);
    if (req.method === "GET" && ownMatch) {
      const runId = ownMatch[1] as string;
      const o = this.ownershipByRun.get(runId);
      if (!o) return send(res, 200, { status: "running" });
      if (o.httpStatus !== 200)
        return send(res, o.httpStatus, { error: "run not found for this worker" });
      // PRD #1391 Run B M4: claim_generation is additive on the probe body; include it only when the
      // test set one, so the default (issue #559) shape stays {status} for unrelated tests.
      const body: Record<string, unknown> = { status: o.status ?? "running" };
      if (o.generation !== undefined) body.claim_generation = o.generation;
      return send(res, 200, body);
    }

    // issue #1319: the owner-scoped orphan-classification read. The `owner` query param is
    // the OWNER run id; the path segment is the CLAIMANT (authz anchor, trusted by the fake).
    const orphanMatch = /^\/api\/worker\/runs\/([^/]+)\/orphan-classification$/.exec(p);
    if (req.method === "GET" && orphanMatch) {
      const owner = url.searchParams.get("owner");
      const o = owner ? this.orphanByRun.get(owner) : undefined;
      // No override, an explicit 404, or a missing owner param => 404 (fail closed).
      if (!o || o.httpStatus === 404) return send(res, 404, { error: "run not found for this worker" });
      if (o.httpStatus !== 200 || !o.identity) return send(res, o.httpStatus, { error: "injected orphan failure" });
      return send(res, 200, o.identity);
    }

    // PRD #1226 M4 (D5): the completion-permit endpoint. Records the request and answers the
    // configured decision. A non-200 httpStatus models a transport/HTTP error (the client throws);
    // otherwise {granted} rides a 200, with deny_reason on a denial.
    const permitMatch = /^\/api\/worker\/runs\/([^/]+)\/completion\/permit$/.exec(p);
    if (req.method === "POST" && permitMatch) {
      const runId = permitMatch[1] as string;
      this.completionPermitRequests.push({ runId, body: json });
      const failing =
        this.completionPermitFailTimes === undefined ||
        this.completionPermitRequests.length <= this.completionPermitFailTimes;
      if (this.completionPermitHttpStatus !== 200 && failing) {
        return send(res, this.completionPermitHttpStatus, { error: "injected permit failure" });
      }
      const body: Record<string, unknown> = { granted: this.completionPermitGranted };
      if (!this.completionPermitGranted && this.completionPermitDenyReason !== undefined) {
        body["deny_reason"] = this.completionPermitDenyReason;
      }
      return send(res, 200, body);
    }

    // PRD #1226 M4 (D6): the completion-hold endpoint. Records the request and answers the
    // configured {run:{status}} at the configured HTTP status (200 landed / 409 refused).
    const holdMatch = /^\/api\/worker\/runs\/([^/]+)\/completion\/hold$/.exec(p);
    if (req.method === "POST" && holdMatch) {
      const runId = holdMatch[1] as string;
      this.completionHoldRequests.push({ runId, body: json });
      return send(res, this.completionHoldHttpStatus, {
        run: { id: runId, status: this.completionHoldStatus },
      });
    }

    // PRD #1497 M2: the wall-park endpoint. Records the request and answers the configured
    // {run:{status}} at the configured HTTP status (200 landed / 409 refused / 404|5xx undeliverable).
    // On an undeliverable status the RunDTO is still shaped but the client throws on the non-200/409.
    const wallParkMatch = /^\/api\/worker\/runs\/([^/]+)\/wall-park$/.exec(p);
    if (req.method === "POST" && wallParkMatch) {
      const runId = wallParkMatch[1] as string;
      this.wallParkRequests.push({ runId, body: json });
      if (this.wallParkHttpStatus !== 200 && this.wallParkHttpStatus !== 409) {
        return send(res, this.wallParkHttpStatus, { error: "run not found for this worker" });
      }
      const run: Record<string, unknown> = { id: runId, status: this.wallParkStatus };
      if (this.wallParkBudget?.total !== undefined) run.budget_total_seconds = this.wallParkBudget.total;
      if (this.wallParkBudget?.used !== undefined) run.budget_used_seconds = this.wallParkBudget.used;
      return send(res, this.wallParkHttpStatus, { run });
    }

    return send(res, 404, { error: "not found", path: p });
  }

  private handleMessages(
    res: http.ServerResponse,
    runId: string,
    json: Record<string, unknown>,
  ): void {
    if (this.msgFailRemaining > 0) {
      this.msgFailRemaining--;
      return send(res, this.msgFailStatus, { error: "injected failure" });
    }
    const incoming = (json.messages ?? []) as OutgoingMessage[];
    // PRD #1247 M5b: record the wrapper as it arrived, so a test can assert claim_generation was
    // stamped (or omitted). Only when the batch has messages — an empty post is a client no-op.
    if (incoming.length > 0) {
      this.requestLog.push("messages");
      this.messageBatches.push({
        runId,
        claim_generation:
          typeof json.claim_generation === "number" ? json.claim_generation : undefined,
        count: incoming.length,
      });
    }
    const list = this.messagesByRun.get(runId) ?? [];
    const seen = this.seenSeqByRun.get(runId) ?? new Set<number>();
    for (const m of incoming) {
      if (seen.has(m.seq)) continue; // server is idempotent on (run_id, seq)
      seen.add(m.seq);
      list.push(m);
    }
    this.messagesByRun.set(runId, list);
    this.seenSeqByRun.set(runId, seen);
    send(res, 200, { accepted: incoming.length });
  }

  private handleState(
    res: http.ServerResponse,
    runId: string,
    json: Record<string, unknown>,
  ): void {
    this.stateAttempts++;
    if (this.strictDecodeStateRemaining > 0) {
      this.strictDecodeStateRemaining--;
      // The exact 400 shape httpx.DecodeJSON (DisallowUnknownFields) produces for an unknown field.
      return send(res, 400, { error: "invalid request body" });
    }
    if (this.stateFailRemaining > 0) {
      this.stateFailRemaining--;
      return send(res, this.stateFailStatus, { error: "injected failure" });
    }
    const raw = this.stateRawOverride.get(runId);
    if (raw) {
      res.writeHead(raw.status, { "Content-Type": "application/json" });
      res.end(raw.body);
      return;
    }
    const body = json as unknown as StateRequest;
    if (this.droppedStates.get(runId)?.(body)) {
      res.destroy();
      return;
    }
    // The run moved on under the worker: refuse the PARK report (recovery_wait / limit_wait / a
    // terminal report) with a 409 carrying the run's real (cancelled) status. The initial `running`
    // report is left to SUCCEED — modelling the realistic sequence (the run was running, then got
    // cancelled concurrently, and only the later park report is refused), which is also what keeps
    // PRD #1391 Run B M4's phaseClone first-running-ack guard from stopping the flight before the
    // park path this refusal is exercising.
    if (this.refuseAllStates.has(runId) && body.status !== "running") {
      return send(res, 409, {
        error: "run already terminal",
        run: { id: runId, status: "cancelled" },
      });
    }
    // m2 (#1197): a per-run predicate can knock out ONE matching report (see
    // failStateWhen). Checked BEFORE recording, so a refused report is not applied —
    // matching the real server, which does not persist a report it declines.
    const when = this.stateFailWhen.get(runId);
    // This is a typed test predicate, not String.match or a dynamic regex.
    // Name verified against CodeQL js/regex-injection's false match, 2026-09-08.
    if (when && !when.fired && when.matchesState(body)) {
      when.fired = true;
      if (when.httpStatus === 409) {
        return send(res, 409, {
          error: "run already moved on",
          run: {
            id: runId,
            status: when.runStatus ?? "cancelled",
            // PRD #1497 M2: a SERVER-side wall park's run DTO carries hold_reason:"budget_exhausted"
            // beside status:"paused"; the reportState closure reads the pair (+ disposition) to throw
            // ServerWallParkedError. Absent unless armed, so existing stale_claim tests are unchanged.
            ...(when.holdReason ? { hold_reason: when.holdReason } : {}),
            ...(when.completionRevision !== undefined ? { completion_revision: when.completionRevision } : {}),
          },
          // PRD #1247 M5b: a stale_claim (or other) disposition rides TOP-LEVEL when armed.
          ...(when.disposition ? { disposition: when.disposition } : {}),
        });
      }
      return send(res, when.httpStatus, { error: "injected state failure" });
    }
    const terminal = body.status === "completed" || body.status === "failed";
    // Both answers carry `{"run": {...}}` because BOTH real handlers do
    // (handler/worker_protocol.go, the 409 at :483 and the 200 at :486). This fake
    // used to answer `{}` and `{"error": ...}`, which was harmless while the client
    // discarded the body and became a lie the moment PRD #35 made the run's real
    // status load-bearing — the exact "two lenient fakes" drift the claim wire
    // contract exists to prevent, one endpoint over.
    if (terminal && this.alreadyTerminal.has(runId)) {
      return send(res, 409, {
        error: "run already terminal",
        run: { id: runId, status: "cancelled" },
      });
    }
    // Issue #1604: SetRunRecoveryWait applies only to a `running` run (opt-in).
    const lastStatus = this.lastRecordedStatus.get(runId);
    if (
      this.recoveryWaitRequiresRunning &&
      body.status === "recovery_wait" &&
      lastStatus !== undefined &&
      lastStatus !== "running" &&
      lastStatus !== "recovery_wait"
    ) {
      return send(res, 409, { error: "run is not running", run: { id: runId, status: lastStatus } });
    }
    this.states.push({ runId, body });
    this.requestLog.push(`state:${body.status}`);
    this.lastRecordedStatus.set(runId, body.status);
    this.timeline.push({ type: "state", runId, status: body.status, ...(body.plan_md !== undefined ? { plan_md: body.plan_md } : {}) });
    // Issue #1604: the server's release (or give-up clear) ends a pending switch.
    // A release also fences the released claim: its later receipts are refused as `released`.
    if (body.status === "credential_switch" || body.status === "credential_switch_failed") {
      this.switchPendingRuns.delete(runId);
      this.switchSignalGeneration.delete(runId);
    }
    if (body.status === "credential_switch") {
      const gen = this.receiptGeneration.get(runId);
      if (gen !== undefined) this.receiptGeneration.set(runId, gen + 1);
      this.receiptFenceReason.set(runId, "released");
    }
    // Issue #1604 (D1b): the plan_rejected `failed` transition settles the run's unapplied
    // reject_plan rows atomically with the transition (only when the transition applies).
    if (body.status === "failed" && body.fail_origin === "plan_rejected") {
      const applied = this.appliedByRun.get(runId) ?? new Set<number>();
      for (const row of this.inputsByRun.get(runId) ?? []) if (row.kind === "reject_plan") applied.add(row.id);
      this.appliedByRun.set(runId, applied);
    }
    this.stateHooks.get(runId)?.(body);
    send(res, 200, {
      run: {
        id: runId,
        // #1539: a statusless ack OMITS run.status entirely (undefined, not "") so readRunAck leaves
        // StateAck.status undefined — the client cannot branch on it and falls through to ownership.
        ...(this.omitStateAckStatusRuns.has(runId)
          ? {}
          : { status: this.stateStatusOverride.get(runId) ?? body.status }),
        // PRD #1190 M2: the server-decided pause boundary rides the RunDTO the ACK wraps. Only
        // present when a test armed it; otherwise absent (the worker reads it as "no pause").
        ...(this.pauseRequestedRuns.has(runId) ? { pause_requested: true } : {}),
        // Issue #1626: the frozen completion-contract revision, only when a test armed it.
        ...(this.stateAckCompletionRevision.has(runId)
          ? { completion_revision: this.stateAckCompletionRevision.get(runId) }
          : {}),
      },
      // PRD #1247 M5b (BLOCKING-2): mirror the real server — an APPLIED credential_switch RELEASE
      // (a 200) carries disposition:"released", which the worker reads to accept the release
      // regardless of the run's returned status (a fresh 'queued' OR an idempotent-after-reclaim
      // 'running' set via overrideStateStatus). enterCredentialSwitch keys off this, not status.
      ...(body.status === "credential_switch" ? { disposition: "released" } : {}),
      // PRD #1247 M5b (MINOR-7): the TOP-LEVEL credential_switch signal the reportState closure feeds
      // to the steering channel (armStateAckCredentialSwitch). Present on the ordinary report acks,
      // not the credential_switch release report itself (which already carries disposition:released).
      ...(this.stateAckCredentialSwitch.has(runId) && body.status !== "credential_switch"
        ? { credential_switch: { generation: this.stateAckCredentialSwitch.get(runId)! } }
        : {}),
    });
  }
}

function readBody(req: http.IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    let data = "";
    req.on("data", (chunk) => (data += chunk));
    req.on("end", () => resolve(data));
    req.on("error", reject);
  });
}

function send(res: http.ServerResponse, status: number, body: unknown): void {
  const payload = JSON.stringify(body);
  res.writeHead(status, { "Content-Type": "application/json" });
  res.end(payload);
}

function sendEmpty(res: http.ServerResponse, status: number): void {
  res.writeHead(status);
  res.end();
}
