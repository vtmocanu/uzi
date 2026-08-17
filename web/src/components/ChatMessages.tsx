import { useMemo } from "react";
import type { CancelRequest, IssueProposal, RunMessage, RunRequest } from "../lib/api";
import { useFollowScroll, useReconnectingBanner } from "../lib/useFollowScroll";
import { buildToolIndex, RunEventRow } from "./RunEvent";
import { Markdown } from "./Markdown";
import { ProposalCard } from "./ProposalCard";
import { RunRequestCard } from "./RunRequestCard";
import { CancelRequestCard } from "./CancelRequestCard";
import { cx } from "./ui";

// ChatMessages renders a chat conversation as bubbles over the SAME persisted,
// seq-numbered stream the run view uses (PRD #39 M4), with the two trust policies
// the PRD requires held apart:
//   - `text` (model prose) → the §61 untrusted-LLM Markdown core (links open in a
//     new tab, images size-capped, no raw HTML) — a chat bubble.
//   - `user_message` → the user's own turn, rendered as plain escaped text.
//   - `proposal` → the human-gated ProposalCard (inert model text, no Markdown).
//   - everything else (thinking/tool_use/tool_result/status/error) → the terse,
//     ESCAPED RunEvent rows (§63) — tool + evidence output is never re-parsed as
//     prose. This is what keeps investigated evidence from becoming live markup.
// Follow behaviour (§62) is the shared useFollowScroll: append-scroll off a ref,
// a "{n} new ↓" pill while scrolled up, a delayed reconnecting banner.
export function ChatMessages({
  chatId,
  messages,
  connected,
  live,
}: {
  chatId: string;
  messages: RunMessage[];
  connected: boolean;
  // live: the conversation's run is non-terminal, so a tool call with no result
  // yet shows a running spinner rather than "no result".
  live: boolean;
}) {
  const follow = useFollowScroll(messages.length);
  const reconnecting = useReconnectingBanner(connected);
  const toolIndex = useMemo(() => buildToolIndex(messages), [messages]);

  return (
    <div className="space-y-2">
      {reconnecting && (
        <div className="rounded-md border border-warn/40 bg-warn/10 px-3 py-1.5 text-xs text-warn">
          Reconnecting… live updates are paused.
        </div>
      )}

      {messages.length === 0 ? (
        <p className="py-10 text-center text-sm text-faint">
          {live ? "Say hello — ask uzi about itself, your runs, or an idea." : "No messages yet."}
        </p>
      ) : (
        <div className="relative">
          <div
            ref={follow.ref}
            onScroll={follow.onScroll}
            role="log"
            aria-live="polite"
            aria-label="Conversation"
            className="max-h-[62vh] space-y-3 overflow-auto rounded-lg border border-edge bg-ink/60 p-3 [overscroll-behavior:contain]"
          >
            {messages.map((m) => (
              <ChatRow
                key={m.seq}
                chatId={chatId}
                msg={m}
                result={
                  m.kind === "tool_use"
                    ? toolIndex.resultByUseId.get((m.payload as { id?: string } | null)?.id ?? "")
                    : undefined
                }
                live={live}
                toolUseIds={toolIndex.toolUseIds}
              />
            ))}
          </div>
          {follow.paused && follow.newCount > 0 && (
            <button
              type="button"
              onClick={follow.jumpToBottom}
              className="absolute bottom-3 left-1/2 -translate-x-1/2 rounded-full bg-brand px-3 py-1 text-xs font-medium text-on-brand shadow-lg hover:bg-brand-hover"
            >
              {follow.newCount} new ↓
            </button>
          )}
        </div>
      )}
    </div>
  );
}

function ChatRow({
  chatId,
  msg,
  result,
  live,
  toolUseIds,
}: {
  chatId: string;
  msg: RunMessage;
  result?: RunMessage;
  live: boolean;
  toolUseIds: Set<string>;
}) {
  // A folded tool_result is skipped here — its call renders it inline (RunEvent).
  if (msg.kind === "tool_result") {
    const useId = (msg.payload as { tool_use_id?: string } | null)?.tool_use_id;
    if (useId && toolUseIds.has(useId)) return null;
  }

  if (msg.kind === "user_message") {
    const text = (msg.payload as { text?: string } | null)?.text ?? "";
    return (
      <div className="flex justify-end">
        <div className="max-w-[85%] whitespace-pre-wrap break-words rounded-2xl rounded-br-sm bg-brand/15 px-3 py-2 text-sm text-fg">
          {text}
        </div>
      </div>
    );
  }

  if (msg.kind === "text") {
    const text = (msg.payload as { text?: string } | null)?.text ?? "";
    if (!text) return null;
    return (
      <div className="flex justify-start">
        <div className="docs-prose max-w-[92%] rounded-2xl rounded-bl-sm border border-edge bg-surface/70 px-3 py-2 text-sm">
          <Markdown content={text} />
        </div>
      </div>
    );
  }

  if (msg.kind === "proposal") {
    const proposal = msg.payload as IssueProposal;
    return (
      <div className="flex justify-start">
        <div className="w-full max-w-[92%]">
          <ProposalCard chatId={chatId} proposal={proposal} />
        </div>
      </div>
    );
  }

  if (msg.kind === "run_request") {
    const request = msg.payload as RunRequest;
    return (
      <div className="flex justify-start">
        <div className="w-full max-w-[92%]">
          <RunRequestCard request={request} />
        </div>
      </div>
    );
  }

  if (msg.kind === "cancel_request") {
    const request = msg.payload as CancelRequest;
    return (
      <div className="flex justify-start">
        <div className="w-full max-w-[92%]">
          <CancelRequestCard request={request} />
        </div>
      </div>
    );
  }

  // thinking / tool_use / tool_result(orphan) / status / error: the terse,
  // escaped RunEvent rows, indented as agent-side context (not a prose bubble).
  return (
    <div className={cx("border-l-2 border-edge/60 pl-3", "text-muted")}>
      <RunEventRow msg={msg} result={result} live={live} />
    </div>
  );
}
