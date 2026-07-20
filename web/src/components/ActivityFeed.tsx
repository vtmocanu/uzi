import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import type { Run, RunMessage } from "../lib/api";
import type { PhaseUsage } from "../lib/runUsage";
import { useReconnectingBanner, useTailOnAppend } from "../lib/useFollowScroll";
import {
  buildToolIndex,
  describeError,
  describeStatus,
  RunEventRow,
  toolSummary,
  truncate,
} from "./RunEvent";
import { ChevronRightIcon } from "./icons";
import { cx } from "./ui";

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
// hidden "message".
function isEmptyText(m: RunMessage): boolean {
  if (m.kind !== "text") return false;
  const t = (m.payload as { text?: unknown } | null)?.text;
  return typeof t !== "string" || t === "";
}

// ── Crew roster (PRD #95 Decision 2) ──────────────────────────────────────────
// The per-agent state ladder derived CLIENT-SIDE from run.status + run.health +
// message recency — no new backend (the pane already has all three on the wire).
type CrewState = "working" | "stalled" | "waiting" | "idle" | "done";

// Recency only governs the NON-active waiting↔idle split. It never gates the active
// speaker's `working` (B2): with the server stall flag defaulting to 300s, a long tool
// call between 45s and 300s must still read working, so the active speaker trusts
// run.health, not this timer.
const STALE_MS = 45_000;

// looping/slow/stalled are PRD #47 WARN flags → amber `stalled`, never green `working`
// (a looping agent is spinning without progress; it must not read as healthy).
const STALLED_HEALTH = new Set<string>(["stalled", "slow", "looping"]);

// crewStateFor is the Decision-2 ladder. Precedence: terminal → gate/worker-wait →
// active-speaker (health, NOT recency) → non-active recency split.
function crewStateFor(
  run: Run,
  agent: string,
  activeAgent: string | undefined,
  lastActivityMs: number,
  now: number,
): CrewState {
  if (isTerminalStatus(run.status)) return "done";
  // Gate / no-live-worker dominates the whole crew: everyone is blocked.
  if (run.status === "awaiting_approval" || run.health === "waiting_worker") return "waiting";
  if (agent === activeAgent) {
    return STALLED_HEALTH.has(run.health) ? "stalled" : "working";
  }
  // Non-active agent: recency is the ONLY signal we have for "handed off" vs "quiet"
  // (no SDK handoff event), and it is a cosmetic split — never touches the exact states.
  return now - lastActivityMs < STALE_MS ? "waiting" : "idle";
}

function isTerminalStatus(status: string): boolean {
  return status === "completed" || status === "failed" || status === "cancelled";
}

// Presentation: the dot inherits the chip's tone via bg-current (mirrors Badge). The
// `working` dot pulses — index.css neutralizes animate-pulse under prefers-reduced-motion,
// so the pulse is honored there for free.
const CREW_TONE: Record<CrewState, string> = {
  working: "text-ok",
  stalled: "text-warn",
  waiting: "text-info",
  idle: "text-faint",
  done: "text-muted",
};

// agentOneLiner is the live header summary ("Running go build", "Reading files",
// "Thinking…") that updates IN PLACE from the agent's newest message — the thing you
// read instead of expanding the log (Problem 1). Reuses RunEvent's own describers.
function agentOneLiner(latest: RunMessage | undefined): string {
  if (!latest) return "";
  switch (latest.kind) {
    case "tool_use": {
      const p = latest.payload as { name?: string; input?: unknown } | null;
      return toolSummary(p?.name, p?.input);
    }
    case "tool_result":
      return "Working";
    case "thinking":
      return "Thinking…";
    case "text": {
      const t = (latest.payload as { text?: string } | null)?.text ?? "";
      return truncate((t.split("\n")[0] ?? "").trim(), 80);
    }
    case "status":
      return describeStatus(latest.payload);
    case "error":
      return `Error: ${describeError(latest.payload)}`;
    case "plan":
      return "Submitted a plan";
    default:
      return "";
  }
}

// relativeTime renders an ISO instant as a coarse "time ago" for the block header
// (the absolute value lives in the element's title).
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

// useNow ticks a coarse clock so relative timestamps + the recency split age without
// the feed needing a re-render from elsewhere. The active speaker's `working` never
// depends on this tick (it trusts run.health), so the coarse cadence is harmless.
function useNow(intervalMs: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), intervalMs);
    return () => window.clearInterval(id);
  }, [intervalMs]);
  return now;
}

// Keep the DOM bounded on very long runs: past the trigger, render only the most
// recent VISIBLE messages behind a "show earlier" expander (cap-and-expand). Untouched
// by the M2 follow-scroll refactor (PRD #95 N4).
const CAP_TRIGGER = 1000;
const CAP_VISIBLE = 500;

// The announced string is untrusted and unbounded, and aria-atomic re-reads the whole
// thing on every change — so cap it (PRD #38 M4 audit).
const ANNOUNCE_MAX = 200;

export function ActivityFeed({
  messages,
  run,
  runningLive,
  connected,
  terminal,
  phaseUsageBySeq,
}: {
  messages: RunMessage[];
  // The full run (PRD #95 M1 seam). M2 consumes it: the crew roster reads run.health /
  // run.status to derive each agent's state (Decision 2).
  run: Run;
  runningLive: boolean;
  connected: boolean;
  terminal: boolean;
  // PRD #40: per-result-frame token/cost deltas keyed by seq. Optional.
  phaseUsageBySeq?: Map<number, PhaseUsage>;
}) {
  const [showAll, setShowAll] = useState(false);
  // Opt-in Follow (Decision 3), default OFF — replaces the old whole-pane auto-jump.
  // When on, each EXPANDED agent body tails its own bounded scroll region.
  const [followLive, setFollowLive] = useState(false);
  const reconnecting = useReconnectingBanner(connected);
  const now = useNow(30_000);

  const toolIndex = useMemo(() => buildToolIndex(messages), [messages]);

  const capped = messages.length > CAP_TRIGGER && !showAll;
  const hiddenCount = capped ? messages.length - CAP_VISIBLE : 0;
  const visible = capped ? messages.slice(-CAP_VISIBLE) : messages;
  const groups = useMemo(() => groupByAgent(visible), [visible]);

  // Per-agent aggregates for the crew roster: first-seen order, message count, newest
  // message (for the one-liner), and last-activity time (for the recency split).
  const crew = useMemo(() => {
    const order: string[] = [];
    const count = new Map<string, number>();
    const latest = new Map<string, RunMessage>();
    const lastMs = new Map<string, number>();
    for (const g of groups) {
      for (const mm of g.messages) {
        const a = mm.agent ?? "lead";
        if (!count.has(a)) order.push(a);
        count.set(a, (count.get(a) ?? 0) + 1);
        const prev = latest.get(a);
        if (!prev || mm.seq > prev.seq) latest.set(a, mm);
        const t = new Date(mm.created_at).getTime();
        if (Number.isFinite(t)) lastMs.set(a, Math.max(lastMs.get(a) ?? 0, t));
      }
    }
    return { order, count, latest, lastMs };
  }, [groups]);

  // The active speaker is the agent of the newest message while the run is live
  // (claimed/running). Drives the crew `working`/`stalled` dot AND the per-row live
  // styling. Under a gate the crew ladder makes everyone `waiting` regardless.
  const newestAgent = groups.length ? groups[groups.length - 1].agent : undefined;
  const liveSpeaker =
    !terminal && (run.status === "running" || run.status === "claimed") ? newestAgent : undefined;
  // activeAgent drives RunEventRow's running-tool spinner; keep it running-only (the
  // pre-v2 behavior) so a claimed-but-not-running run shows no spinner.
  const activeAgent = runningLive ? newestAgent : undefined;

  // ── Collapse-by-default with a finished/single-agent auto-expand escape (S5) ──
  // Default: collapsed. Auto-expand a terminal or single-agent run so reading a done
  // 8-agent run is not death-by-clicks. A per-agent override (user click) always wins;
  // Expand all / Collapse all sets a bulk mode and clears overrides.
  const [overrides, setOverrides] = useState<Map<string, boolean>>(() => new Map());
  const [bulk, setBulk] = useState<"none" | "all" | "collapsed">("none");
  const autoExpand = terminal || crew.order.length <= 1;
  const isExpanded = (agent: string): boolean => {
    const o = overrides.get(agent);
    if (o !== undefined) return o;
    if (bulk === "all") return true;
    if (bulk === "collapsed") return false;
    return autoExpand;
  };
  const toggleAgent = (agent: string) =>
    setOverrides((prev) => new Map(prev).set(agent, !isExpanded(agent)));
  const expandAll = () => {
    setBulk("all");
    setOverrides(new Map());
  };
  const collapseAll = () => {
    setBulk("collapsed");
    setOverrides(new Map());
  };
  const jumpToAgent = (agent: string) => {
    setOverrides((prev) => new Map(prev).set(agent, true));
    document.getElementById(`agent-anchor-${agent}`)?.scrollIntoView?.({ block: "start" });
  };
  const allExpanded = crew.order.length > 0 && crew.order.every(isExpanded);

  // ── `+N` unseen-while-collapsed pill (a genuine rewrite, N4) ──────────────────
  // Per agent, the count of messages that arrived while the agent was COLLAPSED. Seen
  // baselines are held in a ref, synced after render: an agent is "caught up" to its
  // current count while EXPANDED (so reopening clears the pill) and on first sight (a
  // freshly-appeared collapsed agent starts at 0, then accrues).
  const seenRef = useRef<Map<string, number>>(new Map());
  useEffect(() => {
    const seen = seenRef.current;
    for (const a of crew.order) {
      const cur = crew.count.get(a) ?? 0;
      if (!seen.has(a) || isExpanded(a)) seen.set(a, cur);
    }
  });
  const unseenFor = (agent: string): number => {
    if (isExpanded(agent)) return 0;
    const seen = seenRef.current.get(agent);
    if (seen === undefined) return 0;
    return Math.max(0, (crew.count.get(agent) ?? 0) - seen);
  };

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

  // One polite live region carries only MEANINGFUL narration (Decision 8): a handoff,
  // agent started/finished, a plan, an error, or the run finishing — never a per-tool
  // frame. Unchanged from PRD #38.
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

  const anchored = new Set<string>();

  return (
    <div className="space-y-2">
      <div aria-live="polite" aria-atomic="true" className="sr-only">
        {announcement}
      </div>

      <div className="flex items-center justify-between gap-2">
        <h2 className="text-xs font-semibold uppercase tracking-wider text-faint">Activity</h2>
        <div className="flex items-center gap-3">
          {crew.order.length > 1 && (
            <button
              type="button"
              onClick={allExpanded ? collapseAll : expandAll}
              className="text-xs text-muted hover:text-fg"
            >
              {allExpanded ? "Collapse all" : "Expand all"}
            </button>
          )}
          {/* Follow live — opt-in, default off (Decision 3). Replaces the whole-pane yank. */}
          <label className="inline-flex cursor-pointer items-center gap-1.5 text-xs text-muted">
            <input
              type="checkbox"
              checked={followLive}
              onChange={(e) => setFollowLive(e.target.checked)}
              className="h-3 w-3 accent-brand"
            />
            Follow live
          </label>
          <span className="text-xs text-muted">{messages.length} messages</span>
        </div>
      </div>

      {/* Crew roster strip: who's alive, at a glance (Problem 2). */}
      {crew.order.length === 0
        ? !terminal && (
            <p className="rounded-lg border border-edge bg-ink/40 px-3 py-2 text-xs text-muted">
              Waiting for the first agent…
            </p>
          )
        : (
            <div aria-label="Crew" className="flex flex-wrap gap-1.5">
              {crew.order.map((a) => {
                const state = crewStateFor(run, a, liveSpeaker, crew.lastMs.get(a) ?? 0, now);
                return (
                  <button
                    key={a}
                    type="button"
                    onClick={() => jumpToAgent(a)}
                    aria-label={`Jump to ${a} activity (${state})`}
                    title={`${a}: ${state}`}
                    className={cx(
                      "inline-flex items-center gap-1.5 rounded-full border border-edge bg-raised px-2.5 py-0.5 text-xs hover:border-edge-strong",
                      CREW_TONE[state],
                    )}
                  >
                    <span
                      aria-hidden="true"
                      className={cx(
                        "h-1.5 w-1.5 rounded-full bg-current",
                        state === "working" && "animate-pulse",
                      )}
                    />
                    <span className="font-medium text-fg">{a}</span>
                    <span className="text-[10px] uppercase tracking-wide opacity-80">{state}</span>
                  </button>
                );
              })}
            </div>
          )}

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
          role="log"
          // role="log" implies aria-live="polite"; force it OFF so tool frames are not
          // announced. Meaningful transitions route through the sr-only region above.
          aria-live="off"
          aria-label="Run activity"
          className="space-y-3 rounded-lg border border-edge bg-ink/60 p-3"
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
            const firstBlock = !anchored.has(g.agent);
            if (firstBlock) anchored.add(g.agent);
            return (
              <AgentBlock
                key={`${g.agent}-${firstSeq}`}
                anchorId={firstBlock ? `agent-anchor-${g.agent}` : undefined}
                bodyId={`agent-body-${firstSeq}`}
                group={g}
                live={g.agent === activeAgent}
                expanded={isExpanded(g.agent)}
                followLive={followLive}
                oneLiner={firstBlock ? agentOneLiner(crew.latest.get(g.agent)) : ""}
                unseen={firstBlock ? unseenFor(g.agent) : 0}
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
  expanded,
  followLive,
  oneLiner,
  unseen,
  onToggle,
  now,
  anchorId,
  bodyId,
  toolIndex,
  visibleToolUseIds,
  phaseUsageBySeq,
}: {
  group: AgentGroup;
  live: boolean;
  expanded: boolean;
  followLive: boolean;
  oneLiner: string;
  unseen: number;
  onToggle: () => void;
  now: number;
  anchorId?: string;
  bodyId: string;
  toolIndex: ReturnType<typeof buildToolIndex>;
  visibleToolUseIds: Set<string>;
  phaseUsageBySeq?: Map<number, PhaseUsage>;
}) {
  const accent = agentAccent(group.agent);
  // Tail this expanded body to its bottom on append, but ONLY while Follow live is on
  // (Decision 3). With Follow off, an append never moves the viewport.
  const bodyRef = useTailOnAppend(group.messages.length, followLive && expanded);

  // Build the body as a flow of full-width rows with any RUN of consecutive rail-kind
  // rows (tool_use, thinking, orphan tool_result) gathered into ONE bordered rail.
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
    if (isEmptyText(m)) continue;
    if (m.kind === "tool_result") {
      const useId = (m.payload as { tool_use_id?: string } | null)?.tool_use_id;
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
    flushRail();
    rows.push(<RunEventRow key={m.seq} msg={m} live={live} phaseUsage={phaseUsageBySeq?.get(m.seq)} />);
  }
  flushRail();

  return (
    <div id={anchorId} className={cx("rounded-lg border-l-2 bg-surface/60 py-3 pl-3 pr-3", accent)}>
      <div className={cx("flex items-center gap-2", expanded && "mb-2")}>
        <button
          type="button"
          onClick={onToggle}
          aria-expanded={expanded}
          aria-controls={bodyId}
          aria-label={`${expanded ? "Collapse" : "Expand"} ${group.agent} activity`}
          className="-ml-1 inline-flex h-6 w-6 shrink-0 items-center justify-center rounded text-muted transition-colors hover:bg-raised/60 hover:text-fg"
        >
          <span
            aria-hidden="true"
            className={cx("inline-flex transition-transform", expanded && "rotate-90")}
          >
            <ChevronRightIcon />
          </span>
        </button>
        <span className={cx("shrink-0 text-sm font-semibold", accent.split(" ")[0])}>{group.agent}</span>
        {/* The live one-liner updates IN PLACE (no scroll) — the thing you read instead
            of expanding the log. Only on the agent's first block, showing its newest. */}
        {oneLiner && <span className="min-w-0 truncate text-xs text-muted">{oneLiner}</span>}
        {!expanded && unseen > 0 && (
          <span
            title={`${unseen} new since you last looked`}
            className="shrink-0 rounded-full bg-brand/15 px-1.5 py-0.5 text-[10px] font-medium text-brand"
          >
            +{unseen}
          </span>
        )}
        <span className="ml-auto shrink-0 text-[11px] tabular-nums text-faint" title={group.startedAt}>
          {relativeTime(group.startedAt, now)}
        </span>
      </div>
      <div
        id={bodyId}
        ref={bodyRef}
        hidden={!expanded}
        className="max-h-[45vh] space-y-2 overflow-auto [overscroll-behavior:contain]"
      >
        {rows}
      </div>
    </div>
  );
}
