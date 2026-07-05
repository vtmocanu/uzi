import { useMemo, useState } from "react";
import type { RunMessage } from "../lib/api";
import { useFollowScroll, useReconnectingBanner } from "../lib/useFollowScroll";
import { buildToolIndex, RunEventRow } from "./RunEvent";
import { Badge, cx } from "./ui";

// Each agent gets a stable accent so consecutive blocks are scannable — the
// same role that avatars + status colors play on multica's message lists
// (packages/views/chat/components/chat-message-list.tsx uses ActorAvatar).
const AGENT_ACCENTS = [
  "text-brand border-brand/40",
  "text-info border-info/40",
  "text-ok border-ok/40",
  "text-warn border-warn/40",
  "text-danger border-danger/40",
];

function agentAccent(agent: string): string {
  let h = 0;
  for (let i = 0; i < agent.length; i++) h = (h * 31 + agent.charCodeAt(i)) | 0;
  return AGENT_ACCENTS[Math.abs(h) % AGENT_ACCENTS.length];
}

// agentGroup is a run of consecutive messages produced by the same agent.
interface AgentGroup {
  agent: string;
  messages: RunMessage[];
  startedAt: string;
}

function groupByAgent(messages: RunMessage[]): AgentGroup[] {
  const groups: AgentGroup[] = [];
  for (const m of messages) {
    const agent = m.agent ?? "lead";
    const last = groups[groups.length - 1];
    if (last && last.agent === agent) last.messages.push(m);
    else groups.push({ agent, messages: [m], startedAt: m.created_at });
  }
  return groups;
}

// Keep the DOM bounded on very long runs: past the trigger, render only the most
// recent VISIBLE messages behind a "show earlier" expander (cap-and-expand,
// chosen over a virtualization dependency for this MVP).
const CAP_TRIGGER = 1000;
const CAP_VISIBLE = 500;

export function ActivityFeed({
  messages,
  runningLive,
  connected,
  terminal,
}: {
  messages: RunMessage[];
  runningLive: boolean;
  connected: boolean;
  terminal: boolean;
}) {
  const [showAll, setShowAll] = useState(false);
  const follow = useFollowScroll(messages.length);
  const reconnecting = useReconnectingBanner(connected);

  const toolIndex = useMemo(() => buildToolIndex(messages), [messages]);

  const capped = messages.length > CAP_TRIGGER && !showAll;
  const hiddenCount = capped ? messages.length - CAP_VISIBLE : 0;
  const visible = capped ? messages.slice(-CAP_VISIBLE) : messages;
  const groups = useMemo(() => groupByAgent(visible), [visible]);
  const activeAgent = runningLive ? groups[groups.length - 1]?.agent : undefined;

  // A tool_result is fold-skipped only when its call is ALSO in the visible
  // window. Result pairing uses the full-message index (so a visible call still
  // folds its result), but the skip decision is scoped to `visible`: otherwise a
  // call capped out past the ~500-message cap while its result stays visible
  // would skip the result AND never render its call, vanishing it from the view.
  const visibleToolUseIds = useMemo(() => {
    const ids = new Set<string>();
    for (const m of visible) {
      if (m.kind === "tool_use") {
        const id = (m.payload as { id?: string } | null)?.id;
        if (id) ids.add(id);
      }
    }
    return ids;
  }, [visible]);

  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between">
        <h2 className="text-xs font-semibold uppercase tracking-wider text-faint">Activity</h2>
        <div className="flex items-center gap-2">
          {follow.paused && follow.newCount > 0 && (
            <button
              type="button"
              onClick={follow.jumpToBottom}
              className="rounded-full bg-brand px-2.5 py-0.5 text-xs font-medium text-on-brand hover:bg-brand-hover"
            >
              {follow.newCount} new ↓
            </button>
          )}
          <span className="text-xs text-faint">{messages.length} messages</span>
        </div>
      </div>

      {reconnecting && (
        <div className="rounded-md border border-warn/40 bg-warn/10 px-3 py-1.5 text-xs text-warn">
          Reconnecting… live updates are paused.
        </div>
      )}

      {groups.length === 0 ? (
        <p className="py-6 text-center text-sm text-faint">
          {terminal ? "No messages were recorded for this run." : "Waiting for the agent…"}
        </p>
      ) : (
        <div
          ref={follow.ref}
          onScroll={follow.onScroll}
          role="log"
          aria-live="polite"
          aria-label="Run activity"
          className="max-h-[65vh] space-y-3 overflow-auto rounded-lg border border-edge bg-ink/60 p-3 [overscroll-behavior:contain]"
        >
          {capped && (
            <button
              type="button"
              onClick={() => setShowAll(true)}
              className="w-full rounded-md border border-edge bg-raised px-3 py-1.5 text-xs text-muted hover:text-fg"
            >
              Show {hiddenCount} earlier messages
            </button>
          )}
          {groups.map((g, i) => (
            <AgentBlock
              key={`${g.agent}-${g.messages[0]?.seq ?? i}`}
              group={g}
              live={g.agent === activeAgent}
              toolIndex={toolIndex}
              visibleToolUseIds={visibleToolUseIds}
            />
          ))}
        </div>
      )}
    </div>
  );
}

function AgentBlock({
  group,
  live,
  toolIndex,
  visibleToolUseIds,
}: {
  group: AgentGroup;
  live: boolean;
  toolIndex: ReturnType<typeof buildToolIndex>;
  visibleToolUseIds: Set<string>;
}) {
  const accent = agentAccent(group.agent);
  return (
    <div className={cx("rounded-lg border-l-2 bg-surface/60 py-3 pl-3 pr-3", accent)}>
      <div className="mb-2 flex items-center gap-2">
        <span className={cx("text-sm font-semibold", accent.split(" ")[0])}>{group.agent}</span>
        <Badge tone={live ? "warning" : "neutral"} title={live ? "Most recent activity" : "Idle"}>
          {live ? "active" : "idle"}
        </Badge>
        <span className="ml-auto text-[11px] text-faint" title={group.startedAt}>
          {group.startedAt ? new Date(group.startedAt).toLocaleTimeString() : ""}
        </span>
      </div>
      <div className="space-y-2">
        {group.messages.map((m) => {
          // Fold a tool_result under its call (matched by id); skip it here — but
          // only when the call is in the visible window, else render it standalone
          // (its call was capped out, so nothing else would show the result).
          if (m.kind === "tool_result") {
            const useId = (m.payload as { tool_use_id?: string } | null)?.tool_use_id;
            if (useId && visibleToolUseIds.has(useId)) return null;
          }
          const result =
            m.kind === "tool_use"
              ? toolIndex.resultByUseId.get((m.payload as { id?: string } | null)?.id ?? "")
              : undefined;
          return <RunEventRow key={m.seq} msg={m} result={result} live={live} />;
        })}
      </div>
    </div>
  );
}
