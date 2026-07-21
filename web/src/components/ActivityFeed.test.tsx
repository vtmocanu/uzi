// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render } from "@testing-library/react";
import type { Run, RunHealth, RunMessage, RunStatus } from "../lib/api";
import { ActivityFeed } from "./ActivityFeed";
import { mockBusyMessages } from "../mocks/data";

// This jsdom build does not expose window.localStorage (see prefs.test.ts), so the view
// preference has to be backed by a Map-based Storage stub — without one `prefs` silently
// falls back and selectTimelineView() would be a no-op that still passes.
function makeStorage(): Storage {
  const m = new Map<string, string>();
  return {
    getItem: (k: string) => (m.has(k) ? (m.get(k) as string) : null),
    setItem: (k: string, v: string) => void m.set(k, String(v)),
    removeItem: (k: string) => void m.delete(k),
    clear: () => m.clear(),
    key: (i: number) => [...m.keys()][i] ?? null,
    get length() {
      return m.size;
    },
  } as Storage;
}

beforeEach(() => {
  // Fresh per test: the view toggle persists GLOBALLY (PRD #99 Decision 2), so a test
  // that selects Timeline would otherwise leak that choice into every later test.
  Object.defineProperty(window, "localStorage", { configurable: true, value: makeStorage() });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

// The pane defaults to By-agent instance lanes (PRD #99). Two behaviours below are the
// pre-#99 chronological rendering, which is RETAINED but now lives only behind the
// Timeline toggle — seed the persisted preference before rendering to reach it.
function selectTimelineView() {
  window.localStorage.setItem("uzi.activity.view", JSON.stringify("timeline"));
}

function m(
  seq: number,
  kind: string,
  payload: unknown,
  agent = "lead",
  created_at = "2026-07-04T00:00:00.000Z",
): RunMessage {
  return { seq, kind, agent, agent_instance: null, agent_label: null, payload, created_at };
}

// A minimal Run fixture — the crew ladder reads only status + health; the rest is
// filler the DTO requires.
function runFixture(over: Partial<Run> = {}): Run {
  return {
    id: "r1",
    repo_id: "repo1",
    forge_type: "gitlab",
    kind: "issue",
    issue_iid: 7,
    issue_title: "t",
    issue_description: "d",
    title: null,
    resume_of_run_id: null,
    status: "running",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: "w1",
    branch: null,
    mr_iid: null,
    mr_web_url: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    claimed_at: null,
    started_at: null,
    finished_at: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

const TERMINAL: RunStatus[] = ["completed", "failed", "cancelled"];

// renderFeed centralizes the now-required `run` prop and keeps runningLive/terminal
// consistent with status, so a test states only status + health.
function renderFeed(
  messages: RunMessage[],
  opts: { status?: RunStatus; health?: RunHealth; connected?: boolean } = {},
) {
  const { status = "running", health = "ok", connected = true } = opts;
  return render(
    <ActivityFeed
      messages={messages}
      run={runFixture({ status, health })}
      runningLive={status === "running"}
      connected={connected}
      terminal={TERMINAL.includes(status)}
    />,
  );
}

function stubMetrics(el: HTMLElement, scrollHeight: number, clientHeight: number) {
  Object.defineProperty(el, "scrollHeight", { configurable: true, get: () => scrollHeight });
  Object.defineProperty(el, "clientHeight", { configurable: true, get: () => clientHeight });
}

// A lead→worker→lead feed: two non-contiguous lead blocks around a worker block.
function leadWorkerLead(): RunMessage[] {
  return [
    m(1, "text", { text: "planning the change" }, "lead"),
    m(2, "tool_use", { id: "u-read", name: "Read", input: { file_path: "/x" } }, "lead"),
    m(3, "tool_result", { tool_use_id: "u-read", content: "line1\nline2" }, "lead"),
    m(4, "text", { text: "implementing now" }, "worker"),
    m(5, "tool_use", { id: "u-bash", name: "Bash", input: { command: "go build" } }, "worker"),
    m(6, "tool_result", { tool_use_id: "u-bash", content: "ok" }, "worker"),
    m(7, "text", { text: "reviewing the diff" }, "lead"),
    m(8, "tool_use", { id: "u-grep", name: "Grep", input: { pattern: "foo" } }, "lead"),
    m(9, "tool_result", { tool_use_id: "u-grep", content: "hit" }, "lead"),
  ];
}

// ── Crew roster ladder (PRD #95 Decision 2) ───────────────────────────────────
describe("ActivityFeed crew roster", () => {
  it("active speaker reads `working` (pulsing green) on a healthy live run", () => {
    const { getByTitle, container } = renderFeed([m(1, "text", { text: "hi" }, "lead")], {
      status: "running",
      health: "ok",
    });
    expect(getByTitle("lead: working")).toBeTruthy();
    // The working dot pulses (honored by prefers-reduced-motion via index.css).
    expect(container.querySelector(".animate-pulse")).not.toBeNull();
  });

  it("active speaker stays `working` through a MULTI-MINUTE tool call (B2 — recency does not gate it)", () => {
    // The newest message is a tool_use from 5 minutes ago; recency alone would read
    // idle, but a healthy active speaker must still pulse (the server stall flag is
    // 300s, so a long go-test/npm-ci is the common busy case).
    const long = new Date(Date.now() - 5 * 60_000).toISOString();
    const { getByTitle, queryByTitle } = renderFeed(
      [m(1, "tool_use", { id: "u1", name: "Bash", input: { command: "go test ./..." } }, "lead", long)],
      { status: "running", health: "ok" },
    );
    expect(getByTitle("lead: working")).toBeTruthy();
    expect(queryByTitle("lead: idle")).toBeNull();
  });

  it("a `looping` active speaker reads amber `stalled`, never green `working`", () => {
    const { getByTitle, queryByTitle, container } = renderFeed([m(1, "text", { text: "hi" }, "lead")], {
      status: "running",
      health: "looping",
    });
    expect(getByTitle("lead: stalled")).toBeTruthy();
    expect(queryByTitle("lead: working")).toBeNull();
    // stalled is not working → no pulse.
    expect(container.querySelector(".animate-pulse")).toBeNull();
  });

  it("`slow` and `stalled` health also read stalled", () => {
    for (const health of ["slow", "stalled"] as RunHealth[]) {
      const { getByTitle, unmount } = renderFeed([m(1, "text", { text: "hi" }, "lead")], {
        status: "running",
        health,
      });
      expect(getByTitle("lead: stalled")).toBeTruthy();
      unmount();
    }
  });

  it("every agent reads `waiting` at a plan gate", () => {
    const { getByTitle } = renderFeed(leadWorkerLead(), { status: "awaiting_approval", health: "ok" });
    expect(getByTitle("lead: waiting")).toBeTruthy();
    expect(getByTitle("worker: waiting")).toBeTruthy();
  });

  it("every agent reads `waiting` when the worker is gone (waiting_worker)", () => {
    const { getByTitle } = renderFeed(leadWorkerLead(), { status: "running", health: "waiting_worker" });
    expect(getByTitle("lead: waiting")).toBeTruthy();
    expect(getByTitle("worker: waiting")).toBeTruthy();
  });

  it("a non-active agent splits waiting↔idle by recency", () => {
    const now = Date.now();
    const old = new Date(now - 60_000).toISOString(); // ≥45s → idle
    const recent = new Date(now - 5_000).toISOString(); // <45s → waiting
    const fresh = new Date(now - 500).toISOString();
    const msgs = [
      m(1, "text", { text: "old lead" }, "lead", old),
      m(2, "text", { text: "recent reviewer" }, "reviewer", recent),
      m(3, "text", { text: "active worker" }, "worker", fresh),
    ];
    const { getByTitle } = renderFeed(msgs, { status: "running", health: "ok" });
    expect(getByTitle("worker: working")).toBeTruthy(); // active
    expect(getByTitle("reviewer: waiting")).toBeTruthy(); // recent, non-active
    expect(getByTitle("lead: idle")).toBeTruthy(); // quiet, non-active
  });

  it("the header one-liner never reuses the crew-state lexicon (a tool_result reads 'Ran a tool')", () => {
    // web-ux regression: agentOneLiner returned the literal "Working" for a
    // tool_result, so a blocked/non-active agent's header read "Working" while its
    // crew chip read "idle" — a momentary contradiction. The one-liner must use a
    // non-state phrase.
    const now = Date.now();
    const old = new Date(now - 60_000).toISOString();
    const fresh = new Date(now - 500).toISOString();
    const msgs = [
      m(1, "text", { text: "planning" }, "lead", old),
      m(2, "tool_use", { id: "u1", name: "Read", input: { file_path: "/x" } }, "lead", old),
      m(3, "tool_result", { tool_use_id: "u1", content: "ok" }, "lead", old), // lead's newest → idle
      m(4, "text", { text: "implementing" }, "worker", fresh), // active
    ];
    const { getByText, getByTitle } = renderFeed(msgs, { status: "running", health: "ok" });
    // lead is non-active + quiet → its chip reads `idle`…
    expect(getByTitle("lead: idle")).toBeTruthy();
    // …so its header one-liner must NOT echo a state word — a tool_result reads "Ran a tool".
    expect(getByText("Ran a tool")).toBeTruthy();
  });

  it("a terminal run reads every agent `done`", () => {
    const { getByTitle, container } = renderFeed(leadWorkerLead(), { status: "completed" });
    expect(getByTitle("lead: done")).toBeTruthy();
    expect(getByTitle("worker: done")).toBeTruthy();
    expect(container.querySelector(".animate-pulse")).toBeNull();
  });

  it("renders a single muted placeholder (not zero chips) before the first agent speaks", () => {
    const { getByText, queryByRole } = renderFeed([], { status: "queued" });
    expect(getByText("Waiting for the first agent…")).toBeTruthy();
    // No crew chips at all (an empty roster is a placeholder, not zero buttons).
    expect(queryByRole("button", { name: /^Jump to/ })).toBeNull();
  });

  it("clicking a crew chip expands that agent (Timeline — the strip's home)", () => {
    // The jump strip is Timeline-only now: under By-agent the collapsed lanes ARE the
    // roster, so a small crew renders no strip at all (PRD #99 Decision 5).
    selectTimelineView();
    // Newest message is lead (seq 9) → lead is the active speaker; worker is non-active.
    const { getByRole } = renderFeed(leadWorkerLead(), { status: "running", health: "ok" });
    // Multi-agent live run → collapsed by default.
    expect(getByRole("button", { name: /worker activity$/ }).getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(getByRole("button", { name: /^Jump to worker/ }));
    expect(getByRole("button", { name: /worker activity$/ }).getAttribute("aria-expanded")).toBe("true");
  });
});

// ── Collapse-by-default + auto-expand (S5) ────────────────────────────────────
describe("ActivityFeed collapse-by-default", () => {
  it("collapses every agent by default on a multi-agent live run", () => {
    const { getAllByRole } = renderFeed(leadWorkerLead(), { status: "running", health: "ok" });
    for (const t of getAllByRole("button", { name: /activity$/ }))
      expect(t.getAttribute("aria-expanded")).toBe("false");
  });

  it("auto-expands a terminal run (reading a done run is not death-by-clicks)", () => {
    const { getAllByRole } = renderFeed(leadWorkerLead(), { status: "completed" });
    for (const t of getAllByRole("button", { name: /activity$/ }))
      expect(t.getAttribute("aria-expanded")).toBe("true");
  });

  it("auto-expands a single-agent run", () => {
    const { getByRole } = renderFeed([m(1, "text", { text: "solo" }, "lead")], { status: "running" });
    expect(getByRole("button", { name: /lead activity$/ }).getAttribute("aria-expanded")).toBe("true");
  });

  it("collapsing lead reduces BOTH lead blocks, worker untouched (Timeline, keyed by agent name)", () => {
    // Two non-contiguous lead blocks only exist in Timeline: By-agent coalesces them
    // into one lane, which is the whole point of PRD #99's Problem 1.
    selectTimelineView();
    const { getAllByRole, getByRole } = renderFeed(leadWorkerLead(), { status: "completed" });
    // Terminal → all expanded; collapse lead via its first block's chevron.
    const leadToggles = getAllByRole("button", { name: /lead activity$/ });
    expect(leadToggles).toHaveLength(2);
    fireEvent.click(leadToggles[0]);
    for (const t of getAllByRole("button", { name: /lead activity$/ }))
      expect(t.getAttribute("aria-expanded")).toBe("false");
    expect(getByRole("button", { name: /worker activity$/ }).getAttribute("aria-expanded")).toBe("true");
    // Both lead bodies hidden; the worker body is not.
    expect(document.getElementById("agent-body-1")?.hidden).toBe(true);
    expect(document.getElementById("agent-body-7")?.hidden).toBe(true);
    expect(document.getElementById("agent-body-4")?.hidden).toBe(false);
  });

  it("Expand all / Collapse all flips every agent", () => {
    const { getByText, getAllByRole } = renderFeed(leadWorkerLead(), { status: "running", health: "ok" });
    // Collapsed by default → the control offers "Expand all".
    fireEvent.click(getByText("Expand all"));
    for (const t of getAllByRole("button", { name: /activity$/ }))
      expect(t.getAttribute("aria-expanded")).toBe("true");
    // Now it offers "Collapse all".
    fireEvent.click(getByText("Collapse all"));
    for (const t of getAllByRole("button", { name: /activity$/ }))
      expect(t.getAttribute("aria-expanded")).toBe("false");
  });

  it("a new message from a collapsed agent keeps it collapsed and counts in the +N pill", () => {
    const { container, getAllByRole, rerender } = renderFeed(leadWorkerLead(), {
      status: "running",
      health: "ok",
    });
    // lead is collapsed by default (multi-agent live). No pill yet.
    expect(getAllByRole("button", { name: /lead activity$/ })[0].getAttribute("aria-expanded")).toBe("false");
    expect(container.textContent).not.toContain("+1");

    // A new lead message arrives while collapsed.
    rerender(
      <ActivityFeed
        messages={[...leadWorkerLead(), m(10, "text", { text: "still going" }, "lead")]}
        run={runFixture({ status: "running", health: "ok" })}
        runningLive={true}
        connected={true}
        terminal={false}
      />,
    );
    // Still collapsed (no auto-expand)…
    expect(getAllByRole("button", { name: /lead activity$/ })[0].getAttribute("aria-expanded")).toBe("false");
    // …and the +N pill counted it.
    expect(container.textContent).toContain("+1");
  });
});

// ── Opt-in Follow (Decision 3) ────────────────────────────────────────────────
describe("ActivityFeed opt-in Follow live", () => {
  it("with Follow OFF (default), an append does NOT auto-scroll the body", () => {
    const { container, rerender } = renderFeed([m(1, "text", { text: "one" }, "lead")], {
      status: "running",
    });
    const body = document.getElementById("agent-body-1") as HTMLElement;
    stubMetrics(body, 1000, 200);
    body.scrollTop = 0;
    rerender(
      <ActivityFeed
        messages={[m(1, "text", { text: "one" }, "lead"), m(2, "text", { text: "two" }, "lead")]}
        run={runFixture({ status: "running", health: "ok" })}
        runningLive={true}
        connected={true}
        terminal={false}
      />,
    );
    // No follow → the viewport never moved.
    expect(body.scrollTop).toBe(0);
    expect(container).toBeTruthy();
  });

  it("with Follow ON, an expanded body tails to the bottom on append", () => {
    const { getByLabelText, rerender } = renderFeed([m(1, "text", { text: "one" }, "lead")], {
      status: "running",
    });
    const body = document.getElementById("agent-body-1") as HTMLElement;
    stubMetrics(body, 1000, 200);
    body.scrollTop = 0;
    // Turn Follow live on.
    act(() => {
      fireEvent.click(getByLabelText("Follow live"));
    });
    rerender(
      <ActivityFeed
        messages={[m(1, "text", { text: "one" }, "lead"), m(2, "text", { text: "two" }, "lead")]}
        run={runFixture({ status: "running", health: "ok" })}
        runningLive={true}
        connected={true}
        terminal={false}
      />,
    );
    expect(body.scrollTop).toBe(1000);
  });
});

// ── Preserved behavior (tool pairing, cap, reconnect, rail, a11y) ─────────────
describe("ActivityFeed tool pairing (preserved)", () => {
  it("folds a result under its call by id and renders an unmatched result standalone", () => {
    const messages: RunMessage[] = [
      m(1, "tool_use", { id: "use-paired", name: "Read", input: { file_path: "/x" } }),
      m(2, "tool_result", { tool_use_id: "use-paired", content: "paired output" }),
      m(3, "tool_result", { tool_use_id: "use-orphan", content: "orphan output" }),
    ];
    const { container } = renderFeed(messages, { status: "completed" });
    const text = container.textContent ?? "";
    expect(text).toContain("Read");
    expect(text).toContain("paired output");
    expect(text).toContain("orphan output");
    expect(text).toContain("use-orphan");
    expect(text).not.toContain("use-paired");
  });

  it("renders a result standalone when its call was capped out of the visible window", () => {
    const messages: RunMessage[] = [
      m(1, "tool_use", { id: "straddle-call", name: "Read", input: { file_path: "/x" } }),
    ];
    for (let seq = 2; seq <= 1001; seq++) messages.push(m(seq, "text", { text: `filler ${seq}` }));
    messages.push(m(1002, "tool_result", { tool_use_id: "straddle-call", content: "straddle result" }));

    const { container } = renderFeed(messages, { status: "completed" });
    const text = container.textContent ?? "";
    expect(text).toContain("earlier messages");
    expect(text).toContain("straddle result");
    expect(text).toContain("straddle-call");
  });

  it("renders the reconnecting banner only when disconnected, after ~3s", () => {
    const online = renderFeed([m(1, "text", { text: "hi" })], { status: "running", connected: true });
    expect(online.container.textContent).not.toContain("Reconnecting");
    online.unmount();

    vi.useFakeTimers();
    const { container } = renderFeed([m(1, "text", { text: "hi" })], {
      status: "running",
      connected: false,
    });
    expect(container.textContent).not.toContain("Reconnecting");
    act(() => {
      vi.advanceTimersByTime(3000);
    });
    expect(container.textContent).toContain("Reconnecting");
  });

  it("gathers consecutive tool rows into ONE rail, split by interleaved prose", () => {
    const twoTools = [
      m(1, "tool_use", { id: "a", name: "Read", input: { file_path: "/x" } }, "lead"),
      m(2, "tool_use", { id: "b", name: "Grep", input: { pattern: "y" } }, "lead"),
    ];
    const { container, unmount } = renderFeed(twoTools, { status: "completed" });
    expect(container.querySelectorAll('[class*="tool-rail"]')).toHaveLength(1);
    unmount();

    const split = [
      m(1, "tool_use", { id: "a", name: "Read", input: { file_path: "/x" } }, "lead"),
      m(2, "text", { text: "note between" }, "lead"),
      m(3, "tool_use", { id: "b", name: "Grep", input: { pattern: "y" } }, "lead"),
    ];
    const { container: c2 } = renderFeed(split, { status: "completed" });
    expect(c2.querySelectorAll('[class*="tool-rail"]')).toHaveLength(2);
  });
});

describe("ActivityFeed header + timestamps (preserved)", () => {
  it("renders a relative timestamp with the absolute ISO in the title", () => {
    const iso = new Date(Date.now() - 6 * 60 * 1000).toISOString();
    const { getByTitle } = renderFeed([m(1, "text", { text: "hi" }, "lead", iso)], { status: "running" });
    expect(getByTitle(iso).textContent).toBe("6m ago");
  });

  it("renders 'just now' for a very recent timestamp", () => {
    const iso = new Date().toISOString();
    const { getByTitle } = renderFeed([m(1, "text", { text: "hi" }, "lead", iso)], { status: "running" });
    expect(getByTitle(iso).textContent).toBe("just now");
  });

  it("renders status messages as hairline meta divider lines", () => {
    const { container } = renderFeed(
      [
        m(1, "status", { event: "init", model: "claude-opus-4-8" }, "lead"),
        m(2, "text", { text: "starting" }, "lead"),
      ],
      { status: "running" },
    );
    expect(container.textContent).toContain("agent started (claude-opus-4-8)");
    expect(container.querySelector(".h-px")).not.toBeNull();
  });
});

describe("ActivityFeed accessibility (preserved)", () => {
  it("routes only meaningful transitions to the live region, never tool frames", () => {
    const region = () => document.querySelector('[aria-live="polite"]') as HTMLElement;
    const base = [
      m(1, "status", { event: "init", model: "claude-opus-4-8" }, "lead"),
      m(2, "text", { text: "planning" }, "lead"),
      m(3, "tool_use", { id: "u1", name: "Read", input: { file_path: "/x" } }, "lead"),
    ];
    const { rerender } = renderFeed(base, { status: "running" });
    const afterMount = region().textContent;
    expect(afterMount).toContain("agent started (claude-opus-4-8)");

    const withTools = [
      ...base,
      m(4, "tool_result", { tool_use_id: "u1", content: "ok" }, "lead"),
      m(5, "tool_use", { id: "u2", name: "Bash", input: { command: "ls" } }, "lead"),
    ];
    rerender(
      <ActivityFeed
        messages={withTools}
        run={runFixture({ status: "running" })}
        runningLive={true}
        connected={true}
        terminal={false}
      />,
    );
    expect(region().textContent).toBe(afterMount);

    rerender(
      <ActivityFeed
        messages={[...withTools, m(6, "error", { text: "push failed" }, "lead")]}
        run={runFixture({ status: "running" })}
        runningLive={true}
        connected={true}
        terminal={false}
      />,
    );
    expect(region().textContent).toContain("push failed");
  });

  it("bounds a huge untrusted status announcement", () => {
    const region = () => document.querySelector('[aria-live="polite"]') as HTMLElement;
    const huge = "x".repeat(5000);
    renderFeed([m(1, "status", { text: huge }, "lead")], { status: "running" });
    const text = region().textContent ?? "";
    expect(text.length).toBeLessThanOrEqual(200);
    expect(text).toContain("xxx");
  });

  it("uses muted (not faint) for the empty-state text and the message counter", () => {
    const empty = renderFeed([], { status: "completed" });
    const emptyP = empty.getByText("No messages were recorded for this run.");
    expect(emptyP.className).toContain("text-muted");
    expect(emptyP.className).not.toContain("text-faint");
    empty.unmount();

    const withMsgs = renderFeed([m(1, "text", { text: "hi" })], { status: "completed" });
    const counter = withMsgs.getByText("1 messages");
    expect(counter.className).toContain("text-muted");
    expect(counter.className).not.toContain("text-faint");
  });

  it("mutes the scroll container's implicit live region to aria-live=off", () => {
    const { container } = renderFeed([m(1, "text", { text: "hi" })], { status: "running" });
    expect(container.querySelector('[role="log"]')?.getAttribute("aria-live")).toBe("off");
  });
});

// ── PRD #99 — instance lanes (M5) ─────────────────────────────────────────────
// Every test above builds messages with `m()`, which hardcodes agent_instance and
// agent_label to null — so the whole instance-keyed path was invisible to the suite:
// MEASURED at M5, making laneKeyOf ignore agent_instance (Problem 2 verbatim — two
// parallel coders merging into one lane) left all 816 tests green, and swapping the
// plain `{label}` render for <Markdown> left all 30 ActivityFeed tests green. Nothing
// below may use `m()`; a By-agent assertion that does not supply a non-null instance
// id is vacuous by construction.
function mi(
  seq: number,
  agent: string,
  agent_instance: string | null,
  agent_label: string | null,
  over: { kind?: string; payload?: unknown; created_at?: string } = {},
): RunMessage {
  const {
    kind = "text",
    payload = { text: `msg ${seq}` },
    created_at = "2026-07-04T00:00:00.000Z",
  } = over;
  return { seq, kind, agent, agent_instance, agent_label, payload, created_at };
}

// Two parallel coder invocations, NON-ADJACENT (a reviewer frame between them). The
// non-adjacency is load-bearing: two contiguous coder frames render as one block even
// under the pre-#99 consecutive-author grouping, so they cannot catch the merge bug.
function twoParallelCoders(): RunMessage[] {
  return [
    mi(1, "lead", null, null),
    mi(2, "coder", "toolu_A", "API wiring"),
    mi(3, "reviewer", "toolu_R", "audit unit A"),
    mi(4, "coder", "toolu_B", "web gate UX"),
    mi(5, "coder", "toolu_A", "API wiring"),
  ];
}

// Lane titles carry the Expand/Collapse verb, which flips with the auto-expand rule
// (terminal or single-actor). Match on the tail so a test states the LANE, not its
// expansion state.
const laneTitles = (r: { getAllByRole: (...a: never[]) => HTMLElement[] }): string[] =>
  (r.getAllByRole as unknown as (role: string, o: object) => HTMLElement[])("button", {
    name: /activity$/,
  }).map((b) => (b.getAttribute("aria-label") ?? "").replace(/^(Expand|Collapse) | activity$/g, ""));

describe("ActivityFeed instance lanes (PRD #99)", () => {
  it("splits two parallel same-role invocations into two lanes titled by their task", () => {
    const r = renderFeed(twoParallelCoders(), { status: "running", health: "ok" });
    // The whole point of the PRD: `coder` appears twice, distinctly labelled, never
    // merged into one block.
    expect(r.getByRole("button", { name: /coder · API wiring activity$/ })).toBeTruthy();
    expect(r.getByRole("button", { name: /coder · web gate UX activity$/ })).toBeTruthy();
    // Four actors → four lanes: lead, coder/A, reviewer, coder/B. A merge collapses
    // this to three, which is the mutation this test exists to fail.
    expect(laneTitles(r).sort()).toEqual([
      "coder · API wiring",
      "coder · web gate UX",
      "lead",
      "reviewer · audit unit A",
    ]);
  });

  it("coalesces an instance's NON-ADJACENT later turns back into its own lane", () => {
    // seq 2 and seq 5 are the same invocation with a reviewer frame between them, so
    // the lane holds both — the fix for Problem 1's repeated near-empty bars.
    const r = renderFeed(twoParallelCoders(), { status: "completed" });
    const lane = r.getByRole("button", { name: /coder · API wiring activity$/ }).closest("div")
      ?.parentElement;
    expect(lane?.textContent).toContain("msg 2");
    expect(lane?.textContent).toContain("msg 5");
    // …and the OTHER coder's message is not in it.
    expect(lane?.textContent).not.toContain("msg 4");
  });

  it("keys the lane with || not ?? — an empty-string instance falls back to the ROLE", () => {
    // "" cannot reach the browser (the API stores it as SQL NULL) but the type admits
    // it, and `??` would keep it as a key, splitting one role into two lanes for a
    // value that means absence. This is the only fixture in the suite that can tell
    // `||` from `??`.
    const r = renderFeed(
      [mi(1, "coder", "", null), mi(2, "coder", null, null)],
      { status: "running", health: "ok" },
    );
    expect(laneTitles(r)).toEqual(["coder"]);
  });

  it("titles a lane from its first NON-LEAD role, so a replay-shaped opener does not win", () => {
    // An SDK replay frame carries parent_tool_use_id but no subagent_type, and the
    // worker's `subagent_type ?? "lead"` stores the literal "lead" on it. A lane can
    // therefore OPEN lead-looking and hold a coder's work (Decision 8).
    const r = renderFeed(
      [mi(1, "lead", "toolu_C", null), mi(2, "coder", "toolu_C", "web gate UX")],
      { status: "running", health: "ok" },
    );
    expect(laneTitles(r)).toEqual(["coder · web gate UX"]);
    // Testing `agent != null` instead would NOT discriminate: the absent field is
    // already collapsed to "lead" upstream and never arrives as null.
    expect(r.queryByRole("button", { name: /^(Expand|Collapse) lead/ })).toBeNull();
  });

  it("keeps an all-`lead` lane titled lead — a repo may ship .claude/agents/lead.md", () => {
    // agent_instance != null does NOT imply agent != "lead": a repo-authored `lead`
    // registers as an ordinary invocable subagent (no lead filter in
    // subagentsFromTemplates), so two parallel invocations of it are two real lanes.
    // A "first non-lead role, else bust" rule breaks exactly here, and so does the
    // write-side guard that Decision 8 declined twice.
    const r = renderFeed(
      [
        mi(1, "lead", "toolu_L1", "shard 1"),
        mi(2, "lead", "toolu_L2", "shard 2"),
        mi(3, "lead", "toolu_L1", "shard 1"),
      ],
      { status: "running", health: "ok" },
    );
    expect(laneTitles(r).sort()).toEqual(["lead · shard 1", "lead · shard 2"]);
  });

  it("takes the label from the first frame carrying one, independently of the role", () => {
    // laneLabel is a SEPARATE scan from laneRole (Decision 1/3): agent_label can be
    // absent for reasons that have nothing to do with the role, so a labelless opening
    // frame must not cost the lane its title.
    const r = renderFeed(
      [mi(1, "coder", "toolu_A", null), mi(2, "coder", "toolu_A", "web gate UX")],
      { status: "running", health: "ok" },
    );
    expect(laneTitles(r)).toEqual(["coder · web gate UX"]);
  });

  it("flattens a multi-line label instead of silently dropping everything after line 1", () => {
    // The old rule was `label.split("\n")[0]`, which matches toolSummary's firstLine
    // idiom but drops the remainder with NO ellipsis when line 1 is short — nothing
    // on screen said text was missing. Truncation must be the only thing that removes
    // text, because truncation is the only thing that leaves a "…" saying so.
    const r = renderFeed(
      [mi(1, "coder", "toolu_A", "short first line\nSECOND LINE")],
      { status: "running", health: "ok" },
    );
    expect(laneTitles(r)).toEqual(["coder · short first line SECOND LINE"]);
  });

  it("collapses whitespace runs but keeps the ellipsis when a flattened label overflows", () => {
    const long = "first line of the label\nsecond line that pushes it past the clamp";
    const r = renderFeed([mi(1, "coder", "toolu_A", long)], { status: "running", health: "ok" });
    const title = laneTitles(r)[0];
    // Single line, clamped, and the clamp is visible.
    expect(title).not.toContain("\n");
    expect(title.endsWith("…")).toBe(true);
    // The dropped tail is genuinely gone, not merely off-screen.
    expect(title).not.toContain("past the clamp");
  });

  it("renders a labelless lane as the bare role — no `·` suffix, no placeholder", () => {
    const r = renderFeed([mi(1, "reviewer", "toolu_R", null)], { status: "running", health: "ok" });
    expect(laneTitles(r)).toEqual(["reviewer"]);
  });

  it("coalesces a legacy NULL-instance run into one lane per ROLE ([C3])", () => {
    // leadWorkerLead() is nine messages of lead/worker/lead built with `m()` — every
    // instance NULL, i.e. a pre-migration run. By-agent must show two lanes, not the
    // four blocks the pre-#99 consecutive-author grouping produced.
    const r = renderFeed(leadWorkerLead(), { status: "running", health: "ok" });
    expect(laneTitles(r).sort()).toEqual(["lead", "worker"]);
  });
});

describe("ActivityFeed lane dots + role rollup (PRD #99)", () => {
  it("pulses exactly ONE lane — the active INSTANCE, not the active role", () => {
    // Scoped to role="log" (ActivityFeed.tsx's only one) so the rollup strip's own
    // chips cannot be counted as lane dots. Keyed by role instead of lane, both coder
    // lanes would pulse.
    const { container } = renderFeed(twoParallelCoders(), { status: "running", health: "ok" });
    const log = container.querySelector('[role="log"]');
    expect(log).not.toBeNull();
    expect(log?.querySelectorAll(".animate-pulse").length).toBe(1);
    // …and it is the newest message's lane (seq 5 → toolu_A), not the other coder's.
    expect(container.querySelector('[title="coder · API wiring: working"]')).not.toBeNull();
    expect(container.querySelector('[title="coder · web gate UX: working"]')).toBeNull();
  });

  it("shows NO strip for a small By-agent crew — the collapsed lanes ARE the roster", () => {
    const { container } = renderFeed(
      [mi(1, "lead", null, null), mi(2, "coder", "toolu_A", "x"), mi(3, "reviewer", "toolu_R", "y")],
      { status: "running", health: "ok" },
    );
    expect(container.querySelector('[aria-label="Crew"]')).toBeNull();
  });

  it("shows the rollup once a role is DOUBLED, counting instances regardless of state", () => {
    // Corrected at M4: the trigger is the instance count, not liveness — a completed
    // run with two coder lanes still rolls up.
    const { container, getByRole } = renderFeed(twoParallelCoders(), { status: "completed" });
    expect(container.querySelector('[aria-label="Crew"]')).not.toBeNull();
    expect(getByRole("button", { name: "Jump to coder activity (2 instances, done)" })).toBeTruthy();
  });

  it("shows the rollup once lanes OVERFLOW the glance threshold, with no role doubled", () => {
    // Seven single-instance roles: no role is doubled, so only the >6 arm can fire.
    const roles = ["lead", "coder", "reviewer", "tester", "auditor", "architect", "documenter"];
    const { container } = renderFeed(
      roles.map((role, i) => mi(i + 1, role, `toolu_${i}`, null)),
      { status: "running", health: "ok" },
    );
    expect(container.querySelector('[aria-label="Crew"]')).not.toBeNull();
  });

  it("gives a doubled role the WORST state of its instances, not the first or the last", () => {
    // Lane order is A, B, C and the worst state sits in the MIDDLE, so "take the first"
    // and "take the last" both produce `idle` while worst-wins produces `stalled`.
    // Attention priority: stalled > waiting > working > idle > done.
    const now = Date.now();
    const old = new Date(now - 120_000).toISOString();
    const fresh = new Date(now - 500).toISOString();
    const { getByTitle, queryByTitle } = renderFeed(
      [
        mi(1, "coder", "toolu_A", "A", { created_at: old }),
        mi(2, "coder", "toolu_B", "B", { created_at: old }),
        mi(3, "coder", "toolu_C", "C", { created_at: old }),
        mi(4, "coder", "toolu_B", "B", { created_at: fresh }), // newest → B is the live lane
      ],
      { status: "running", health: "looping" },
    );
    expect(getByTitle("coder ×3: stalled")).toBeTruthy();
    expect(queryByTitle("coder ×3: idle")).toBeNull();
    expect(queryByTitle("coder ×3: done")).toBeNull();
  });

  it("rolls a doubled role up as `waiting` while one of its own lanes pulses `working`", () => {
    // The pairing nobody had on screen before the M6 fixtures. worstStateFor takes the
    // MIN of STATE_PRIORITY and `waiting`(1) outranks `working`(2), so a role with one
    // ACTIVE lane and one recently-spoken sibling summarises as `waiting` and its chip
    // does NOT pulse — while the active lane below it does. That is deliberate (the
    // chip is a worst-state summary, not a most-active one), but nothing on screen
    // says so and the no-legend decision removed the obvious place to say it.
    //
    // Pinned here so the behaviour is explicit rather than incidental: if the product
    // answer changes, this test is the thing that has to change with it.
    const now = Date.now();
    const recent = new Date(now - 24_000).toISOString(); // <45s -> waiting
    const fresh = new Date(now - 1_000).toISOString();
    const { container, getByTitle } = renderFeed(
      [
        mi(1, "tester", "toolu_Y", "unit", { created_at: recent }),
        mi(2, "tester", "toolu_X", "e2e", { created_at: fresh }), // newest -> active
      ],
      { status: "running", health: "ok" },
    );
    expect(getByTitle("tester ×2: waiting")).toBeTruthy();
    // The lane, meanwhile, reads working and pulses — exactly one, inside role="log".
    expect(getByTitle("tester · e2e: working")).toBeTruthy();
    expect(container.querySelector('[role="log"]')?.querySelectorAll(".animate-pulse").length).toBe(1);
    // And the rollup chip itself does not pulse, so "one pulse" is never a whole-pane
    // claim — it is scoped to the lane list.
    const crew = container.querySelector('[aria-label="Crew"]');
    expect(crew?.querySelectorAll(".animate-pulse").length).toBe(0);
  });

  it("orders rollup chips ATTENTION-FIRST, not first-seen", () => {
    // The mockup's caption is explicit: "the one stalled tester is the first thing you
    // see." Only the POSITION diverged — the dot colour was already worst-state-wins.
    // Priority: stalled > waiting > working > idle > done.
    const now = Date.now();
    const old = new Date(now - 120_000).toISOString(); // >45s -> idle
    const recent = new Date(now - 20_000).toISOString(); // <45s -> waiting
    const { container } = renderFeed(
      [
        // `coder` is seen FIRST and is doubled, but both its lanes are stale -> idle…
        mi(1, "coder", "toolu_c1", "a", { created_at: old }),
        mi(2, "coder", "toolu_c2", "b", { created_at: old }),
        // …while `tester` is seen SECOND and is doubled with a recent lane -> waiting,
        // which outranks idle and must therefore sort ahead of coder.
        mi(3, "tester", "toolu_t1", "x", { created_at: old }),
        mi(4, "tester", "toolu_t2", "y", { created_at: recent }),
        // A third role holds the ACTIVE slot so neither doubled role is the live lane —
        // otherwise `tester` would win on `working` and the waiting-vs-idle comparison
        // this test exists for would never be made. (A `waiting_worker` health would
        // flatten every lane to `waiting` and make it a tie, which is how the first
        // draft of this fixture passed for the wrong reason.)
        mi(5, "lead", null, null),
      ],
      { status: "running", health: "ok" },
    );
    const chips = [...(container.querySelectorAll('[aria-label="Crew"] button') ?? [])].map((b) =>
      b.getAttribute("title"),
    );
    // Pin the FULL ordering across three different states, not just the head: it
    // exercises waiting(1) > working(2) > idle(3) in one go. First-seen order would
    // read ["coder …", "tester …", "lead …"] and fails on the first element.
    expect(chips.map((c) => (c ?? "").split(" ")[0])).toEqual(["tester", "lead", "coder"]);
    expect(chips[0]).toMatch(/^tester ×2: waiting/);
    expect(chips[2]).toMatch(/^coder ×2: idle/);
  });

  it("sorts a STALLED role to the very front — the mockup's headline case", () => {
    // The other ordering test covers waiting > working > idle. `stalled` cannot appear
    // in that render: crewStateFor only returns it for the ACTIVE lane when run.health
    // is degraded, so it needs its own fixture — and it is priority 0, ahead of
    // everything. Together the two tests pin the whole ladder.
    const now = Date.now();
    const old = new Date(now - 120_000).toISOString(); // >45s -> idle
    const recent = new Date(now - 20_000).toISOString(); // <45s -> waiting
    const { container } = renderFeed(
      [
        mi(1, "coder", "toolu_c1", "a", { created_at: old }),
        mi(2, "coder", "toolu_c2", "b", { created_at: old }),
        mi(3, "reviewer", "toolu_r1", "r", { created_at: recent }),
        // Doubled `tester`, seen LAST, and its active lane is stalled by run health.
        mi(4, "tester", "toolu_t1", "x", { created_at: old }),
        mi(5, "tester", "toolu_t2", "y", { created_at: recent }),
      ],
      { status: "running", health: "looping" },
    );
    const chips = [...container.querySelectorAll('[aria-label="Crew"] button')].map(
      (b) => b.getAttribute("title") ?? "",
    );
    expect(chips[0]).toMatch(/^tester ×2: stalled/);
    // Seen first, but idle, so it sorts last.
    expect(chips[chips.length - 1]).toMatch(/^coder ×2: idle/);
  });

  it("orders the shipped run-busy demo fixture attention-first too", () => {
    // Against the REAL mock fixture rather than a synthetic one, so the demo everybody
    // looks at is the thing under test. run-busy has a doubled `tester` (one lane
    // active, one recent -> waiting) and a doubled `coder` (both stale -> idle), so
    // waiting must sort ahead of idle and `tester` leads the strip.
    //
    // TIME MUST BE PINNED. data.ts bakes its timestamps at MODULE LOAD via minsAgo(),
    // so they age in real time while the suite runs: the recent tester lane sits 24s
    // back, and once this file is reached more than ~21s after data.ts loads it falls
    // out of the 45s recency window, flips waiting -> idle, and the rollup reads
    // `tester ×2: working` instead. MEASURED as a real flake across repeated runs
    // (1 in 3), and it is a property of the FIXTURE's clock, not of the ordering under
    // test. Pinning to just after the newest frame makes it deterministic without
    // weakening the assertion.
    const newest = Math.max(...mockBusyMessages.map((m) => new Date(m.created_at).getTime()));
    vi.useFakeTimers();
    vi.setSystemTime(newest + 1_000);
    const { container } = renderFeed(mockBusyMessages, { status: "running", health: "ok" });
    const chips = [...container.querySelectorAll('[aria-label="Crew"] button')].map(
      (b) => b.getAttribute("title") ?? "",
    );
    expect(chips.length).toBeGreaterThan(1);
    expect(chips[0]).toMatch(/^tester ×2: waiting/);
    // …and `coder ×2`, seen earlier in the stream, is pushed behind it.
    const coderAt = chips.findIndex((c) => c.startsWith("coder"));
    expect(coderAt).toBeGreaterThan(0);
  });

  it("keeps the lane label SHRINKABLE, the single cause of the narrow-viewport overflow", () => {
    // jsdom does no layout, so this CANNOT prove the page stops scrolling — that was
    // measured in real Chrome (390px: scrollWidth 432 -> 390; 560px on the busy run:
    // 579 -> 560; 640/900/1440 unchanged). What it CAN pin is the structural cause, so
    // the fix is not silently undone: the label was `shrink-0`, and at up to 48 mono
    // characters (~300px) it alone overflowed a 390px viewport because it could not
    // yield while everything around it was also fixed.
    const r = renderFeed(
      [mi(1, "coder", "toolu_A", "a reasonably long task label for the lane header")],
      { status: "running", health: "ok" },
    );
    const header = r.getByRole("button", { name: /coder · a reasonably/ }).parentElement;
    const label = header?.querySelector(".font-mono");
    expect(label).not.toBeNull();
    expect(label?.className).not.toContain("shrink-0");
    expect(label?.className).toContain("truncate");
    expect(label?.className).toContain("min-w-0");
  });

  it("makes the one-liner yield BEFORE the label — identity outranks detail", () => {
    // Shrinkable was not enough. With the label and the one-liner both at the default
    // flex-shrink:1 they yielded at the same rate and the label lost: MEASURED in
    // Chrome at 390px it rendered 11px of an 86px `· web gate UX` — zero characters —
    // so two same-role lanes stopped being tellable apart, which is the one thing this
    // PRD exists to deliver. The label is the lane's IDENTITY; the one-liner is detail
    // about what it is doing right now, so detail yields first.
    //
    // jsdom does no layout, so this pins the PRIORITY RELATIONSHIP rather than the
    // widths: the one-liner carries an explicit larger shrink and the label does not.
    // Post-fix widths at 390px were 46-80px per label with zero starved; full labels
    // return by 768-900px.
    const r = renderFeed(
      [
        mi(1, "coder", "toolu_A", "web gate UX", {
          kind: "tool_use",
          payload: { id: "u1", name: "Bash", input: { command: "npm run build" } },
        }),
        mi(2, "reviewer", "toolu_R", "audit", {
          kind: "tool_use",
          payload: { id: "u2", name: "Read", input: { file_path: "/x" } },
        }),
      ],
      { status: "running", health: "ok" },
    );
    const header = r.getByRole("button", { name: /coder · web gate UX/ }).parentElement;
    const label = header?.querySelector(".font-mono");
    const oneLiner = header?.querySelector("span.truncate.text-xs.text-muted");
    expect(label).not.toBeNull();
    expect(oneLiner).not.toBeNull();
    // The one-liner shrinks faster; the label carries no explicit shrink override.
    expect(oneLiner?.className).toContain("shrink-[20]");
    expect(label?.className).not.toContain("shrink-[");
  });

  it("keeps the Timeline jump strip, which groups by ROLE and never mentions instances", () => {
    selectTimelineView();
    const { container, getByRole } = renderFeed(twoParallelCoders(), {
      status: "running",
      health: "ok",
    });
    expect(container.querySelector('[aria-label="Crew"]')).not.toBeNull();
    // Timeline's chip is per-role with no instance count — the pre-#99 shape.
    expect(getByRole("button", { name: /^Jump to coder activity \((working|waiting|idle)\)$/ })).toBeTruthy();
  });
});

describe("ActivityFeed lane label is untrusted-ish text (PRD #99 Decision 7)", () => {
  // The label is model-authored prose. It renders PLAIN — never through <Markdown> —
  // and the assertions below deliberately carry BOTH an HTML and a markdown vector: a
  // markdown renderer escapes the raw <img> while still parsing **bold** and the link,
  // so an angle-bracket-only check would pass against the very sink Decision 7 exists
  // to forbid. The `strong` and `a` assertions are the load-bearing ones.
  //
  // The payload is 43 UTF-16 code units, under the 48-code-unit layout clamp (LABEL_MAX
  // is `s.length`-based, NOT runes — see the note at its declaration; identical here
  // because this payload is pure ASCII). All three vectors therefore reach the DOM
  // intact. The lead's longer draft clamped mid-link, which would have made the `a`
  // assertion vacuous against the very sink it targets — MEASURED, hence the shortening.
  const HOSTILE = '<img src=x onerror=alert(1)> **b** [l](j:1)';

  // Rendered through the REAL By-agent view, not the block in isolation: isolation can
  // miss the actual sink. Scoped to the lane HEADER because the collapsed body is still
  // in the DOM and message text DOES render through <Markdown> (RunEvent.tsx:741), which
  // would otherwise supply a false-positive <strong>.
  function hostileHeader(): HTMLElement {
    const r = renderFeed(
      [
        mi(1, "coder", "toolu_A", HOSTILE, { payload: { text: "plain body" } }),
        mi(2, "reviewer", "toolu_R", "safe", { payload: { text: "plain body" } }),
      ],
      { status: "running", health: "ok" },
    );
    const header = r
      .getByRole("button", { name: new RegExp("coder · " + HOSTILE.replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + " activity$") })
      .parentElement;
    if (!header) throw new Error("lane header not found");
    return header;
  }

  it("renders the label plain: no img, no strong, no anchor", () => {
    const header = hostileHeader();
    expect(header.querySelector("img")).toBeNull();
    expect(header.querySelector("strong")).toBeNull();
    expect(header.querySelector("a")).toBeNull();
  });

  it("keeps the raw markup as INERT TEXT", () => {
    const header = hostileHeader();
    expect(header.textContent).toContain("<img src=x");
    expect(header.textContent).toContain("**b**");
    expect(header.textContent).toContain("[l](j:1)");
  });

  it("puts the CLAMPED label — not the raw one — in the title and aria-label", () => {
    // A 65-code-unit label with a distinctive tail. The layout clamp (48 UTF-16 code
    // units) must apply to the accessible name and the tooltip too, or the a11y surface
    // leaks the unclamped model text that the visible one refuses to show.
    const LONG = "lane grouping and the conditional role rollup for PRD ninety-nine";
    const { container, getByRole } = renderFeed(
      [mi(1, "coder", "toolu_A", LONG), mi(2, "reviewer", "toolu_R", "safe")],
      { status: "running", health: "ok" },
    );
    const aria = getByRole("button", { name: /coder · lane grouping/ }).getAttribute("aria-label") ?? "";
    // The tail check comes FIRST deliberately: vitest truncates a long string in its
    // own failure output with an ellipsis, so a failing `toContain("…")` prints a
    // message that appears to contradict itself. The tail assertion fails legibly.
    expect(aria).not.toContain("ninety-nine");
    expect(aria).toContain("…");

    const dot = container.querySelector('[title^="coder · lane grouping"]');
    expect(dot).not.toBeNull();
    expect(dot?.getAttribute("title")).toContain("…");
    expect(dot?.getAttribute("title")).not.toContain("ninety-nine");

    // The visible label and the accessible name must agree — a disagreement between
    // them is the signal, so assert both carry the SAME clamped string.
    //
    // Scoped to the coder lane's own header rather than `querySelector(".font-mono")`:
    // that takes the FIRST match in the document and silently depends on the coder lane
    // sorting before the reviewer lane, so a lane-ordering change would make it assert
    // against the wrong element instead of failing.
    const header = getByRole("button", { name: /coder · lane grouping/ }).parentElement;
    const visible = header?.querySelector(".font-mono")?.textContent ?? "";
    expect(visible.endsWith("…")).toBe(true);
    expect(aria).toContain(visible);
  });
});
