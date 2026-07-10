// The ChatRunner (PRD #39 M2, Decision 13). A slim sibling of RunRunner for the
// `chat` run kind: claim → session loop → complete. It shares the batcher, client,
// and redaction collaborators with a run, but has NO git collaborator at all — no
// ensureClone, no worktree, no push, no MR — because a chat holds no PAT and works
// the baked read-only source, never a clone. That absence is the point: the "no
// clone attempted by chat" property is structural, not a runtime check.
//
// Phase-1 scope: the runner skeleton against a PROVISIONAL ChatClaim shape (below),
// driving the ChatExecutor with real collaborators. The wire that yields the next
// user message (steering's await-next-follow-up, Decision 2) is Phase 2 — injected
// here as `nextUserMessage`, defaulting to "no more input" so a bare claim
// idle-completes cleanly.

import type { WorkerClient } from "./client.js";
import type { Logger } from "./log.js";
import type { StateRequest } from "./protocol.js";
import { MessageBatcher } from "./batcher.js";
import { ChatExecutor, type ChatContext, type ChatExecutorResult } from "./chat-executor.js";
import { makeRedactor, makeTextRedactor } from "./redact.js";
import { errMessage } from "./util.js";

/** Cap on a reported failure_reason (matches RunRunner / the GitLab error cap). */
const MAX_FAILURE_REASON_LEN = 512;

/**
 * PROVISIONAL chat-claim wire shape (PRD #39 Phase 1). It is DECOUPLED from M1's
 * real claim on purpose and reconciled in Phase 2 (M2 claim-loop split + live wire).
 * Load-bearing assumptions, each a Phase-2 reconciliation point:
 *   - `kind` is `"chat"` and there is no `repo` / `repo_id` — a chat run has no repo
 *     (Decision 12); claim assembly opens only the Anthropic token.
 *   - `secrets` carries the Anthropic token ONLY. There is NO `forge_pat` /
 *     `forge_username` key (Decision 9 — chat claims omit the PAT at the type level).
 *   - the FIRST user message rides the input wire like every subsequent turn
 *     (uniform), so the claim carries no message text; the runner's
 *     `nextUserMessage` source yields it. (If M1 instead delivers the first prompt
 *     as a claim field, this is where it lands.)
 */
export interface ChatClaim {
  run_id: string;
  kind: "chat";
  /** Conversation title, first-message derived (nullable). */
  title?: string | null;
  /** SDK session to resume; null/absent for a fresh chat. */
  session_id?: string | null;
  /** Decision 11: the ended chat this one continues. The worker best-effort resumes
   *  its `session_id` when affinity landed it here; otherwise it says so honestly. */
  resume_of_run_id?: string | null;
  /** High-water mark of run_messages.seq; the worker continues numbering from here. */
  last_seq: number;
  secrets: ChatClaimSecrets;
  config?: ChatClaimConfig | null;
}

/** A chat claim's secrets — the Anthropic token and NOTHING else (Decision 9). */
export interface ChatClaimSecrets {
  anthropic_oauth_token?: string;
}

/** Per-run chat caps the server may push down. */
export interface ChatClaimConfig {
  /** Per-run turn ceiling; when present the runner prefers it over the worker default. */
  chat_max_turns?: number;
  /** The run owner's per-user default model (PRD #17). */
  default_model?: string;
}

/** Worker-config lifecycle defaults the runner resolves each ChatContext from. */
export interface ChatRunnerDefaults {
  maxTurns: number;
  turnTimeoutMs: number;
  idleTimeoutMs: number;
}

export interface ChatRunnerOptions {
  /**
   * Source of the next user message for a claim (Phase 2: steering's await-next-
   * follow-up over `GET /inputs`, Decision 2). Returns undefined when there is no
   * further input, which the executor's park loop treats as idle-completion.
   * Default: always undefined (a fresh claim idle-completes with zero turns) — a
   * safe skeleton until Phase 2 wires the real input channel.
   */
  nextUserMessage?: (claim: ChatClaim, signal: AbortSignal) => Promise<string | undefined>;
}

/**
 * Drives one claimed `chat` run: report running → run the ChatExecutor's park/turn
 * loop → report completed (or failed). The WORKER never pushes or opens an MR here;
 * a chat produces conversation, not a branch.
 */
export class ChatRunner {
  private readonly nextUserMessage: (claim: ChatClaim, signal: AbortSignal) => Promise<string | undefined>;

  constructor(
    private readonly client: WorkerClient,
    private readonly executor: ChatExecutor,
    private readonly log: Logger,
    private readonly batchMs: number,
    private readonly defaults: ChatRunnerDefaults,
    /** The worker's join token — redacted from every message payload (it lives in
     *  the worker env, reachable via a /proc read of the parent). */
    private readonly joinToken?: string,
    opts: ChatRunnerOptions = {},
  ) {
    this.nextUserMessage = opts.nextUserMessage ?? (async () => undefined);
  }

  async execute(claim: ChatClaim): Promise<void> {
    const runId = claim.run_id;
    // Register the only secret a chat claim carries; never log the claim itself.
    if (claim.secrets.anthropic_oauth_token) this.log.addSecret(claim.secrets.anthropic_oauth_token);

    const runLog = this.log.child({ run_id: runId, kind: "chat" });
    const secrets = [claim.secrets.anthropic_oauth_token, this.joinToken];
    const redact = makeRedactor(secrets);
    const redactText = makeTextRedactor(secrets);
    const batcher = new MessageBatcher(this.client, runId, claim.last_seq, this.batchMs, runLog, redact);

    // A cancel (an explicit "End chat", or shutdown) aborts the whole conversation;
    // the executor's ctx.signal watches it. Phase 2 trips this from the steering
    // channel; Phase 1 leaves it inert (a bare claim idle-completes).
    const cancel = new AbortController();

    // Last SDK session id observed, carried on every state report so resume survives
    // a lost report (§51, same as RunRunner).
    let observedSessionId = claim.session_id ?? undefined;
    const reportState = (body: StateRequest): Promise<void> =>
      this.client.reportState(runId, observedSessionId ? { ...body, session_id: observedSessionId } : body);

    try {
      runLog.info("chat claimed", { resume_of: claim.resume_of_run_id ?? null, session: claim.session_id ?? null });
      await reportState({ status: "running" });

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
        // Claim config (per-run) wins over the worker default for the turn cap
        // (Decision 3); the wall-clock + idle windows are worker cadences.
        maxTurns: claim.config?.chat_max_turns ?? this.defaults.maxTurns,
        turnTimeoutMs: this.defaults.turnTimeoutMs,
        idleTimeoutMs: this.defaults.idleTimeoutMs,
        model: claim.config?.default_model,
        nextUserMessage: () => this.nextUserMessage(claim, cancel.signal),
      };

      const result = await this.executor.run(ctx);
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
    }
  }
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
