import http from "node:http";
import { createHash, randomUUID } from "node:crypto";
import type { AddressInfo } from "node:net";
import type { FakePrDescApi, PrDescOp } from "./fake-pr-desc-api.js";
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

/** Issue #1673/#1604: the input receipt routes (POST /inputs/{ack,applied,discarded}). */
type ReceiptKind = "ack" | "applied" | "discarded";

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
  /** Issue #2230: retain handler exceptions behind HTTP 500 for failure-only diagnostics. */
  readonly handlerExceptions: Array<{ method: string; path: string; error: string }> = [];

  // --- control -------------------------------------------------------------
  private readonly claimQueue: ClaimResponse[] = [];
  private readonly inputsByRun = new Map<string, UserInput[]>();
  private readonly ackedByRun = new Map<string, Set<number>>();
  private readonly appliedByRun = new Map<string, Set<number>>();
  private readonly receiptGeneration = new Map<string, number>();
  private readonly lostReceiptReplies = new Map<string, number>();
  private readonly seenInputIds = new Map<string, Set<number>>();
  private readonly receiptFenceReason = new Map<string, string>();
  private readonly failingReceipts = new Map<ReceiptKind, number>();
  private readonly failingReceiptReasons = new Map<ReceiptKind, string>();
  /** Runs whose credential switch becomes pending right after the next successful ACK. */
  private readonly switchAfterAck = new Set<string>();
  /** Runs with a pending switch: GET drains nothing (the server fence) and APPLIED is refused. */
  private readonly switchPendingRuns = new Set<string>();
  private readonly delayedReceipts = new Map<ReceiptKind, number>();
  /** Issue #1604: a pending switch whose `credential_switch {generation}` signal rides every GET
   *  (the real server's transport). Absent keeps the #1673 shape: no signal on the GET. */
  private readonly switchSignalGeneration = new Map<string, number>();
  /** Issue #1604: the claim generation that ACKed each input id, per run, so the server's
   *  own-revise APPLIED exemption under switch_pending (D6) can be mirrored. */
  private readonly ackGenerationByRun = new Map<string, Map<number, number>>();
  /** Issue #1604: count-limited receipt failures (consumed before the persistent failInputReceipts). */
  private readonly failingReceiptsRemaining = new Map<ReceiptKind, { times: number; status: number }>();
  /** Issue #1604: receipts held BEFORE evaluation until the test releases them (a late receipt). */
  private readonly receiptHolds = new Map<ReceiptKind, Promise<void>[]>();
  /** Issue #1604: GET /inputs failure, delay and raw-body injection, per run. */
  private readonly inputGetFailures = new Map<string, { times: number; status: number }>();
  private readonly inputGetDelays = new Map<string, { times: number; ms: number; after: number }>();
  /** Test-only event barrier for one selected GET, after a requested read count. */
  private readonly inputGetHolds = new Map<string, { after: number; enter: () => void; released: Promise<void> }>();
  private readonly inputGetRaw = new Map<string, { times: number; body: unknown }>();
  /** Issue #1604: GET /inputs reads per run (answered or not), so a test can prove polling continued. */
  readonly inputGets = new Map<string, number>();
  /** GET response send attempts; a held GET has one more entry than send attempt. */
  readonly inputGetSendAttempts = new Map<string, number>();
  /** Issue #1604: /state reports this run drops (connection destroyed, nothing recorded). */
  private readonly droppedStates = new Map<string, { matches: (body: StateRequest) => boolean; beforeDrop?: Promise<void> }>();
  /** Issue #1604: the created_at of each recorded `plan` run_message, with its plan_md, per run. */
  private readonly planFramesByRun = new Map<string, Array<{ at: string; plan_md: unknown }>>();
  /** Issue #1604: runs.stop_kind as CreateStopVerdictInput stamps it (last write wins): a
   *  reject_plan input stamps 'plan_rejected', a cancel 'cancelled'. */
  private readonly stopKindByRun = new Map<string, string>();
  /** Issue #1604: a credential_switch signal for another claim generation that rides every GET
   *  WITHOUT fencing it (the rows still drain): a superseded claim's switch. */
  private readonly foreignSwitchSignal = new Map<string, number>();
  /** Issue #1604: stamp created_at on input rows as the real server does. Off by default so the
   *  wire-shape tests that deep-compare rows keep their exact fixtures; the #1604 interruption
   *  suite turns it on (resume_plan_at is compared against it). */
  stampInputCreatedAt = false;
  /** Issue #1604: omit the claim's resume_plan_at, as the server does when it cannot name the
   *  persisted plan's frame (a query error, a frame tombstoned above the batcher cap, a redaction
   *  mismatch between the frame and plan_md). */
  omitResumePlanAt = false;
  /** Issue #1604: the last issued timestamp, in nanoseconds (always a whole microsecond); every
   *  stamp is strictly later. */
  private lastStampNs = 0n;
  /** Issue #1604: the last recorded /state status per run, the fake's view of the run row. */
  private readonly lastRecordedStatus = new Map<string, string>();
  /** Issue #1604: mirror SetRunRecoveryWait's guard: a recovery_wait report applies only while the
   *  run is `running`; otherwise it is refused 409 with the run's real status. Off by default. */
  recoveryWaitRequiresRunning = false;
  /** Issue #1604: one ordered log of state records and receipt calls/replies, per request, so a test
   *  can pin the order of an APPLIED against the report that made it due. */
  readonly timeline: Array<
    | { type: "state"; runId: string; status: string; plan_md?: string }
    | { type: "receipt_call"; runId: string; kind: ReceiptKind; ids: number[]; generation: number }
    | { type: "receipt_reply"; runId: string; kind: ReceiptKind; ids: number[]; httpStatus: number }
  > = [];
  /** Issue #1673: receipt tests set this so a receipt for a run with no explicit claim
   *  generation fails loudly instead of being accepted as the only claim. */
  strictReceiptGenerations = false;
  /** Issue #1673: answer GET /inputs like an older api pod: consume on read, no receipt marker. */
  legacyConsumeOnRead = false;
  /** Issue #1604: answer POST /inputs/discarded with an untyped 404, like an api that predates it. */
  discardRouteMissing = false;
  /** Issue #1800: answer POST /inputs/included with an untyped 404, like an api that predates it. */
  inclusionRouteMissing = false;
  /** Issue #1800: answer POST /inputs/included like an api that does not own the run: a TYPED 404
   *  ({"error":"run not found","reason":"stale"}), unlike the untyped one of a missing route. */
  inclusionNotOwned = false;
  /** Issue #1800: fail the next `times` POST /inputs/included with this HTTP status. */
  failingInclusions: { status: number; times: number } | undefined = undefined;
  /** Issue #1800: when set, GET /follow-ups rows carry `inclusion_reported: true` and, once a row is
   *  included, `included_at`, as an api that tracks inclusion does. Off, rows are served as before. */
  inclusionTracking = false;
  /** Issue #1800: every POST /inputs/included the api received, in arrival order. */
  readonly inclusionCalls: Array<{ runId: string; ids: number[]; generation: number }> = [];
  private readonly includedByRun = new Map<string, Set<number>>();
  /** Issue #1800: whether POST /inputs/included stamped input `id` of `runId`. */
  isInputIncluded(runId: string, id: number): boolean {
    return this.includedByRun.get(runId)?.has(id) ?? false;
  }
  /** Issue #1604: the approve_plan rows settled through /inputs/discarded, per run. */
  private readonly discardedByRun = new Map<string, Set<number>>();
  /** Issue #1604: the most rows one GET /inputs returns (the server's ListReplayRunInputs LIMIT);
   *  unset returns every pending row. */
  inputPageSize: number | undefined = undefined;
  private nextSyntheticInputId = 1_000_000;
  readonly inputReceiptCalls: Array<{ runId: string; kind: ReceiptKind; ids: number[]; generation: number }> = [];
  /** Receipts answered 200, in reply order (a delayed reply lands here only once it is sent). */
  readonly inputReceiptReplies: Array<{ runId: string; kind: ReceiptKind }> = [];
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
  // Issue #1514: the operator scope ceiling + completed ids the /state ACK's RunDTO carries.
  private readonly stateAckScope = new Map<string, { scope_ceiling: number; milestones_completed: string[] }>();
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

  /** Focused lead-side cross-check transport seam; absent keeps existing fake routes unchanged. */
  crossCheckHandler?: (request: { runId: string; method: string; body: Record<string, unknown> }) =>
    Promise<{ status: number; body: unknown; drop?: boolean }> | { status: number; body: unknown; drop?: boolean };
  /** Hold a pending status response until the owning client's HTTP request aborts. */
  holdCrossCheckStatusUntilAbort?: (runId: string) => boolean;
  abortedHeldCrossCheckStatuses = 0;
  readonly crossCheckRequests: { runId: string; method: string; body: Record<string, unknown> }[] = [];
  readonly crossCheckReplies: { runId: string; method: string; status: number; acceptedCandidate: boolean; dropped: boolean }[] = [];
  usageHandler?: () => { status: number; body: unknown };
  /** Focused receipt fixture: hold a message response until its request is aborted. */
  holdMessagesUntilAbort?: (runId: string, body: Record<string, unknown>) => boolean;
  abortedHeldMessages = 0;
  readonly usageRequests: Record<string, unknown>[] = [];
  checkedTransport = false;
  private readonly checkedFencedRuns = new Set<string>();

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

  // --- PRD #1795: gate revisions ------------------------------------------------------------
  /** Model a revision-allocating api's awaiting_approval report (PRD #1795 M1): a new
   *  presentation id allocates the next revision, the current id with an unchanged payload is an
   *  idempotent retry, a historical id / changed payload / stale adoption is refused 409 with a
   *  reason, and an id-less report allocates. The applied ACK carries top-level gate_revision.
   *  Off by default, so every existing test sees an older api's exact wire. */
  gateRevisions = false;
  /** Stamp the approve/reject/revise rows set through setInputs/appendInputs like the api's M2
   *  verdict inserts: bound(revision) while the run is awaiting_approval at a revision above 0,
   *  no binding (legacy) at revision 0, unbound in any other status. A row that already carries
   *  gate_binding or gate_revision is left as the test wrote it. */
  stampGateBindings = false;
  /** RUN_GATE_REFUSAL_MAX (0 = unlimited): the refusal that would push a run's count past it
   *  fails the run (the 409 then carries run.status "failed"). Counted once per claim generation. */
  gateRefusalMax = 0;
  /** Every refused awaiting_approval report, in order. */
  readonly gateRefusals: Array<{ runId: string; reason: string; generation: number | undefined }> = [];
  private readonly gateByRun = new Map<string, FakeGate>();
  /** PRD #1795: one-shot unrecorded answers with a gate revision (see answerStateOnce). */
  private readonly stateAnswerOnce = new Map<
    string,
    { matches: (body: StateRequest) => boolean; httpStatus: number; runStatus: string; gateRevision: number }
  >();
  /** Reports matching a hook are persisted (recorded, revision allocated) and THEN held or dropped:
   *  "drop" destroys the connection after persistence (a lost ACK), a promise holds the ACK until it
   *  settles (the window between persistence and the response). One-shot. */
  private readonly afterPersistHooks = new Map<string, { matches: (body: StateRequest) => boolean; action: "drop" | Promise<void> }>();
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
  private wallParkLoseFirstReply = false;
  private wallParkCommitted = false;
  readonly ownershipRequests: string[] = [];
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

  /** PRD #1798 M6: the in-memory pr-description api (fake-pr-desc-api.ts); unset ⇒ the routes 404. */
  prDescription: FakePrDescApi | undefined;

  /** Issue #2083: the decisions-memo routes. `get` is the GET answer (unset ⇒ 404, like an api
   *  that predates the feature); `postStatus` is the POST status (204 = saved). Every call is
   *  recorded so a test can assert exactly what the worker read and saved. */
  decisionsMemo: { get?: { status: number; body: unknown }; postStatus: number } = { postStatus: 204 };
  readonly decisionsMemoGets: Array<{ runId: string; claimGeneration: string | null }> = [];
  readonly decisionsMemoPosts: Array<{ runId: string; body: { claim_generation?: number; body?: string } }> = [];

  constructor(private readonly token: string) {
    this.server = http.createServer((req, res) => {
      this.handle(req, res).catch((err) => {
        this.handlerExceptions.push({
          method: req.method ?? "<absent>",
          path: (req.url ?? "/").split("?", 1)[0]!,
          error: err instanceof Error ? err.stack ?? err.message : String(err),
        });
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
    // (runner.test.ts was split into runner-*.test.ts on 2026-08-03; the hazard
    // applies to each of those files, which all share this fake through
    // test/runner-harness.ts. On current node (24 in CI, v26.8 locally) the cap binds
    // each test, not the file, so a leaked handle is worse than it was: the file never
    // exits and --test-timeout does not end it (measured 2026-09-29: a passing test
    // that leaves a listening server open hangs its file until killed from outside).)
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
      // Issue #1604: the server stamps created_at on insert, and CreateStopVerdictInput stamps
      // runs.stop_kind for a reject_plan or cancel in the same statement.
      if (fresh.kind === "reject_plan") this.stopKindByRun.set(runId, "plan_rejected");
      if (fresh.kind === "cancel") this.stopKindByRun.set(runId, "cancelled");
      const bound = this.stampBinding(runId, fresh);
      return this.stampInputCreatedAt && bound.created_at === undefined ? { ...bound, created_at: this.stamp() } : bound;
    });
    this.seenInputIds.set(runId, seen);
    this.inputsByRun.set(runId, inputs);
  }

  /** PRD #1795: the api's M2 binding for a verdict row inserted now (see stampGateBindings). */
  private stampBinding(runId: string, row: UserInput): UserInput {
    if (!this.stampGateBindings) return row;
    if (row.kind !== "approve_plan" && row.kind !== "reject_plan" && row.kind !== "revise_plan") return row;
    if (row.gate_binding !== undefined || row.gate_revision !== undefined) return row;
    const revision = this.gateByRun.get(runId)?.revision ?? 0;
    if (this.lastRecordedStatus.get(runId) !== "awaiting_approval") return { ...row, gate_binding: "unbound" };
    return revision > 0 ? { ...row, gate_binding: "bound", gate_revision: revision } : row;
  }

  /** Issue #1604: a server timestamp (RFC 3339, microsecond precision like Postgres' timestamptz,
   *  UTC), strictly increasing across the fake, like created_at on rows written in order. */
  private stamp(): string {
    let ns = BigInt(Date.now()) * 1_000_000n;
    if (ns <= this.lastStampNs) ns = this.lastStampNs + 1_000n;
    this.lastStampNs = ns;
    const iso = new Date(Number(ns / 1_000_000n)).toISOString();
    return `${iso.slice(0, 19)}.${((ns % 1_000_000_000n) / 1_000n).toString().padStart(6, "0")}Z`;
  }

  /** Issue #1604: the claim's resume_plan_at for this run: the created_at of the latest `plan`
   *  run_message that carries the persisted plan (the last applied awaiting_approval report's
   *  plan_md); undefined with none, or while omitResumePlanAt is set. */
  resumePlanAt(runId: string): string | undefined {
    if (this.omitResumePlanAt) return undefined;
    const frames = this.planFramesByRun.get(runId) ?? [];
    const persisted = this.states.filter((s) => s.runId === runId && s.body.status === "awaiting_approval").at(-1)?.body.plan_md;
    const matching = persisted === undefined ? [] : frames.filter((f) => f.plan_md === persisted);
    // Like the server (LatestPersistedPlanFrameAtForRun): only a frame matching the persisted
    // plan counts; with none the field is omitted, never the latest frame.
    return matching.at(-1)?.at;
  }

  /** Issue #1604: a credential_switch signal for `generation` rides every GET without fencing it
   *  (undefined withdraws it). */
  signalForeignCredentialSwitch(runId: string, generation: number | undefined): void {
    if (generation === undefined) this.foreignSwitchSignal.delete(runId);
    else this.foreignSwitchSignal.set(runId, generation);
  }

  setInputClaimGeneration(runId: string, generation: number): void {
    this.receiptGeneration.set(runId, generation);
  }

  /** Why the run's claim is inactive once its generation moves on (default "stale"). */
  setInputFenceReason(runId: string, reason: "switch_pending" | "released" | "stale"): void {
    this.receiptFenceReason.set(runId, reason);
  }

  /** Answer every `kind` receipt with `status` (undefined restores normal replies). */
  failInputReceipts(kind: ReceiptKind, status: number | undefined, reason?: string): void {
    if (status === undefined) this.failingReceipts.delete(kind);
    else this.failingReceipts.set(kind, status);
    if (reason === undefined) this.failingReceiptReasons.delete(kind);
    else this.failingReceiptReasons.set(kind, reason);
  }

  /** Hold every `kind` receipt reply for `ms` before answering (0 restores immediate replies). */
  delayInputReceipts(kind: ReceiptKind, ms: number): void {
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
   *  An optional hold delays destruction of matching requests without committing them.
   *  Returns a function that stops dropping future requests; already matched requests still drop. */
  dropStatesWhen(runId: string, matches: (body: StateRequest) => boolean, beforeDrop?: Promise<void>): () => void {
    const drop = { matches, beforeDrop };
    this.droppedStates.set(runId, drop);
    return () => {
      if (this.droppedStates.get(runId) === drop) this.droppedStates.delete(runId);
    };
  }

  /** PRD #1795: persist the next /state report for this run matching `matches`, then (instead of
   *  answering at once) destroy the connection ("drop": a lost ACK the client retries) or hold the
   *  answer until `hold` settles (a verdict created in that window is stamped against the
   *  just-persisted gate). One-shot. The dropStatesWhen hook drops BEFORE anything is recorded. */
  afterPersistState(runId: string, matches: (body: StateRequest) => boolean, action: "drop" | Promise<void>): void {
    this.afterPersistHooks.set(runId, { matches, action });
  }

  /** PRD #1795: answer the next /state report for this run matching `matches` with `httpStatus`
   *  and a `{run: {status: runStatus}, gate_revision}` body WITHOUT recording it or touching the
   *  modelled gate: an ACK that carries a gate revision yet does not say the gate was published (a
   *  409, or a 200 whose run is not awaiting_approval). One-shot; `matches` runs when the report
   *  arrives, so a test can create rows at exactly that moment. */
  answerStateOnce(
    runId: string,
    matches: (body: StateRequest) => boolean,
    answer: { httpStatus: number; runStatus: string; gateRevision: number },
  ): void {
    this.stateAnswerOnce.set(runId, { matches, ...answer });
  }

  /** PRD #1795: seed the run's gate as an api would hold it before this worker's claim (a gate an
   *  earlier worker published). `presentationId` null is an id-less gate (an old worker, or a gate
   *  published before the migration); `historical` ids are recorded as earlier presentations. */
  seedGate(runId: string, gate: { revision: number; presentationId: string | null; payload: FakeGatePayload; historical?: string[] }): void {
    const presentations = new Map<string, number>();
    (gate.historical ?? []).forEach((id, i) => presentations.set(id, i + 1));
    if (gate.presentationId !== null) presentations.set(gate.presentationId, gate.revision);
    this.gateByRun.set(runId, {
      revision: gate.revision,
      currentId: gate.presentationId,
      presentations,
      snapshot: normalizePayload(gate.payload, undefined),
      countedGeneration: undefined,
      refusals: 0,
    });
  }

  /** PRD #1795: the run's current gate revision and presentation id (0 / null before any gate). */
  gateOf(runId: string): { revision: number; presentationId: string | null } {
    const g = this.gateByRun.get(runId);
    return { revision: g?.revision ?? 0, presentationId: g?.currentId ?? null };
  }

  /** PRD #1795: the gate-resume claim fields the api assembles for an awaiting_approval resume:
   *  the current revision and id, and the requirement half of the IMMUTABLE presented snapshot. */
  gateResumeFields(runId: string): Partial<ClaimResponse> {
    const g = this.gateByRun.get(runId);
    if (!g || g.revision <= 0) return {};
    return {
      resume_gate_revision: g.revision,
      ...(g.currentId !== null ? { resume_gate_presentation_id: g.currentId } : {}),
      resume_gate_presented: {
        required_capabilities: g.snapshot.required_capabilities,
        required_tools: g.snapshot.required_tools,
        ...(g.snapshot.size_class !== undefined ? { size_class: g.snapshot.size_class } : {}),
      },
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

  /** Issue #1604: GetRunClaimContext's human_plan_approved: ANY applied approve_plan row of the run,
   *  whichever plan it was sent against. The claim's plan_approved is true when it is (the service
   *  ORs it with auto_approve), and resumePhaseFor then answers "implementing". */
  humanPlanApproved(runId: string): boolean {
    const applied = this.appliedByRun.get(runId);
    const discarded = this.discardedByRun.get(runId);
    return (this.inputsByRun.get(runId) ?? []).some(
      (row) => row.kind === "approve_plan" && (applied?.has(row.id) ?? false) && !(discarded?.has(row.id) ?? false),
    );
  }

  /** Issue #1604: the row was settled through POST /inputs/discarded (disposition 'superseded'). */
  isDiscarded(runId: string, id: number): boolean {
    return this.discardedByRun.get(runId)?.has(id) ?? false;
  }

  /** Issue #1604: answer the next `times` `kind` receipts with `status`, then normally. */
  failInputReceiptsTimes(kind: ReceiptKind, times: number, status = 503): void {
    this.failingReceiptsRemaining.set(kind, { times, status });
  }

  /** Issue #1604: hold the next `kind` receipt before the fake evaluates it (claim generation and
   *  row state are read only once released), modelling a receipt that lands late. */
  holdNextInputReceipt(kind: ReceiptKind): () => void {
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

  /** Issue #1604: hold the next `times` GET /inputs for this run `ms` before answering, once the
   *  run has had more than `afterReads` reads in all. */
  delayInputGets(runId: string, ms: number, times = 1, afterReads = 0): void {
    this.inputGetDelays.set(runId, { times, ms, after: afterReads });
  }

  /** Hold one GET until the test has observed its entry and released it. */
  holdNextInputGet(runId: string, afterReads = 0): { entered: Promise<void>; release: () => void } {
    let enter!: () => void;
    let release!: () => void;
    const entered = new Promise<void>((resolve) => { enter = resolve; });
    const released = new Promise<void>((resolve) => { release = resolve; });
    this.inputGetHolds.set(runId, { after: afterReads, enter, released });
    return { entered, release };
  }

  /** Issue #1604: answer the next `times` GET /inputs with this 200 body verbatim (a malformed body
   *  is a definitive protocol failure). */
  rawInputGets(runId: string, body: unknown, times = Infinity): void {
    this.inputGetRaw.set(runId, { times, body });
  }

  loseNextInputReceiptReply(kind: ReceiptKind): void {
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

  /** Issue #1514: make this run's 200 /state ACKs carry `run.scope_ceiling` and
   *  `run.milestones_completed`, as the api serves them after a scope directive. */
  setStateAckScope(runId: string, scope: { scope_ceiling: number; milestones_completed: string[] }): void {
    this.stateAckScope.set(runId, scope);
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

  /** Commit the first park, lose its reply, then answer the identical retry as already paused. */
  commitWallParkThenLoseFirstReply(): void {
    this.wallParkLoseFirstReply = true;
    this.wallParkCommitted = false;
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
    const p = url.pathname;
    const body = await readBody(req);
    // The checkpoint broker streams a Git pack, not JSON. This fake has no publisher:
    // preserve the ordinary missing-route answer instead of throwing on the PACK header.
    if (req.method === "POST" && /^\/api\/worker\/runs\/[^/]+\/publish$/.test(p))
      return send(res, 404, { error: "not found", path: p });
    const json: Record<string, unknown> = body ? JSON.parse(body) : {};

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

    // Issue #1800: POST /inputs/included stamps the follow_up rows a prompt really carried. Fenced on
    // the claim generation only; rows not yet ACKed, not follow_up, or already included are skipped.
    const includedMatch = /^\/api\/worker\/runs\/([^/]+)\/inputs\/included$/.exec(p);
    if (req.method === "POST" && includedMatch) {
      const runId = includedMatch[1]!;
      const ids = json.ids as number[];
      const generation = json.claim_generation as number;
      this.inclusionCalls.push({ runId, ids: [...ids], generation });
      if (this.inclusionRouteMissing) return send(res, 404, { error: "not found" });
      if (this.inclusionNotOwned) return send(res, 404, { error: "run not found", reason: "stale" });
      const failing = this.failingInclusions;
      if (failing && failing.times > 0) {
        failing.times--;
        return send(res, failing.status, { error: "fake: inclusion failure" });
      }
      const current = this.receiptGeneration.get(runId);
      if (current !== undefined && generation !== current)
        return send(res, 200, { inputs: [], active: false, reason: "stale" });
      const rows = this.inputsByRun.get(runId) ?? [];
      const acked = this.ackedByRun.get(runId) ?? new Set<number>();
      const included = this.includedByRun.get(runId) ?? new Set<number>();
      const stamped = rows.filter((row) => ids.includes(row.id) && row.kind === "follow_up" && acked.has(row.id) && !included.has(row.id));
      for (const row of stamped) included.add(row.id);
      this.includedByRun.set(runId, included);
      return send(res, 200, { inputs: stamped.map((row) => ({ id: row.id, kind: row.kind })), active: true });
    }

    const receiptMatch = /^\/api\/worker\/runs\/([^/]+)\/inputs\/(ack|applied|discarded)$/.exec(p);
    if (req.method === "POST" && receiptMatch) {
      const runId = receiptMatch[1]!;
      const kind = receiptMatch[2] as ReceiptKind;
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
      // Issue #1604: an api that predates POST /inputs/discarded answers an untyped 404 (no route).
      if (kind === "discarded" && this.discardRouteMissing) return sendReceipt(404, { error: "not found" });
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
      // Issue #1604: the discarded route settles approve_plan rows only; any other kind is a 400.
      if (kind === "discarded" && ids.some((id) => rows.find((row) => row.id === id)?.kind !== "approve_plan"))
        return sendReceipt(400, { error: "only approve_plan inputs can be discarded" });
      if (kind === "ack" && !active && ids.some((id) => !acked.has(id)))
        return sendReceipt(409, { error: "inactive claim", reason });
      // Like the server: a retried applied for rows already applied succeeds even after the
      // claim was fenced; an unapplied row needs the active claim.
      if (kind !== "ack" && ids.some((id) => !acked.has(id) || (!active && !applied.has(id))))
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
        // Issue #1604: a discarded approve is applied with disposition 'superseded' (idempotent: a
        // row a gate took and applied keeps its human-approval disposition).
        if (kind === "discarded") {
          const discarded = this.discardedByRun.get(runId) ?? new Set<number>();
          for (const id of ids) if (!applied.has(id)) discarded.add(id);
          this.discardedByRun.set(runId, discarded);
        }
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

    if (req.method === "POST" && /^\/api\/worker\/runs\/[^/]+\/usage$/.test(p) && this.usageHandler) {
      this.usageRequests.push(json);
      const answer = this.usageHandler();
      return send(res, answer.status, answer.body);
    }

    const crossCheckMatch = /^\/api\/worker\/runs\/([^/]+)\/cross-checks(?:\/plan\/1)?$/.exec(p);
    if (crossCheckMatch && this.crossCheckHandler) {
      const request = { runId: crossCheckMatch[1]!, method: req.method ?? "", body: json };
      this.crossCheckRequests.push(request);
      const reply = (status: number, body: unknown, drop = false): void => {
        this.crossCheckReplies.push({ ...request, status, dropped: drop,
          acceptedCandidate: request.method === "POST" && status === 200 &&
            (body as { result?: string } | null)?.result === "candidate" });
        if (drop) res.destroy();
        else send(res, status, body);
      };
      if (request.method === "POST" && this.checkedFencedRuns.has(request.runId))
        return reply(409, { reason: "cross_check_refused" });
      if (request.method === "GET" && this.holdCrossCheckStatusUntilAbort?.(request.runId)) {
        await new Promise<void>((resolve) => res.once("close", resolve));
        this.abortedHeldCrossCheckStatuses++;
        return;
      }
      const answer = await this.crossCheckHandler(request);
      // A request held across the applied forced gate is fenced under that same row lock.
      if (request.method === "POST" && this.checkedFencedRuns.has(request.runId))
        return reply(409, { reason: "cross_check_refused" });
      return reply(answer.status, answer.body, answer.drop);
    }

    const runMatch =
      /^\/api\/worker\/runs\/([^/]+)\/(messages|state|inputs)$/.exec(p);
    if (runMatch) {
      const runId = runMatch[1] as string;
      const kind = runMatch[2] as string;
      if (req.method === "POST" && kind === "messages") {
        if (this.holdMessagesUntilAbort?.(runId, json)) {
          await new Promise<void>((resolve) => res.once("close", resolve));
          this.abortedHeldMessages++;
          return;
        }
        return this.handleMessages(res, runId, json);
      }
      if (req.method === "POST" && kind === "state")
        return this.handleState(res, runId, json);
      if (req.method === "GET" && kind === "inputs") {
        this.inputGets.set(runId, (this.inputGets.get(runId) ?? 0) + 1);
        const sendInputs = (status: number, body: unknown): void => {
          this.inputGetSendAttempts.set(runId, (this.inputGetSendAttempts.get(runId) ?? 0) + 1);
          send(res, status, body);
        };
        const hold = this.inputGetHolds.get(runId);
        if (hold && (this.inputGets.get(runId) ?? 0) > hold.after) {
          this.inputGetHolds.delete(runId);
          hold.enter();
          await hold.released;
        }
        // Issue #1604: delivery-failure injection (delay, transient status, raw body), consumed per read.
        const delay = this.inputGetDelays.get(runId);
        if (delay && delay.times > 0 && (this.inputGets.get(runId) ?? 0) > delay.after) {
          delay.times--;
          await new Promise((r) => setTimeout(r, delay.ms));
        }
        const failure = this.inputGetFailures.get(runId);
        if (failure && failure.times > 0) {
          failure.times--;
          return sendInputs(failure.status, { error: "fake: input GET failure" });
        }
        const raw = this.inputGetRaw.get(runId);
        if (raw && raw.times > 0) {
          raw.times--;
          return sendInputs(200, raw.body);
        }
        const rows = this.inputsByRun.get(runId) ?? [];
        const applied = this.appliedByRun.get(runId) ?? new Set<number>();
        const switchPending = this.switchPendingRuns.has(runId);
        const pending = (switchPending
          ? []
          : rows.filter((row) => !applied.has(row.id)).sort((a, b) => a.id - b.id)
        ).slice(0, this.inputPageSize ?? Infinity);
        if (this.legacyConsumeOnRead) {
          // An older api pod: consume on read (mark applied now) and send no receipt marker.
          for (const row of pending) applied.add(row.id);
          this.appliedByRun.set(runId, applied);
          return sendInputs(200, { inputs: pending });
        }
        const signal = switchPending ? this.switchSignalGeneration.get(runId) : this.foreignSwitchSignal.get(runId);
        return sendInputs(200, {
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
      const consumed = this.consumedFollowUpsByRun.get(runId) ?? [];
      if (!this.inclusionTracking) return send(res, 200, { inputs: consumed });
      const included = this.includedByRun.get(runId) ?? new Set<number>();
      return send(res, 200, {
        inputs: consumed.map((row) => ({
          ...row,
          inclusion_reported: true,
          ...(included.has(row.id) ? { included_at: "2026-10-01T00:00:00Z" } : {}),
        })),
      });
    }

    // issue #559 M3: the read-only ownership probe. Not part of the runMatch
    // alternation above (it is a GET-only read of a distinct segment), so it is
    // routed here. No override ⇒ 200 {status:"running"} (still ours, keep going).
    const ownMatch = /^\/api\/worker\/runs\/([^/]+)\/ownership$/.exec(p);
    if (req.method === "GET" && ownMatch) {
      const runId = ownMatch[1] as string;
      this.ownershipRequests.push(runId);
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

    // Issue #2083: the decisions-memo routes.
    const decisionsMatch = /^\/api\/worker\/runs\/([^/]+)\/decisions-memo$/.exec(p);
    if (decisionsMatch) {
      const runId = decisionsMatch[1] as string;
      if (req.method === "GET") {
        this.decisionsMemoGets.push({ runId, claimGeneration: url.searchParams.get("claim_generation") });
        const g = this.decisionsMemo.get;
        if (!g) return send(res, 404, { error: "not found" });
        return send(res, g.status, g.body);
      }
      if (req.method === "POST") {
        this.decisionsMemoPosts.push({ runId, body: json as { claim_generation?: number; body?: string } });
        if (this.decisionsMemo.postStatus === 204) return sendEmpty(res, 204);
        return send(res, this.decisionsMemo.postStatus, { error: "decisions_memo_rejected" });
      }
    }

    // PRD #1798 M6: the pr-description routes, answered by an in-memory model when a test installs
    // one (prDescription); without one they fall through to the 404 below, like an older api.
    const prDescMatch = /^\/api\/worker\/runs\/([^/]+)\/pr-description\/(stage|bind|lookup|ack)$/.exec(p);
    if (req.method === "POST" && prDescMatch && this.prDescription) {
      const out = this.prDescription.handle(prDescMatch[2] as PrDescOp, prDescMatch[1] as string, json);
      return send(res, out.status, out.body);
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
      if (this.wallParkLoseFirstReply && !this.wallParkCommitted) {
        this.wallParkCommitted = true;
        this.setOwnershipStatus(runId, "paused", json.claim_generation as number);
        req.socket.destroy();
        return;
      }
      if (this.wallParkCommitted) return send(res, 200, { run: { id: runId, status: "paused" } });
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
      if (m.kind === "plan") {
        const frames = this.planFramesByRun.get(runId) ?? [];
        frames.push({ at: this.stamp(), plan_md: (m.payload as { plan_md?: unknown } | undefined)?.plan_md });
        this.planFramesByRun.set(runId, frames);
      }
    }
    this.messagesByRun.set(runId, list);
    this.seenSeqByRun.set(runId, seen);
    send(res, 200, { accepted: incoming.length });
  }

  /** PRD #1795 M1: classify an awaiting_approval report against the run's gate (the real
   *  api's gate_revision.go), applying an accepted one's allocation / adoption. */
  private classifyGate(runId: string, body: StateRequest): { revision: number } | { refused: string } {
    const gate: FakeGate = this.gateByRun.get(runId) ?? {
      revision: 0,
      currentId: null,
      presentations: new Map(),
      snapshot: normalizePayload({}, undefined),
      countedGeneration: undefined,
      refusals: 0,
    };
    this.gateByRun.set(runId, gate);
    const effective = normalizePayload(body, gate.snapshot);
    const same = JSON.stringify(effective) === JSON.stringify(gate.snapshot);
    const id = body.presentation_id;
    const accept = (): { revision: number } => {
      gate.refusals = 0;
      gate.countedGeneration = undefined;
      return { revision: gate.revision };
    };
    if (body.adopt_gate_revision !== undefined) {
      if (id === undefined || gate.revision <= 0 || gate.revision !== body.adopt_gate_revision || gate.currentId !== null || !same) {
        // A retry after a lost adoption ACK is a current-id retry.
        if (id !== undefined && id === gate.currentId && same) return accept();
        return { refused: "gate_adoption_stale" };
      }
      gate.currentId = id;
      gate.presentations.set(id, gate.revision);
      return accept();
    }
    if (id !== undefined && id === gate.currentId) return same ? accept() : { refused: "gate_presentation_conflict" };
    if (id !== undefined && gate.presentations.has(id)) return { refused: "gate_presentation_historical" };
    gate.revision++;
    gate.currentId = id ?? null;
    if (id !== undefined) gate.presentations.set(id, gate.revision);
    gate.snapshot = effective;
    return accept();
  }

  private async handleState(
    res: http.ServerResponse,
    runId: string,
    json: Record<string, unknown>,
  ): Promise<void> {
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
    const drop = this.droppedStates.get(runId);
    if (drop?.matches(body)) {
      if (drop.beforeDrop !== undefined) await drop.beforeDrop;
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
    const answer = this.stateAnswerOnce.get(runId);
    if (answer && answer.matches(body)) {
      this.stateAnswerOnce.delete(runId);
      return send(res, answer.httpStatus, { gate_revision: answer.gateRevision, run: { id: runId, status: answer.runStatus } });
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
    // PRD #1795 M1: classify an awaiting_approval report under the modelled run-row lock, before
    // anything is recorded: a refusal publishes nothing.
    let gateRevision: number | undefined;
    if (this.gateRevisions && body.status === "awaiting_approval") {
      if ((body.presentation_id !== undefined || body.adopt_gate_revision !== undefined) && body.claim_generation === undefined)
        return send(res, 400, { error: "presentation_id and adopt_gate_revision require claim_generation", reason: "claim_generation_required" });
      const verdict = this.classifyGate(runId, body);
      if ("refused" in verdict) {
        const gate = this.gateByRun.get(runId)!;
        this.gateRefusals.push({ runId, reason: verdict.refused, generation: body.claim_generation });
        let status = lastStatus ?? "running";
        if (gate.countedGeneration !== body.claim_generation) {
          gate.countedGeneration = body.claim_generation;
          gate.refusals++;
          if (this.gateRefusalMax > 0 && gate.refusals > this.gateRefusalMax) {
            status = "failed";
            this.lastRecordedStatus.set(runId, "failed");
          }
        }
        return send(res, 409, { run: { id: runId, status }, reason: verdict.refused });
      }
      gateRevision = verdict.revision;
    }
    if (this.checkedTransport && body.status === "awaiting_approval") this.checkedFencedRuns.add(runId);
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
    // Issue #1604 (D1b): like SetRunFailedPlanRejected, a `failed` transition on a run whose
    // stop_kind is 'plan_rejected' (stamped when a reject_plan input was enqueued) settles the
    // run's unapplied reject_plan rows with the transition. The worker's fail_origin plays no part:
    // the server ignores it there. Reached only when the report is recorded (it applies).
    if (body.status === "failed" && this.stopKindByRun.get(runId) === "plan_rejected") {
      const applied = this.appliedByRun.get(runId) ?? new Set<number>();
      for (const row of this.inputsByRun.get(runId) ?? []) if (row.kind === "reject_plan") applied.add(row.id);
      this.appliedByRun.set(runId, applied);
    }
    this.stateHooks.get(runId)?.(body);
    // PRD #1795: persisted; now lose or hold the answer if a test armed it.
    const after = this.afterPersistHooks.get(runId);
    if (after && after.matches(body)) {
      this.afterPersistHooks.delete(runId);
      if (after.action === "drop") {
        res.destroy();
        return;
      }
      await after.action;
    }
    send(res, 200, {
      ...(this.checkedTransport && body.status === "awaiting_approval" ? {
        claim_generation: body.claim_generation, plan_cross_check_settled: true,
        lead_last_seq: Math.max(0, ...this.messages(runId).map((m) => m.seq)),
        current_plan_sha256: createHash("sha256").update(body.plan_md ?? "").digest("hex"),
        gate_presentation_id: body.presentation_id,
        gate_payload_digest: createHash("sha256").update(JSON.stringify(this.gateByRun.get(runId)?.snapshot)).digest("hex"),
      } : {}),
      // PRD #1795 M1 (decision 5): the revision this report was answered with, top-level.
      ...(gateRevision !== undefined ? { gate_revision: gateRevision } : {}),
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
        ...(this.stateAckScope.has(runId) ? this.stateAckScope.get(runId) : {}),
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

/** PRD #1795: the approval-relevant payload of a gate presentation (plan_changed_files excluded). */
export interface FakeGatePayload {
  plan_md?: string;
  milestones?: unknown;
  required_capabilities?: string[] | null;
  required_tools?: string[] | null;
  size_class?: string;
}

interface NormalizedGatePayload {
  plan_md: string;
  milestones: unknown;
  required_capabilities: string[];
  required_tools: string[];
  size_class: string | undefined;
}

interface FakeGate {
  revision: number;
  currentId: string | null;
  presentations: Map<string, number>;
  snapshot: NormalizedGatePayload;
  countedGeneration: number | undefined;
  refusals: number;
}

/** The effective presented payload: each field the report carries, else the previous snapshot's
 *  (the fields the api's persist query keeps when a report omits them). */
function normalizePayload(p: FakeGatePayload, prev: NormalizedGatePayload | undefined): NormalizedGatePayload {
  const sorted = (v: string[] | null | undefined, fallback: string[]): string[] => (v ? [...v].sort() : fallback);
  return {
    plan_md: p.plan_md ?? prev?.plan_md ?? "",
    milestones: p.milestones ?? prev?.milestones ?? null,
    required_capabilities: sorted(p.required_capabilities, prev?.required_capabilities ?? []),
    required_tools: sorted(p.required_tools, prev?.required_tools ?? []),
    size_class: p.size_class ?? prev?.size_class,
  };
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
