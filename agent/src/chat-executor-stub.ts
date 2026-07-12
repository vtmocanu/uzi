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
import type { ToolTextResult, UziToolHandlers } from "./uzi-tools.js";

// Deterministic sentinels the e2e puts in a chat message to drive the stub through
// the REAL uzi tools with no live model. Documented for the tester (task #22):
//
//   "UZI_STUB_READ [<run_id>]"
//     → the stub calls the real list_runs handler (a genuine tool result lands in the
//       feed) and, when a run_id is given, get_run_messages(run_id) too. Both outputs
//       are the evidence-fenced text the tools produce, emitted as tool_result run
//       messages — so the M5 red-team leg can seed a poisoned run message/title and
//       assert it comes back QUOTED inside the nonce fence, never as a bare instruction.
//
//   "UZI_STUB_PROPOSE <repo_path> [title words...]"
//     → the stub calls the real propose_issue handler (createProposal → issue_proposals
//       row + the `proposal` card emit), so the e2e can drive propose → card → confirm.
//       repo_path comes from the message (the tester passes a repo the seed user owns);
//       omit it and the tool returns its "needs the target repo" guidance (no card).
//
// A non-sentinel message just gets a canned "stub chat reply to: <msg>".
export const STUB_CHAT_READ = "UZI_STUB_READ";
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

  /** Answer one turn: on a sentinel, invoke the REAL uzi tools (read and/or propose);
   *  otherwise a canned reply. A message may carry both sentinels; each is handled
   *  independently. Always closes with a result frame (a feed turn boundary). */
  private async answer(ctx: ChatContext, message: string): Promise<void> {
    let handled = false;
    if (ctx.uziTools && message.includes(STUB_CHAT_READ)) {
      await this.doRead(ctx, ctx.uziTools, tailAfter(message, STUB_CHAT_READ));
      handled = true;
    }
    if (ctx.uziTools && message.includes(STUB_CHAT_PROPOSE)) {
      await this.doPropose(ctx, ctx.uziTools, tailAfter(message, STUB_CHAT_PROPOSE));
      handled = true;
    }
    if (!handled) {
      ctx.emit({ kind: "text", agent: "lead", payload: { text: `stub chat reply to: ${message}` } });
    }
    ctx.emit({ kind: "status", agent: "lead", payload: { event: "result", subtype: "success" } });
  }

  /** Call the real read tools; emit their (evidence-fenced) output as tool_result run
   *  messages, so a genuine tool result — and any poisoned run content, still quoted
   *  inside the nonce fence — lands in the feed. */
  private async doRead(ctx: ChatContext, tools: UziToolHandlers, args: string[]): Promise<void> {
    const list = await tools.listRuns({});
    ctx.emit({ kind: "tool_result", agent: "lead", payload: { tool_use_id: "stub-list_runs", content: resultText(list) } });
    const runId = args[0];
    if (runId) {
      const msgs = await tools.getRunMessages({ run_id: runId });
      ctx.emit({ kind: "tool_result", agent: "lead", payload: { tool_use_id: "stub-get_run_messages", content: resultText(msgs) } });
    }
    ctx.emit({ kind: "text", agent: "lead", payload: { text: "I looked at your runs (stub)." } });
  }

  /** Call the real propose_issue handler (pending proposal + card). */
  private async doPropose(ctx: ChatContext, tools: UziToolHandlers, args: string[]): Promise<void> {
    const repoPath = args[0];
    const title = args.slice(1).join(" ") || "stub proposed issue";
    const res = await tools.proposeIssue({
      repo_path: repoPath,
      title,
      description: "Proposed by the stub chat executor for the e2e.",
      labels: [],
    });
    const text = res.isError
      ? resultText(res)
      : "I drafted an issue proposal for you — click Create on the card to open it.";
    ctx.emit({ kind: "text", agent: "lead", payload: { text } });
  }
}

/** Tokens after a sentinel occurrence in a message (whitespace-split, empties dropped). */
function tailAfter(message: string, sentinel: string): string[] {
  const i = message.indexOf(sentinel);
  if (i < 0) return [];
  return message.slice(i + sentinel.length).trim().split(/\s+/).filter(Boolean);
}

/** The text a tool handler returned (the evidence-fenced payload for a read tool). */
function resultText(r: ToolTextResult): string {
  return String(r.content[0]?.text ?? "");
}
