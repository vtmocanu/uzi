import { useMemo, useState } from "react";
import type { RunMessage } from "../lib/api";
import { useFollowScroll, useReconnectingBanner } from "../lib/useFollowScroll";
import { buildToolIndex, RunEventRow } from "./RunEvent";
import { Badge } from "./ui";

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

  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between">
        <h2 className="text-sm font-semibold uppercase tracking-wide text-slate-500">Activity</h2>
        <div className="flex items-center gap-2">
          {follow.paused && follow.newCount > 0 && (
            <button
              type="button"
              onClick={follow.jumpToBottom}
              className="rounded-full bg-indigo-500 px-2.5 py-0.5 text-xs font-medium text-white hover:bg-indigo-400"
            >
              {follow.newCount} new ↓
            </button>
          )}
          <span className="text-xs text-slate-500">{messages.length} messages</span>
        </div>
      </div>

      {reconnecting && (
        <div className="rounded-md border border-amber-800 bg-amber-950/50 px-3 py-1.5 text-xs text-amber-300">
          Reconnecting… live updates are paused.
        </div>
      )}

      {groups.length === 0 ? (
        <p className="py-6 text-center text-sm text-slate-600">
          {terminal ? "No messages were recorded for this run." : "Waiting for the agent…"}
        </p>
      ) : (
        <div
          ref={follow.ref}
          onScroll={follow.onScroll}
          role="log"
          aria-live="polite"
          aria-label="Run activity"
          className="max-h-[65vh] space-y-3 overflow-auto rounded-lg border border-slate-800 bg-slate-900/60 p-3 [overscroll-behavior:contain]"
        >
          {capped && (
            <button
              type="button"
              onClick={() => setShowAll(true)}
              className="w-full rounded-md border border-slate-800 bg-slate-900 px-3 py-1.5 text-xs text-slate-400 hover:text-slate-200"
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
}: {
  group: AgentGroup;
  live: boolean;
  toolIndex: ReturnType<typeof buildToolIndex>;
}) {
  return (
    <div className="rounded-lg border border-slate-800 bg-slate-900/40 p-3">
      <div className="mb-2 flex items-center gap-2">
        <span className="text-sm font-semibold text-slate-200">{group.agent}</span>
        <Badge tone={live ? "warning" : "neutral"} title={live ? "Most recent activity" : "Idle"}>
          {live ? "active" : "idle"}
        </Badge>
        <span className="ml-auto text-[11px] text-slate-600" title={group.startedAt}>
          {group.startedAt ? new Date(group.startedAt).toLocaleTimeString() : ""}
        </span>
      </div>
      <div className="space-y-2">
        {group.messages.map((m) => {
          // Fold a tool_result under its call (matched by id); skip it here.
          if (m.kind === "tool_result") {
            const useId = (m.payload as { tool_use_id?: string } | null)?.tool_use_id;
            if (useId && toolIndex.toolUseIds.has(useId)) return null;
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
