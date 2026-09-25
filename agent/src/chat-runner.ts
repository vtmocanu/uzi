// The ChatRunner (PRD #39 M2/Decision 13). A slim sibling of RunRunner for the
// `chat` run kind: claim → session loop → complete. It shares the batcher, client,
// and redaction collaborators with a run, but has NO git collaborator at all — no
// ensureClone, no worktree, no push, no MR — because a chat holds no PAT and works
// the baked read-only source, never a clone. That absence is the point: the "no
// clone attempted by chat" property is structural, not a runtime check.
//
// Phase 2 wires the real input channel: the ChatSteering channel feeds
// `nextUserMessage` (Decision 2), and each consumed input — including the seeded
// first message (M1 CreateChatRun) — is emitted as ONE `user_message` run message
// (the worker owns the gapless seq) BEFORE the model's response for that turn.

import type { WorkerClient } from "./client.js";
import type { Logger } from "./log.js";
import type { ChatClaimResponse, StateAck, StateRequest } from "./protocol.js";
import { MessageBatcher } from "./batcher.js";
import type { Outbox } from "./outbox.js";
import type { ChatContext, ChatExecutorLike, ChatExecutorResult } from "./chat-executor.js";
import { ChatSteering, type ChatInputSource } from "./steering.js";
import { buildUziToolsServer, UZI_TOOLS_SERVER_NAME } from "./uzi-tools.js";
import { makeRedactor, makeTextRedactor } from "./redact.js";
import { sessionTranscriptResolvable } from "./sdk-session.js";
import { errMessage } from "./util.js";

/** Cap on a reported failure_reason (matches RunRunner / the GitLab error cap). */
const MAX_FAILURE_REASON_LEN = 512;

/**
 * Build a per-session chat executor (PRD #42 Decision 4, chat lane). Called once
 * per `execute`, so each concurrent chat session drives its OWN executor instance
 * and its `spawnedPids`/`killAgentTree` are private to it — two sessions at
 * `WORKER_CHAT_SESSIONS>1` can never reap each other's SDK subprocess. Returns a
 * `ChatExecutorLike` so the factory can build the real `ChatExecutor` or the
 * `StubChatExecutor` (E2E, PRD #39) per session. The real executor KEEPS the shared
 * SDK HOME (a Continue resumes the same session under a new run_id, so per-run HOME
 * would break resume); only the instance is per-session.
 */
export type ChatExecutorFactory = () => ChatExecutorLike;

/** Worker-config lifecycle defaults the runner resolves each ChatContext from; a
 *  claim's own config wins per-run over these (Decision 3, brief §4). */
export interface ChatRunnerDefaults {
  maxTurns: number;
  turnTimeoutMs: number;
  idleTimeoutMs: number;
  /** Chat input poll cadence (WORKER_CHAT_POLL_MS). */
  pollMs: number;
}

export interface ChatRunnerOptions {
  /**
   * Build the input source for a claim (Decision 2). Default: a real ChatSteering
   * over `GET /inputs`. Tests inject a fake that yields scripted ChatInputs so the
   * user_message emission and the turn/idle/cancel flow are provable without HTTP.
   */
  makeSource?: (runId: string, cancel: AbortController, log: Logger, claimGeneration: number) => ChatInputSource;
  /**
   * The SDK HOME the chat executor runs under, so a Continue can check whether the
   * session it was handed is actually resolvable on THIS worker (issue #105). The check
   * globs this HOME's project dirs (sdk-session.ts) rather than computing the one the
   * cwd encodes to, so only the HOME is needed here, not the cwd: every chat session
   * runs under the same baked-source cwd, so the shared chat HOME holds exactly one
   * project dir.
   *
   * Chat shares one HOME across sessions by design (main.ts) — a Continue is a new run
   * id resuming the same session, so a per-run HOME would file the transcript under the
   * new id and break resume outright.
   *
   * Absent ⇒ no preflight. main.ts omits it for the stub executor, which persists no
   * real SDK session, so the e2e's report → resume_of → Continue flow is unaffected.
   */
  sdkHomeDir?: string;
  /** PRD #1391 M2 — the worker message outbox the chat batcher SPILLS to after a
   *  sustained transient outage (chat keeps the message outbox even though it has no
   *  claim generation, D6/D10). Undefined ⇒ today's trip behaviour. */
  outbox?: Outbox;
  /** PRD #1391 M2 — the shared re-arm registry (runId → the live batcher's rearm()),
   *  the same map the run lane + worker share. */
  rearm?: Map<string, () => void>;
  /** PRD #1391 M2 — the spill trip window (config.transientTripMs); default is the
   *  batcher's own TRANSIENT_TRIP_MS. */
  transientTripMs?: number;
  /** PRD #1391 M2 — the in-memory spill-buffer cap (config.outboxSpillBufferBytes);
   *  default is the batcher's own 2 MiB. */
  outboxSpillBufferBytes?: number;
}

/**
 * Drives one claimed `chat` run: report running → run the ChatExecutor's park/turn
 * loop → report completed (or failed). The WORKER never pushes or opens an MR here;
 * a chat produces conversation, not a branch.
 */
export class ChatRunner {
  private readonly makeSource: (runId: string, cancel: AbortController, log: Logger, claimGeneration: number) => ChatInputSource;
  private readonly sdkHomeDir?: string;
  /** PRD #1391 M2 — spill collaborators threaded into the chat batcher (see execute). */
  private readonly outbox: Outbox | undefined;
  private readonly rearm: Map<string, () => void> | undefined;
  private readonly transientTripMs: number | undefined;
  private readonly outboxSpillBufferBytes: number | undefined;

  constructor(
    private readonly client: WorkerClient,
    /** Per-session executor factory (PRD #42 Decision 4). Called once per `execute`
     *  so each chat session drives its OWN executor instance (real or stub). */
    private readonly makeExecutor: ChatExecutorFactory,
    private readonly log: Logger,
    private readonly batchMs: number,
    private readonly defaults: ChatRunnerDefaults,
    /** The worker's join token — redacted from every message payload (it lives in
     *  the worker env, reachable via a /proc read of the parent). */
    private readonly joinToken?: string,
    opts: ChatRunnerOptions = {},
  ) {
    this.makeSource =
      opts.makeSource ??
      ((runId, cancel, runLog, generation) => new ChatSteering(this.client, runId, this.defaults.pollMs, runLog, cancel, {}, generation));
    this.sdkHomeDir = opts.sdkHomeDir;
    this.outbox = opts.outbox;
    this.rearm = opts.rearm;
    this.transientTripMs = opts.transientTripMs;
    this.outboxSpillBufferBytes = opts.outboxSpillBufferBytes;
  }

  /**
   * @param signal the worker's shutdown signal (optional). When it aborts, the chat's
   *   own cancel trips, so an in-flight chat stops promptly on SIGTERM rather than
   *   parking until idle — the chat lane fires sessions concurrently (fire-and-forget),
   *   so unlike the inline run lane it needs this to drain cleanly at shutdown.
   */
  async execute(claim: ChatClaimResponse, signal?: AbortSignal): Promise<void> {
    const runId = claim.run_id;
    // This session's OWN executor (PRD #42 Decision 4), so its spawnedPids/reap are
    // private to it — no sharing with a concurrent chat session.
    const executor = this.makeExecutor();
    // Register the only secret a chat claim carries; never log the claim itself.
    // Evicted on terminal (PRD #42 Decision 7) like a run's secrets — the registry
    // is reference-counted, so an issue run of the same user still holding this
    // token keeps it scrubbed.
    const oauthToken = claim.secrets.anthropic_oauth_token;
    if (oauthToken) this.log.addSecret(oauthToken);

    const runLog = this.log.child({ run_id: runId, kind: "chat" });
    const secrets = [claim.secrets.anthropic_oauth_token, this.joinToken];
    const redact = makeRedactor(secrets);
    const redactText = makeTextRedactor(secrets);
    // redactText also covers the PRD #99 top-level agent_label/agent_instance the
    // batcher carries beside the payload. A chat run never spawns a subagent
    // (NESTED_AGENT_TOOL is disallowed), so those fields stay absent here — the
    // redactor is wired anyway so the two runner paths cannot drift.
    const batcher = new MessageBatcher(this.client, runId, claim.last_seq, this.batchMs, runLog, redact, redactText, {
      // PRD #1391 M2: chat keeps the message outbox (Run A) — it spills like the run
      // lane after a sustained transient outage. Generation is always 0: chat has no
      // claim generation (D6/D10), so nothing is fenced on replay.
      ...(this.outbox ? { outbox: this.outbox } : {}),
      generation: 0,
      ...(this.transientTripMs !== undefined ? { transientTripMs: this.transientTripMs } : {}),
      ...(this.outboxSpillBufferBytes !== undefined ? { spillBufferBytes: this.outboxSpillBufferBytes } : {}),
    });
    // PRD #1391 M2: register this chat's batcher in the shared re-arm registry so the
    // per-worker drainer can return it to the network once its spilled segments retire.
    // Dropped in the terminal finally below.
    this.rearm?.set(runId, () => batcher.rearm());

    // A `cancel` (End chat) input, or worker shutdown, aborts the whole conversation.
    // This SAME controller is the steering channel's cancel AND the executor's
    // ctx.signal, so an End chat aborts a turn in flight, not just a parked wait.
    const cancel = new AbortController();
    const onShutdown = (): void => {
      if (!cancel.signal.aborted) cancel.abort();
    };
    if (signal) {
      if (signal.aborted) cancel.abort();
      else signal.addEventListener("abort", onShutdown, { once: true });
    }
    // Issue #1673: the chat claim carries its generation for the input receipts (normally 0).
    const source = this.makeSource(runId, cancel, runLog, claim.claim_generation ?? 0);

    // The uzi tools MCP server (M3): bound to THIS run's client + run id, so
    // propose_issue can only ever propose on this chat run, and the read tools call
    // the worker-authenticated, user-scoped endpoints. Its tool names are added to
    // the executor's `tools` allowlist so they are actually callable. `emit` is the
    // run's batcher so propose_issue can stream the proposal card (worker owns the seq).
    const uziTools = buildUziToolsServer({
      client: this.client,
      runId,
      emit: (m) => batcher.emit(m),
      log: runLog,
    });

    // Resolve this run's clocks: the claim config (server-pushed, no drift) wins over
    // the worker env defaults (Decision 3). Timeouts are delivered in SECONDS.
    const maxTurns = positiveOr(claim.config.max_turns, this.defaults.maxTurns);
    const turnTimeoutMs = secondsToMs(claim.config.turn_timeout_seconds) ?? this.defaults.turnTimeoutMs;
    const idleTimeoutMs = secondsToMs(claim.config.idle_timeout_seconds) ?? this.defaults.idleTimeoutMs;

    // Resume preflight (issue #105) — see the Decision 11 block below for why the
    // TRANSCRIPT, not the id, is the honest signal. Done before the first state report
    // so a dropped session id is never carried back to the server as this run's resume
    // target; the feed message it warrants is emitted in that block.
    let sessionId = claim.session_id ?? undefined;
    let resumeDropped = false;
    if (sessionId && this.sdkHomeDir && !(await sessionTranscriptResolvable(this.sdkHomeDir, sessionId, runLog))) {
      runLog.warn("chat resume session transcript is not on this worker; continuing without it");
      sessionId = undefined;
      resumeDropped = true;
    }

    // Last SDK session id observed, carried on every state report so resume survives
    // a lost report (§51, same as RunRunner). Seeded from the (preflighted) claim so a
    // resumed/continued chat reports its resume target even before the first turn.
    let observedSessionId = sessionId;
    // Widened for PRD #35's acknowledgement contract. Chat never parks (Decision 9),
    // so this lane ignores the value; the annotation just tracks the client.
    const reportState = (body: StateRequest): Promise<StateAck> =>
      this.client.reportState(runId, observedSessionId ? { ...body, session_id: observedSessionId } : body);

    // PRD #108 M3: the chat lane builds the SAME MessageBatcher against the SAME
    // endpoint as the run lane, so it inherits the byte cap, the split, the backoff
    // and the breaker for free — but the breaker's report is injected, so it must be
    // wired HERE too or a wedged chat trips silently. This closure differs from the
    // run lane's in one way that matters: it carries `session_id`, so a Continue
    // still resumes the right SDK session after a transport failure.
    batcher.onPermanentFailureReport(({ reason }) => {
      void reportState({ status: "failed", failure_reason: reason.slice(0, MAX_FAILURE_REASON_LEN) }).catch((e) =>
        runLog.error("could not report the message-transport failure", { error: errMessage(e) }),
      );
    });

    try {
      runLog.info("chat claimed", {
        resume_of: claim.resume_of_run_id ?? null,
        session: claim.session_id ?? null,
        resume_dropped: resumeDropped,
      });
      await reportState({ status: "running" });
      source.start();

      // Decision 11: a Continue whose prior session is not on this worker's disk
      // resumes without context — say so honestly instead of pretending.
      //
      // This USED to test `resume_of_run_id && !claim.session_id`, and that condition
      // could not detect the case the comment describes (issue #105). The server copies
      // the prior run's session id onto a Continue unconditionally
      // (GetChatRunClaimContext, api/internal/store/queries/chat.sql) and ClaimChatRun
      // hands the run to any of the user's workers once the affinity grace lapses — so
      // on a cross-worker Continue the id IS present, this stayed silent, and the id
      // pointed at a transcript on another machine. The SDK resolves a resume locally,
      // so every turn then died with `error_during_execution` and, because an errored
      // chat turn parks for the next message instead of ending, the conversation was
      // bricked for good. "Session id absent" is only ever the case where the prior run
      // never persisted one at all.
      //
      // So the honest signal is the TRANSCRIPT, not the id: the preflight above drops an
      // unresolvable resume so the chat continues context-free (and says so here)
      // instead of failing forever. `resumeDropped` covers the cross-worker Continue;
      // the original `!sessionId` arm still covers a prior run that never persisted a
      // session at all. No preflight without a HOME to look in (the stub, e2e).
      if ((claim.resume_of_run_id && !sessionId) || resumeDropped) {
        batcher.emit({
          kind: "status",
          agent: "worker",
          payload: { text: "continuing this chat without its earlier context (the prior session is not on this worker)" },
        });
      }

      const ctx: ChatContext = {
        runId,
        oauthToken: claim.secrets.anthropic_oauth_token,
        // Preflighted above: the claim's id, or undefined when its transcript is not
        // on this worker (issue #105).
        sessionId,
        emit: (m) => batcher.emit(m),
        onSessionId: (sessionId) => {
          observedSessionId = sessionId;
          void reportState({ status: "running" }).catch((e) =>
            runLog.warn("could not persist chat session id", { error: errMessage(e) }),
          );
        },
        signal: cancel.signal,
        maxTurns,
        turnTimeoutMs,
        model: claim.config.default_model,
        effort: claim.config.default_effort,
        mcpServers: { [UZI_TOOLS_SERVER_NAME]: uziTools.server },
        extraTools: uziTools.toolNames,
        // The stub executor (e2e) calls these directly; the real executor ignores
        // them and drives the model through the MCP server above.
        uziTools: uziTools.handlers,
        // Park on the steering channel; on a delivered message, emit the user_message
        // run message (worker owns the seq) BEFORE the executor streams the model's
        // reply for that turn. `idle`/`ended` → undefined (the loop completes; the
        // executor reads ctx.signal to tell an End chat from an idle).
        nextUserMessage: async () => {
          const input = await source.awaitFollowUp(idleTimeoutMs);
          if (input.kind !== "message") return undefined;
          batcher.emit({ kind: "user_message", payload: { text: input.text } });
          return input.text;
        },
      };

      const result = await executor.run(ctx);
      await source.stop();
      if (source.claimLost?.()) {
        await batcher.close().catch(() => undefined);
        return;
      }
      // Issue #1673: a message whose applied receipt was given up must not be completed over.
      const unconfirmed = source.unconfirmedInput?.();
      if (unconfirmed) throw new Error(unconfirmed);
      batcher.emit({ kind: "status", agent: "worker", payload: { text: chatEndText(result) } });
      await batcher.close();
      await reportState({ status: "completed" });
      runLog.info("chat completed", { turns: result.turns, end_reason: result.endReason });
    } catch (err) {
      await source.stop().catch(() => undefined);
      if (source.claimLost?.()) {
        await batcher.close().catch(() => undefined);
        return;
      }
      const reason = redactText(source.unconfirmedInput?.() ?? errMessage(err));
      runLog.error("chat failed", { error: reason });
      batcher.emit({ kind: "error", agent: "worker", payload: { text: reason } });
      await batcher.close().catch(() => undefined);
      await reportState({ status: "failed", failure_reason: reason.slice(0, MAX_FAILURE_REASON_LEN) }).catch((e) =>
        runLog.error("could not report failed chat state", { error: errMessage(e) }),
      );
    } finally {
      if (signal) signal.removeEventListener("abort", onShutdown);
      // PRD #1391 M2: drop this chat's re-arm registration (the batcher is closed by
      // now, so a later drainer retire never needs to re-arm it).
      this.rearm?.delete(runId);
      await source.stop().catch(() => undefined);
      // Evict this chat's token now the run is terminal (Decision 7).
      if (oauthToken) this.log.removeSecret(oauthToken);
    }
  }
}

/** Convert an optional positive seconds value to ms, else undefined (use fallback). */
function secondsToMs(seconds: number | undefined): number | undefined {
  return typeof seconds === "number" && seconds > 0 ? Math.round(seconds * 1000) : undefined;
}

/** A positive integer override, else the fallback. */
function positiveOr(value: number | undefined, fallback: number): number {
  return typeof value === "number" && value > 0 ? Math.floor(value) : fallback;
}

/** A user-facing completion line for how the conversation ended. */
function chatEndText(result: ChatExecutorResult): string {
  switch (result.endReason) {
    case "turn_cap":
      return `chat ended after reaching its turn limit (${result.turns} turns)`;
    case "ended":
      return "chat ended";
    case "error":
      return "chat ended after an error";
    case "idle":
    default:
      return "chat ended after a period of inactivity";
  }
}
