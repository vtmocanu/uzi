// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render } from "@testing-library/react";
import type { Run, RunHealth, RunMessage, RunStatus } from "../lib/api";
import { ActivityFeed } from "./ActivityFeed";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

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

  it("clicking a crew chip expands that agent", () => {
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

  it("collapsing lead reduces BOTH lead blocks, worker untouched (keyed by agent name)", () => {
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
