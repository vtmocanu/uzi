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
import type { ChatClaimResponse, StateRequest } from "./protocol.js";
import { MessageBatcher } from "./batcher.js";
import { ChatExecutor, type ChatContext, type ChatExecutorResult } from "./chat-executor.js";
import { ChatSteering, type ChatInputSource } from "./steering.js";
import { buildUziToolsServer, UZI_TOOLS_SERVER_NAME } from "./uzi-tools.js";
import { makeRedactor, makeTextRedactor } from "./redact.js";
import { errMessage } from "./util.js";

/** Cap on a reported failure_reason (matches RunRunner / the GitLab error cap). */
const MAX_FAILURE_REASON_LEN = 512;

/**
 * Build a per-session `ChatExecutor` (PRD #42 Decision 4, chat lane). Called once
 * per `execute`, so each concurrent chat session drives its OWN executor instance
 * and its `spawnedPids`/`killAgentTree` are private to it — two sessions at
 * `WORKER_CHAT_SESSIONS>1` can never reap each other's SDK subprocess. The chat
 * executor KEEPS the shared SDK HOME (a Continue resumes the same session under a
 * new run_id, so per-run HOME would break resume); only the instance is per-session.
 */
export type ChatExecutorFactory = () => ChatExecutor;

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
  makeSource?: (runId: string, cancel: AbortController, log: Logger) => ChatInputSource;
}

/**
 * Drives one claimed `chat` run: report running → run the ChatExecutor's park/turn
 * loop → report completed (or failed). The WORKER never pushes or opens an MR here;
 * a chat produces conversation, not a branch.
 */
export class ChatRunner {
  private readonly makeSource: (runId: string, cancel: AbortController, log: Logger) => ChatInputSource;

  constructor(
    private readonly client: WorkerClient,
    /** Per-session executor factory (PRD #42 Decision 4). Called once per `execute`
     *  so each chat session drives its OWN ChatExecutor instance. */
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
      ((runId, cancel, runLog) => new ChatSteering(this.client, runId, this.defaults.pollMs, runLog, cancel));
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
    const batcher = new MessageBatcher(this.client, runId, claim.last_seq, this.batchMs, runLog, redact);

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
    const source = this.makeSource(runId, cancel, runLog);

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

    // Last SDK session id observed, carried on every state report so resume survives
    // a lost report (§51, same as RunRunner). Seeded from the claim so a resumed/
    // continued chat reports its resume target even before the first turn.
    let observedSessionId = claim.session_id ?? undefined;
    const reportState = (body: StateRequest): Promise<void> =>
      this.client.reportState(runId, observedSessionId ? { ...body, session_id: observedSessionId } : body);

    try {
      runLog.info("chat claimed", { resume_of: claim.resume_of_run_id ?? null, session: claim.session_id ?? null });
      await reportState({ status: "running" });
      source.start();

      // Decision 11: a Continue whose prior session is not on this worker's disk
      // resumes without context — say so honestly instead of pretending.
      if (claim.resume_of_run_id && !claim.session_id) {
        batcher.emit({
          kind: "status",
          agent: "worker",
          payload: { text: "continuing this chat without its earlier context (the prior session is not on this worker)" },
        });
      }

      const ctx: ChatContext = {
        runId,
        oauthToken: claim.secrets.anthropic_oauth_token,
        sessionId: claim.session_id,
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
        mcpServers: { [UZI_TOOLS_SERVER_NAME]: uziTools.server },
        extraTools: uziTools.toolNames,
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
      batcher.emit({ kind: "status", agent: "worker", payload: { text: chatEndText(result) } });
      await batcher.close();
      await reportState({ status: "completed" });
      runLog.info("chat completed", { turns: result.turns, end_reason: result.endReason });
    } catch (err) {
      const reason = redactText(errMessage(err));
      runLog.error("chat failed", { error: reason });
      batcher.emit({ kind: "error", agent: "worker", payload: { text: reason } });
      await batcher.close().catch(() => undefined);
      await reportState({ status: "failed", failure_reason: reason.slice(0, MAX_FAILURE_REASON_LEN) }).catch((e) =>
        runLog.error("could not report failed chat state", { error: errMessage(e) }),
      );
    } finally {
      if (signal) signal.removeEventListener("abort", onShutdown);
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
