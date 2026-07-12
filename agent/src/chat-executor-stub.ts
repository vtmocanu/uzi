// Stub chat executor (PRD #39, team task #15). The chat lane's counterpart to the
// run lane's StubExecutor: under UZI_EXECUTOR=stub it drives the SAME ChatContext
// park/turn loop as the real ChatExecutor but emits canned replies instead of
// running a live Claude Agent SDK session — so the M6 chat e2e can run on the
// isolated stack with dummy creds and NO Anthropic token.
//
// It exercises the whole seam the browser depends on: user_message emission
// (the ChatRunner emits that before nextUserMessage returns), an assistant reply,
// a proposal CARD (via the real propose_issue handler on a sentinel), the turn cap,
// idle-completion, End-chat cancel, and a persisted session id for Continue.

import type { Logger } from "./log.js";
import type { ChatContext, ChatEndReason, ChatExecutorLike, ChatExecutorResult } from "./chat-executor.js";

/**
 * Sentinel in a user message that makes the stub DRAFT an issue via the real
 * propose_issue handler (create the pending proposal + emit the card), so the e2e
 * can drive propose → card → confirm with no live model:
 *
 *   "UZI_STUB_PROPOSE <repo_path> [title words...]"
 */
export const STUB_CHAT_PROPOSE = "UZI_STUB_PROPOSE";

export class StubChatExecutor implements ChatExecutorLike {
  constructor(private readonly log: Logger) {}

  async run(ctx: ChatContext): Promise<ChatExecutorResult> {
    // Fabricate a stable session id so /state carries one and Continue (Decision 11)
    // has something to resume — the stub persists no real SDK session, but the flow
    // (report → resume_of on continue) must still line up in the e2e.
    const sessionId = ctx.sessionId ?? `stub-chat-${ctx.runId}`;
    ctx.onSessionId?.(sessionId);

    let turns = 0;
    let endReason: ChatEndReason = "ended";
    for (;;) {
      const next = await ctx.nextUserMessage();
      if (next === undefined) {
        endReason = ctx.signal?.aborted ? "ended" : "idle";
        break;
      }
      if (turns >= ctx.maxTurns) {
        endReason = "turn_cap";
        ctx.emit({
          kind: "status",
          agent: "worker",
          payload: { text: `chat reached its ${ctx.maxTurns}-turn limit; start a new chat to continue` },
        });
        break;
      }
      turns++;
      await this.answer(ctx, next);
      if (ctx.signal?.aborted) {
        endReason = "ended";
        break;
      }
    }

    this.log.info("stub chat session ended", { run_id: ctx.runId, turns, end_reason: endReason });
    return { turns, endReason, sessionId };
  }

  /** Answer one turn: a canned reply, or — on the propose sentinel — the real
   *  propose_issue handler (pending proposal + card). */
  private async answer(ctx: ChatContext, message: string): Promise<void> {
    const at = message.indexOf(STUB_CHAT_PROPOSE);
    if (at >= 0 && ctx.uziTools) {
      const rest = message.slice(at + STUB_CHAT_PROPOSE.length).trim().split(/\s+/).filter(Boolean);
      const repoPath = rest[0];
      const title = rest.slice(1).join(" ") || "stub proposed issue";
      const res = await ctx.uziTools.proposeIssue({
        repo_path: repoPath,
        title,
        description: "Proposed by the stub chat executor for the e2e.",
        labels: [],
      });
      const text = res.isError
        ? String(res.content[0]?.text ?? "could not draft the issue")
        : "I drafted an issue proposal for you — click Create on the card to open it.";
      ctx.emit({ kind: "text", agent: "lead", payload: { text } });
    } else {
      ctx.emit({ kind: "text", agent: "lead", payload: { text: `stub chat reply to: ${message}` } });
    }
    // Mirror the real executor's terminal result frame so the feed shows a turn boundary.
    ctx.emit({ kind: "status", agent: "lead", payload: { event: "result", subtype: "success" } });
  }
}
