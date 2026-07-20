import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import type { Run, RunMessage } from "../lib/api";
import type { PhaseUsage } from "../lib/runUsage";
import { useFollowScroll, useReconnectingBanner } from "../lib/useFollowScroll";
import { buildToolIndex, describeError, describeStatus, RunEventRow, truncate } from "./RunEvent";
import { ChevronRightIcon } from "./icons";
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

// isEmptyText marks a text message that renders to nothing (RunEventRow returns
// null for empty/blank text): it must neither split the tool rail nor count as a
// hidden "message" in a collapsed block's summary (PRD #38 M4 review).
function isEmptyText(m: RunMessage): boolean {
  if (m.kind !== "text") return false;
  const t = (m.payload as { text?: unknown } | null)?.text;
  return typeof t !== "string" || t === "";
}

// hiddenSummary describes what a collapsed block hides: prose/meta rows counted
// as "messages", tool calls counted separately. A folded tool_result (its call is
// visible, so it renders under the call rather than as its own row) is not a row,
// so it is not counted — the summary reflects what actually disappears.
function hiddenSummary(group: AgentGroup, visibleToolUseIds: Set<string>): string {
  let tools = 0;
  let msgs = 0;
  for (const m of group.messages) {
    if (isEmptyText(m)) continue;
    if (m.kind === "tool_use") {
      tools++;
      continue;
    }
    if (m.kind === "tool_result") {
      const useId = (m.payload as { tool_use_id?: string } | null)?.tool_use_id;
      if (useId && visibleToolUseIds.has(useId)) continue;
    }
    msgs++;
  }
  const msgPart = `${msgs} ${msgs === 1 ? "message" : "messages"}`;
  const toolPart = `${tools} tool ${tools === 1 ? "call" : "calls"}`;
  return `${msgPart}, ${toolPart} hidden`;
}

// relativeTime renders an ISO instant as a coarse "time ago" for the block header
// (the absolute value lives in the element's title). Coarse on purpose — the feed
// reads as a narrative, not a precise log.
function relativeTime(iso: string, now: number): string {
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return "";
  const s = Math.max(0, Math.floor((now - t) / 1000));
  if (s < 60) return "just now";
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

// useNow ticks a coarse clock so relative timestamps age without the feed needing
// a re-render from elsewhere (e.g. a terminal run left open). One interval for the
// whole feed; settled RunEventRow rows are memoized so a tick is cheap.
function useNow(intervalMs: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), intervalMs);
    return () => window.clearInterval(id);
  }, [intervalMs]);
  return now;
}

// Keep the DOM bounded on very long runs: past the trigger, render only the most
// recent VISIBLE messages behind a "show earlier" expander (cap-and-expand,
// chosen over a virtualization dependency for this MVP).
const CAP_TRIGGER = 1000;
const CAP_VISIBLE = 500;

// The announced string is untrusted and unbounded (agent names, describeStatus/
// describeError text), and aria-atomic="true" makes AT re-read the whole thing on
// every change — so cap it (PRD #38 M4 audit).
const ANNOUNCE_MAX = 200;

export function ActivityFeed({
  messages,
  runningLive,
  connected,
  terminal,
  phaseUsageBySeq,
}: {
  messages: RunMessage[];
  // run is threaded in by the M1 seam (PRD #95 Decision 9) for M2's crew roster, which
  // reads run.health / run.status to derive the per-agent state ladder. Optional and
  // NOT yet consumed here — M2 destructures it and builds the roster; landing it now is
  // what keeps M2 a change to THIS file only, never to RunView.
  run?: Run;
  runningLive: boolean;
  connected: boolean;
  terminal: boolean;
  // PRD #40: per-result-frame token/cost deltas keyed by seq, so a result frame's
  // finish line can show its phase's usage. Optional — omitted, finish lines render
  // exactly as before.
  phaseUsageBySeq?: Map<number, PhaseUsage>;
}) {
  const [showAll, setShowAll] = useState(false);
  // Agent collapse is keyed by agent NAME, not by block: groupByAgent emits a
  // fresh block whenever the speaker changes, so lead→worker→lead yields two
  // lead blocks — collapsing lead must mute BOTH (Decision 7). Per-run client
  // state, never persisted; a new message from a collapsed agent does not
  // auto-expand it (this Set is untouched by appends) but still counts in the
  // follow pill (useFollowScroll sees the full, unfiltered messages.length).
  const [collapsed, setCollapsed] = useState<Set<string>>(() => new Set());
  const follow = useFollowScroll(messages.length);
  const reconnecting = useReconnectingBanner(connected);
  const now = useNow(30_000);

  // Collapsing/expanding changes scroll height, which can strand a following user
  // mid-feed or silently flip isNearBottom. Capture "were we following?" at click
  // time, then re-arm the tail AFTER the height change lands (layout effect, keyed
  // on the collapse Set) — but only if we were following, so a scrolled-up reader
  // is left in place (Decision 7).
  const rearmRef = useRef(false);
  const toggleAgent = useCallback(
    (agent: string) => {
      rearmRef.current = !follow.paused;
      setCollapsed((prev) => {
        const next = new Set(prev);
        if (next.has(agent)) next.delete(agent);
        else next.add(agent);
        return next;
      });
    },
    [follow.paused],
  );
  useLayoutEffect(() => {
    if (!rearmRef.current) return;
    rearmRef.current = false;
    follow.jumpToBottom();
  }, [collapsed, follow.jumpToBottom]);

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

  // One polite live region carries only MEANINGFUL narration (Decision 8): an
  // agent handoff, agent started/finished, a plan submitted, an error, or the run
  // finishing — never a per-tool frame. The scroll container's own role="log" is
  // muted to aria-live="off" (below), so a burst of tool_use/tool_result/text/
  // thinking appends leaves this string untouched and screen readers hear the
  // story, not every shell command.
  const [announcement, setAnnouncement] = useState("");
  const announcedRef = useRef({ agent: undefined as string | undefined, seq: 0, terminal: false });
  useEffect(() => {
    const seen = announcedRef.current;
    let next: string | null = null;

    if (activeAgent && activeAgent !== seen.agent) next = `${activeAgent} is now active`;
    seen.agent = activeAgent;

    let meaningful: RunMessage | undefined;
    for (const mm of messages) {
      if (mm.kind === "status" || mm.kind === "error" || mm.kind === "plan") meaningful = mm;
    }
    if (meaningful && meaningful.seq !== seen.seq) {
      seen.seq = meaningful.seq;
      next =
        meaningful.kind === "error"
          ? `Error: ${describeError(meaningful.payload)}`
          : meaningful.kind === "plan"
            ? "Plan submitted, awaiting approval"
            : describeStatus(meaningful.payload);
    }

    if (terminal && !seen.terminal) next = "Run finished";
    seen.terminal = terminal;

    if (next !== null) setAnnouncement(truncate(next, ANNOUNCE_MAX));
  }, [messages, activeAgent, terminal]);

  return (
    <div className="space-y-2">
      <div aria-live="polite" aria-atomic="true" className="sr-only">
        {announcement}
      </div>
      <div className="flex items-center justify-between">
        <h2 className="text-xs font-semibold uppercase tracking-wider text-faint">Activity</h2>
        <div className="flex items-center gap-2">
          {follow.paused && follow.newCount > 0 && (
            <button
              type="button"
              onClick={follow.jumpToBottom}
              className="inline-flex min-h-[24px] items-center rounded-full bg-brand px-2.5 py-0.5 text-xs font-medium text-on-brand hover:bg-brand-hover"
            >
              {follow.newCount} new ↓
            </button>
          )}
          <span className="text-xs text-muted">{messages.length} messages</span>
        </div>
      </div>

      {reconnecting && (
        <div className="rounded-md border border-warn/40 bg-warn/10 px-3 py-1.5 text-xs text-warn">
          Reconnecting… live updates are paused.
        </div>
      )}

      {groups.length === 0 ? (
        <p className="py-6 text-center text-sm text-muted">
          {terminal ? "No messages were recorded for this run." : "Waiting for the agent…"}
        </p>
      ) : (
        <div
          ref={follow.ref}
          onScroll={follow.onScroll}
          role="log"
          // role="log" implies aria-live="polite"; force it OFF so tool frames are
          // not announced. Meaningful transitions route through the sr-only region
          // above (Decision 8).
          aria-live="off"
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
          {groups.map((g, i) => {
            const firstSeq = g.messages[0]?.seq ?? i;
            return (
              <AgentBlock
                key={`${g.agent}-${firstSeq}`}
                bodyId={`agent-body-${firstSeq}`}
                group={g}
                live={g.agent === activeAgent}
                collapsed={collapsed.has(g.agent)}
                onToggle={() => toggleAgent(g.agent)}
                now={now}
                toolIndex={toolIndex}
                visibleToolUseIds={visibleToolUseIds}
                phaseUsageBySeq={phaseUsageBySeq}
              />
            );
          })}
        </div>
      )}
    </div>
  );
}

function AgentBlock({
  group,
  live,
  collapsed,
  onToggle,
  now,
  bodyId,
  toolIndex,
  visibleToolUseIds,
  phaseUsageBySeq,
}: {
  group: AgentGroup;
  live: boolean;
  collapsed: boolean;
  onToggle: () => void;
  now: number;
  bodyId: string;
  toolIndex: ReturnType<typeof buildToolIndex>;
  visibleToolUseIds: Set<string>;
  phaseUsageBySeq?: Map<number, PhaseUsage>;
}) {
  const accent = agentAccent(group.agent);

  // Build the body as a flow of full-width rows (prose, status divider, error,
  // plan) with any RUN of consecutive rail-kind rows (tool_use, thinking, orphan
  // tool_result) gathered into ONE bordered rail container — the continuous rail
  // of the mock, instead of a per-row border segmented by the space-y gaps (M4).
  const rows: ReactNode[] = [];
  let rail: ReactNode[] = [];
  let railSeq = 0;
  const flushRail = () => {
    if (rail.length === 0) return;
    rows.push(
      <div key={`rail-${railSeq}`} className="space-y-2 border-l border-tool-rail/70 pl-3">
        {rail}
      </div>,
    );
    rail = [];
  };
  for (const m of group.messages) {
    // Empty text renders nothing — drop it so it neither splits the rail nor
    // shows up in a collapsed block's summary count.
    if (isEmptyText(m)) continue;
    if (m.kind === "tool_result") {
      const useId = (m.payload as { tool_use_id?: string } | null)?.tool_use_id;
      // Folded under its visible call (rendered there); skip. Otherwise orphan →
      // stands alone inside the rail.
      if (useId && visibleToolUseIds.has(useId)) continue;
      if (rail.length === 0) railSeq = m.seq;
      rail.push(<RunEventRow key={m.seq} msg={m} live={live} />);
      continue;
    }
    if (m.kind === "tool_use" || m.kind === "thinking") {
      const result =
        m.kind === "tool_use"
          ? toolIndex.resultByUseId.get((m.payload as { id?: string } | null)?.id ?? "")
          : undefined;
      if (rail.length === 0) railSeq = m.seq;
      rail.push(<RunEventRow key={m.seq} msg={m} result={result} live={live} />);
      continue;
    }
    // Full-width kinds (text, status→divider, error, plan, unknown) break the rail.
    // A status/error result frame carries its per-phase usage (undefined otherwise).
    flushRail();
    rows.push(<RunEventRow key={m.seq} msg={m} live={live} phaseUsage={phaseUsageBySeq?.get(m.seq)} />);
  }
  flushRail();

  return (
    <div className={cx("rounded-lg border-l-2 bg-surface/60 py-3 pl-3 pr-3", accent)}>
      <div className={cx("flex items-center gap-2", !collapsed && "mb-2")}>
        <button
          type="button"
          onClick={onToggle}
          aria-expanded={!collapsed}
          aria-controls={bodyId}
          aria-label={`${collapsed ? "Expand" : "Collapse"} ${group.agent} activity`}
          className="-ml-1 inline-flex h-6 w-6 shrink-0 items-center justify-center rounded text-muted transition-colors hover:bg-raised/60 hover:text-fg"
        >
          <span
            aria-hidden="true"
            className={cx("inline-flex transition-transform", !collapsed && "rotate-90")}
          >
            <ChevronRightIcon />
          </span>
        </button>
        <span className={cx("text-sm font-semibold", accent.split(" ")[0])}>{group.agent}</span>
        <Badge
          tone={live ? "warning" : "neutral"}
          dot
          pulse={live}
          title={live ? "Most recent activity" : "Idle"}
        >
          {live ? "active" : "idle"}
        </Badge>
        {collapsed && (
          <span className="min-w-0 truncate text-xs italic text-muted">
            {hiddenSummary(group, visibleToolUseIds)}
          </span>
        )}
        <span
          className="ml-auto shrink-0 text-[11px] tabular-nums text-faint"
          title={group.startedAt}
        >
          {relativeTime(group.startedAt, now)}
        </span>
      </div>
      <div id={bodyId} hidden={collapsed} className="space-y-2">
        {rows}
      </div>
    </div>
  );
}
