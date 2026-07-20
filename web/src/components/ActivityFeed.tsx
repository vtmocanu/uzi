import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import type { Run, RunMessage } from "../lib/api";
import { prefs } from "../lib/prefs";
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

// The role name the worker falls back to when a frame carries no `subagent_type`.
// It is a VALUE, not a marker: see laneRole below for why that distinction bites.
const LEAD = "lead";

// agentGroup is a run of consecutive messages produced by the same agent.
interface AgentGroup {
  agent: string;
  messages: RunMessage[];
  startedAt: string;
}

// groupByAgent is the pre-#99 chronological grouping: a new block on every switch of
// the CONSECUTIVE message author. It is retained, not superseded — the `Timeline` view
// renders exactly this (PRD #99 Decision 2 keeps the raw stream one toggle away), while
// `By agent` uses groupByInstance below.
function groupByAgent(messages: RunMessage[]): AgentGroup[] {
  const groups: AgentGroup[] = [];
  for (const m of messages) {
    const agent = m.agent ?? LEAD;
    const last = groups[groups.length - 1];
    if (last && last.agent === agent) last.messages.push(m);
    else groups.push({ agent, messages: [m], startedAt: m.created_at });
  }
  return groups;
}

// ── Instance lanes (PRD #99) ──────────────────────────────────────────────────
// A lane is one actor's WHOLE contribution, keyed by the subagent invocation rather
// than by turn order, so a lead↔subagent ping-pong stops multiplying near-empty bars
// (Problem 1) and two parallel same-role subagents stay distinct (Problem 2).
interface AgentLane {
  /** agent_instance ?? agent ?? "lead" — the invocation, or the role when there is none. */
  key: string;
  role: string;
  label: string | null;
  messages: RunMessage[];
  startedAt: string;
}

// The lane key falls back to the role, and then to "lead", so pre-migration rows and
// the orchestrator's own turns (both NULL-instance) coalesce into one lane per role.
// An empty-string instance is impossible on the wire — the API stores "" as SQL NULL —
// but `??` would keep one, so nothing here may treat "" as a key.
function laneKeyOf(m: RunMessage): string {
  return m.agent_instance || m.agent || LEAD;
}

function groupByInstance(messages: RunMessage[]): AgentLane[] {
  const byKey = new Map<string, RunMessage[]>();
  const order: string[] = [];
  for (const m of messages) {
    const key = laneKeyOf(m);
    const bucket = byKey.get(key);
    if (bucket) bucket.push(m);
    else {
      byKey.set(key, [m]);
      order.push(key);
    }
  }
  return order.map((key) => {
    const msgs = byKey.get(key) as RunMessage[];
    return {
      key,
      role: laneRole(msgs),
      label: laneLabel(msgs),
      messages: msgs,
      startedAt: msgs[0].created_at,
    };
  });
}

// laneRole takes the first frame naming a NON-lead role, NOT the lane's first frame.
// Why: an SDK replay frame carries `parent_tool_use_id` but no `subagent_type`, and the
// worker's `subagent_type ?? "lead"` fallback therefore stores the literal string "lead"
// on it — so a resumed subagent's lane can OPEN with a lead-looking frame and would be
// titled `lead` while holding a coder's tool rail (PRD #99 Decision 8).
//
// Testing `agent != null` instead would NOT catch that: the absent field is already
// collapsed to "lead" upstream (`agentOf`, agent/src/sdk-messages.ts), never to null.
//
// The fallback is the lane's FIRST role and may legitimately BE "lead": a repo can ship
// `.claude/agents/lead.md`, which registers as an ordinary invocable subagent (no lead
// filter in subagentsFromTemplates), so an all-"lead" lane can be a real invocation with
// a real instance id. That is why this cannot be "the first non-lead role, or bust".
function laneRole(messages: RunMessage[]): string {
  for (const m of messages) {
    if (m.agent && m.agent !== LEAD) return m.agent;
  }
  return messages[0]?.agent ?? LEAD;
}

// laneLabel is INDEPENDENT of laneRole (Decision 1/3): the label is a separate column
// and can be absent for reasons that have nothing to do with the role, so the first
// frame carrying one wins and a lane with none renders the role alone — no `· task`
// suffix, no placeholder.
function laneLabel(messages: RunMessage[]): string | null {
  for (const m of messages) {
    if (m.agent_label) return m.agent_label;
  }
  return null;
}

// The label is model-authored text: it is rendered PLAIN and clamped to a single
// truncated line, never through <Markdown> (Decision 7). The API's 80-rune cap is a
// STORAGE bound (Decision 7a); this is the layout one, and both must exist.
const LABEL_MAX = 48;

function laneLabelText(label: string | null): string {
  if (!label) return "";
  return truncate((label.split("\n")[0] ?? "").trim(), LABEL_MAX);
}

// ── View toggle (Decision 2) ──────────────────────────────────────────────────
// `By agent` is the default; `Timeline` is the escape hatch for anyone debugging a
// cross-agent interleaving, and reproduces the pre-#99 rendering verbatim. The choice
// persists GLOBALLY (not per run) through the typed prefs wrapper — the same one behind
// uzi.sidebar.collapsed and uzi.board.<id>.hideEmpty, so a disabled/full store degrades
// to the default instead of throwing into render.
type ViewMode = "agent" | "timeline";
const VIEW_KEY = "uzi.activity.view";

function storedView(): ViewMode {
  return prefs.get<ViewMode>(VIEW_KEY, "agent") === "timeline" ? "timeline" : "agent";
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
//
// `actor` is an OPAQUE identity compared only for equality against `activeActor`: a
// role name in Timeline view, a lane key (invocation id) in By-agent view. PRD #99
// multiplies the rendering site, not the state model — the ladder itself is unchanged.
function crewStateFor(
  run: Run,
  actor: string,
  activeActor: string | undefined,
  lastActivityMs: number,
  now: number,
): CrewState {
  if (isTerminalStatus(run.status)) return "done";
  // Gate / no-live-worker dominates the whole crew: everyone is blocked.
  if (run.status === "awaiting_approval" || run.health === "waiting_worker") return "waiting";
  if (actor === activeActor) {
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

// Attention priority for the role rollup's single dot (Decision 5): one stalled tester
// must surface over a working one, so the WORST state wins, not the newest.
const STATE_PRIORITY: Record<CrewState, number> = {
  stalled: 0,
  waiting: 1,
  working: 2,
  idle: 3,
  done: 4,
};

// The glance threshold (Decision 5, committed at 6): past this many lanes the collapsed
// lanes stop being scannable as a roster, so the role rollup earns its place. Below it —
// and with no doubled role — there is deliberately NO strip: the lanes ARE the roster,
// and rendering both is the duplication this PRD exists to remove.
const GLANCE_LANES = 6;

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
      // A non-state phrase on purpose: "Working"/"waiting"/"idle" are the crew-CHIP
      // lexicon, so echoing one here read as a contradiction ("coder Ran a tool" is
      // fine, but "coder Working" clashed with a `waiting` chip on a blocked agent).
      return "Ran a tool";
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
    case "plan_revising":
      return "Revising the plan";
    case "plan_feedback":
      return "Sent revision feedback";
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

// Per-actor aggregates: how many messages, the newest one (for the one-liner) and the
// last activity instant (for the recency split).
interface ActorAgg {
  count: number;
  latest: RunMessage | undefined;
  lastMs: number;
}

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
  const [view, setView] = useState<ViewMode>(storedView);
  const reconnecting = useReconnectingBanner(connected);
  const now = useNow(30_000);
  const byAgent = view === "agent";

  const selectView = (next: ViewMode) => {
    setView(next);
    prefs.set(VIEW_KEY, next);
  };

  const toolIndex = useMemo(() => buildToolIndex(messages), [messages]);

  const capped = messages.length > CAP_TRIGGER && !showAll;
  const hiddenCount = capped ? messages.length - CAP_VISIBLE : 0;
  const visible = capped ? messages.slice(-CAP_VISIBLE) : messages;
  const groups = useMemo(() => groupByAgent(visible), [visible]);
  const lanes = useMemo(() => groupByInstance(visible), [visible]);

  // Per-agent aggregates for the crew roster: first-seen order, message count, newest
  // message (for the one-liner), and last-activity time (for the recency split).
  const crew = useMemo(() => {
    const order: string[] = [];
    const count = new Map<string, number>();
    const latest = new Map<string, RunMessage>();
    const lastMs = new Map<string, number>();
    for (const g of groups) {
      for (const mm of g.messages) {
        const a = mm.agent ?? LEAD;
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

  // The same aggregates per LANE. Kept separate from `crew` rather than parameterised:
  // the two key spaces coexist (a view flip must not recompute the other's map), and
  // Timeline's roster is unchanged by this PRD.
  const laneAgg = useMemo(() => {
    const map = new Map<string, ActorAgg>();
    for (const l of lanes) {
      let latest: RunMessage | undefined;
      let lastMs = 0;
      for (const mm of l.messages) {
        if (!latest || mm.seq > latest.seq) latest = mm;
        const t = new Date(mm.created_at).getTime();
        if (Number.isFinite(t)) lastMs = Math.max(lastMs, t);
      }
      map.set(l.key, { count: l.messages.length, latest, lastMs });
    }
    return map;
  }, [lanes]);

  // The active speaker is the actor of the newest message while the run is live
  // (claimed/running). Drives the `working`/`stalled` dot AND the per-row live styling.
  // Under a gate the crew ladder makes everyone `waiting` regardless. Keyed by ROLE for
  // the Timeline roster and by LANE for the lane dots, so exactly one lane pulses even
  // when two invocations of one role are running.
  const newestMsg = visible.length ? visible[visible.length - 1] : undefined;
  const newestAgent = newestMsg ? (newestMsg.agent ?? LEAD) : undefined;
  const newestLane = newestMsg ? laneKeyOf(newestMsg) : undefined;
  const live = !terminal && (run.status === "running" || run.status === "claimed");
  const liveSpeaker = live ? newestAgent : undefined;
  const liveLane = live ? newestLane : undefined;
  // activeAgent drives RunEventRow's running-tool spinner; keep it running-only (the
  // pre-v2 behavior) so a claimed-but-not-running run shows no spinner.
  const activeAgent = runningLive ? newestAgent : undefined;
  const activeLane = runningLive ? newestLane : undefined;

  // ── Collapse-by-default with a finished/single-agent auto-expand escape (S5) ──
  // Default: collapsed. Auto-expand a terminal or single-actor run so reading a done
  // 8-agent run is not death-by-clicks. A per-actor override (user click) always wins;
  // Expand all / Collapse all sets a bulk mode and clears overrides.
  //
  // Overrides are keyed by the CURRENT view's actor key (lane key vs role name). The two
  // key spaces overlap only where a lane has no instance and is keyed by its role, which
  // is the case where carrying the choice across a view flip is the right answer anyway.
  const [overrides, setOverrides] = useState<Map<string, boolean>>(() => new Map());
  const [bulk, setBulk] = useState<"none" | "all" | "collapsed">("none");
  const actorKeys = byAgent ? lanes.map((l) => l.key) : crew.order;
  const countFor = (key: string): number =>
    byAgent ? (laneAgg.get(key)?.count ?? 0) : (crew.count.get(key) ?? 0);
  const autoExpand = terminal || actorKeys.length <= 1;
  const isExpanded = (key: string): boolean => {
    const o = overrides.get(key);
    if (o !== undefined) return o;
    if (bulk === "all") return true;
    if (bulk === "collapsed") return false;
    return autoExpand;
  };
  const toggleActor = (key: string) =>
    setOverrides((prev) => new Map(prev).set(key, !isExpanded(key)));
  const expandAll = () => {
    setBulk("all");
    setOverrides(new Map());
  };
  const collapseAll = () => {
    setBulk("collapsed");
    setOverrides(new Map());
  };
  const jumpTo = (key: string) => {
    setOverrides((prev) => new Map(prev).set(key, true));
    document.getElementById(`agent-anchor-${key}`)?.scrollIntoView?.({ block: "start" });
  };
  const allExpanded = actorKeys.length > 0 && actorKeys.every(isExpanded);

  // ── `+N` unseen-while-collapsed pill (a genuine rewrite, N4) ──────────────────
  // Per actor, the count of messages that arrived while it was COLLAPSED. Seen baselines
  // are held in a ref, synced after render: an actor is "caught up" to its current count
  // while EXPANDED (so reopening clears the pill) and on first sight (a freshly-appeared
  // collapsed actor starts at 0, then accrues).
  const seenRef = useRef<Map<string, number>>(new Map());
  useEffect(() => {
    const seen = seenRef.current;
    for (const key of actorKeys) {
      const cur = countFor(key);
      if (!seen.has(key) || isExpanded(key)) seen.set(key, cur);
    }
  });
  const unseenFor = (key: string): number => {
    if (isExpanded(key)) return 0;
    const seen = seenRef.current.get(key);
    if (seen === undefined) return 0;
    return Math.max(0, countFor(key) - seen);
  };

  // ── Conditional role rollup (Decision 5) ──────────────────────────────────────
  // One chip per ROLE, not per instance: a summary tier the per-instance lanes cannot
  // provide. It appears ONLY when a role is doubled or the lanes overflow the glance —
  // otherwise the collapsed lanes are the roster and there is no strip at all.
  const roles = useMemo(() => {
    const order: string[] = [];
    const count = new Map<string, number>();
    for (const l of lanes) {
      if (!count.has(l.role)) order.push(l.role);
      count.set(l.role, (count.get(l.role) ?? 0) + 1);
    }
    return { order, count };
  }, [lanes]);
  const showRollup =
    lanes.length > GLANCE_LANES || roles.order.some((r) => (roles.count.get(r) ?? 0) >= 2);
  const worstStateFor = (role: string): CrewState => {
    let worst: CrewState = "done";
    for (const l of lanes) {
      if (l.role !== role) continue;
      const s = crewStateFor(run, l.key, liveLane, laneAgg.get(l.key)?.lastMs ?? 0, now);
      if (STATE_PRIORITY[s] < STATE_PRIORITY[worst]) worst = s;
    }
    return worst;
  };
  const jumpToRole = (role: string) => {
    const hits = lanes.filter((l) => l.role === role);
    if (hits.length === 0) return;
    setOverrides((prev) => {
      const next = new Map(prev);
      for (const l of hits) next.set(l.key, true);
      return next;
    });
    document.getElementById(`agent-anchor-${hits[0].key}`)?.scrollIntoView?.({ block: "start" });
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
  // frame. Unchanged from PRD #38: it narrates the ROLE, never a `toolu_*` lane key,
  // which no screen-reader user could act on.
  const [announcement, setAnnouncement] = useState("");
  const announcedRef = useRef({ agent: undefined as string | undefined, seq: 0, terminal: false });
  useEffect(() => {
    const seen = announcedRef.current;
    let next: string | null = null;

    if (activeAgent && activeAgent !== seen.agent) next = `${activeAgent} is now active`;
    seen.agent = activeAgent;

    let meaningful: RunMessage | undefined;
    for (const mm of messages) {
      if (
        mm.kind === "status" ||
        mm.kind === "error" ||
        mm.kind === "plan" ||
        mm.kind === "plan_revising" ||
        mm.kind === "plan_feedback"
      )
        meaningful = mm;
    }
    if (meaningful && meaningful.seq !== seen.seq) {
      seen.seq = meaningful.seq;
      next =
        meaningful.kind === "error"
          ? `Error: ${describeError(meaningful.payload)}`
          : meaningful.kind === "plan"
            ? "Plan submitted, awaiting approval"
            : meaningful.kind === "plan_revising"
              ? "Revising the plan"
              : meaningful.kind === "plan_feedback"
                ? "Revision feedback sent"
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
          {/* By agent / Timeline (Decision 2). By-agent groups an invocation's whole
              contribution into one lane; Timeline is the raw chronological stream. */}
          <div
            role="group"
            aria-label="Activity view"
            className="inline-flex overflow-hidden rounded-lg border border-edge text-xs"
          >
            {(["agent", "timeline"] as const).map((v) => (
              <button
                key={v}
                type="button"
                onClick={() => selectView(v)}
                aria-pressed={view === v}
                className={cx(
                  "px-2.5 py-1 transition-colors",
                  v === "timeline" && "border-l border-edge",
                  view === v ? "bg-raised font-medium text-fg" : "text-muted hover:bg-raised/60",
                )}
              >
                {v === "agent" ? "By agent" : "Timeline"}
              </button>
            ))}
          </div>
          {actorKeys.length > 1 && (
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

      {actorKeys.length === 0 ? (
        !terminal && (
          <p className="rounded-lg border border-edge bg-ink/40 px-3 py-2 text-xs text-muted">
            Waiting for the first agent…
          </p>
        )
      ) : byAgent ? (
        // Small By-agent crew: NO strip. Every collapsed lane already carries name + dot
        // + state, so a roster beside it would re-render the lane list (Decision 5).
        showRollup && (
          <div aria-label="Crew" className="flex flex-wrap items-center gap-1.5">
            {roles.order.map((r) => {
              const state = worstStateFor(r);
              const n = roles.count.get(r) ?? 0;
              return (
                <button
                  key={r}
                  type="button"
                  onClick={() => jumpToRole(r)}
                  aria-label={`Jump to ${r} activity (${n} ${n === 1 ? "instance" : "instances"}, ${state})`}
                  title={`${r} ×${n}: ${state}`}
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
                  <span className="font-medium text-fg">{r}</span>
                  {n > 1 && <span className="tabular-nums text-faint">×{n}</span>}
                  <span className="text-[11px] uppercase tracking-wide opacity-80">{state}</span>
                </button>
              );
            })}
          </div>
        )
      ) : (
        // Timeline view keeps the PRD #95 jump strip: in a scattered chronological stream
        // a fixed status + jump strip is still the best navigation aid.
        <div aria-label="Crew" className="flex flex-wrap gap-1.5">
          {crew.order.map((a) => {
            const state = crewStateFor(run, a, liveSpeaker, crew.lastMs.get(a) ?? 0, now);
            return (
              <button
                key={a}
                type="button"
                onClick={() => jumpTo(a)}
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
                <span className="text-[11px] uppercase tracking-wide opacity-80">{state}</span>
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

      {actorKeys.length === 0 ? (
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
          {byAgent
            ? lanes.map((l) => (
                <AgentBlock
                  key={l.key}
                  anchorId={`agent-anchor-${l.key}`}
                  bodyId={`agent-body-${l.messages[0]?.seq ?? 0}`}
                  agent={l.role}
                  label={laneLabelText(l.label)}
                  messages={l.messages}
                  startedAt={l.startedAt}
                  state={crewStateFor(run, l.key, liveLane, laneAgg.get(l.key)?.lastMs ?? 0, now)}
                  live={l.key === activeLane}
                  expanded={isExpanded(l.key)}
                  followLive={followLive}
                  // The one-liner and the +N pill live on the LANE, not on an agent's
                  // first block: that first-block-only rule is exactly what made every
                  // repeat bar a near-empty label (Problem 1), and a lane has no repeats.
                  oneLiner={agentOneLiner(laneAgg.get(l.key)?.latest)}
                  unseen={unseenFor(l.key)}
                  onToggle={() => toggleActor(l.key)}
                  now={now}
                  toolIndex={toolIndex}
                  visibleToolUseIds={visibleToolUseIds}
                  phaseUsageBySeq={phaseUsageBySeq}
                />
              ))
            : groups.map((g, i) => {
                const firstSeq = g.messages[0]?.seq ?? i;
                const firstBlock = !anchored.has(g.agent);
                if (firstBlock) anchored.add(g.agent);
                return (
                  <AgentBlock
                    key={`${g.agent}-${firstSeq}`}
                    anchorId={firstBlock ? `agent-anchor-${g.agent}` : undefined}
                    bodyId={`agent-body-${firstSeq}`}
                    agent={g.agent}
                    messages={g.messages}
                    startedAt={g.startedAt}
                    live={g.agent === activeAgent}
                    expanded={isExpanded(g.agent)}
                    followLive={followLive}
                    oneLiner={firstBlock ? agentOneLiner(crew.latest.get(g.agent)) : ""}
                    unseen={firstBlock ? unseenFor(g.agent) : 0}
                    onToggle={() => toggleActor(g.agent)}
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
  agent,
  label,
  messages,
  startedAt,
  state,
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
  agent: string;
  // The lane's task label, ALREADY clamped by laneLabelText: model-authored text
  // rendered plain, never through <Markdown> (Decision 7). Empty ⇒ no `· task` suffix.
  label?: string;
  messages: RunMessage[];
  startedAt: string;
  // Per-lane state dot + word (Decision 3/4), so a COLLAPSED lane still shows whether
  // that actor is live. Absent in Timeline view, where the roster strip carries the dots
  // exactly as it did before this PRD.
  state?: CrewState;
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
  // Accent is keyed on the ROLE, not the lane, so two parallel coders read as one role
  // in two lanes rather than two unrelated colours.
  const accent = agentAccent(agent);
  // The dot's tooltip reinforces the colour (Decision 4) — there is deliberately NO
  // standing legend, because the state word beside every dot already IS the label.
  const name = label ? `${agent} · ${label}` : agent;
  // Tail this expanded body to its bottom on append, but ONLY while Follow live is on
  // (Decision 3). With Follow off, an append never moves the viewport.
  const bodyRef = useTailOnAppend(messages.length, followLive && expanded);

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
  for (const m of messages) {
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
          aria-label={`${expanded ? "Collapse" : "Expand"} ${name} activity`}
          className="-ml-1 inline-flex h-6 w-6 shrink-0 items-center justify-center rounded text-muted transition-colors hover:bg-raised/60 hover:text-fg"
        >
          <span
            aria-hidden="true"
            className={cx("inline-flex transition-transform", expanded && "rotate-90")}
          >
            <ChevronRightIcon />
          </span>
        </button>
        {state && (
          <span
            title={`${name}: ${state}`}
            className={cx("inline-flex shrink-0 items-center", CREW_TONE[state])}
          >
            <span
              aria-hidden="true"
              className={cx(
                "h-1.5 w-1.5 rounded-full bg-current",
                state === "working" && "animate-pulse",
              )}
            />
          </span>
        )}
        <span className={cx("shrink-0 text-sm font-semibold", accent.split(" ")[0])}>{agent}</span>
        {label && (
          <span className="shrink-0 font-mono text-[11px] text-faint">
            <span aria-hidden="true">· </span>
            {label}
          </span>
        )}
        {/* The live one-liner updates IN PLACE (no scroll) — the thing you read instead
            of expanding the log. On a lane it shows the lane's newest message. */}
        {oneLiner && <span className="min-w-0 truncate text-xs text-muted">{oneLiner}</span>}
        {state && (
          <span className={cx("shrink-0 text-[11px] uppercase tracking-wide", CREW_TONE[state])}>
            {state}
          </span>
        )}
        {!expanded && unseen > 0 && (
          <span
            title={`${unseen} new since you last looked`}
            className="shrink-0 rounded-full bg-brand/15 px-1.5 py-0.5 text-[10px] font-medium text-brand"
          >
            +{unseen}
          </span>
        )}
        <span className="ml-auto shrink-0 text-[11px] tabular-nums text-faint" title={startedAt}>
          {relativeTime(startedAt, now)}
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
