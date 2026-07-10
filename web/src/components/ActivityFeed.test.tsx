// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render } from "@testing-library/react";
import type { RunMessage } from "../lib/api";
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
  return { seq, kind, agent, payload, created_at };
}

// jsdom does not lay out, so a scroll container reports 0 for scrollHeight/
// clientHeight. Stub them (mirrors useFollowScroll.test) to drive follow state.
function stubMetrics(el: HTMLElement, scrollHeight: number, clientHeight: number) {
  Object.defineProperty(el, "scrollHeight", { configurable: true, get: () => scrollHeight });
  Object.defineProperty(el, "clientHeight", { configurable: true, get: () => clientHeight });
}

describe("ActivityFeed tool pairing", () => {
  it("folds a result under its call by id and renders an unmatched result standalone", () => {
    const messages: RunMessage[] = [
      m(1, "tool_use", { id: "use-paired", name: "Read", input: { file_path: "/x" } }),
      m(2, "tool_result", { tool_use_id: "use-paired", content: "paired output" }),
      m(3, "tool_result", { tool_use_id: "use-orphan", content: "orphan output" }),
    ];
    const { container } = render(
      <ActivityFeed messages={messages} runningLive={false} connected={true} terminal={true} />,
    );
    const text = container.textContent ?? "";
    expect(text).toContain("Read");
    expect(text).toContain("paired output");
    expect(text).toContain("orphan output");
    // The orphan result renders standalone (its id is surfaced in the header)…
    expect(text).toContain("use-orphan");
    // …while the paired result is folded under Read — never a standalone header,
    // so its tool_use_id never appears in the DOM.
    expect(text).not.toContain("use-paired");
  });

  it("renders a result standalone when its call was capped out of the visible window", () => {
    // >1000 messages triggers the DOM cap (last 500 visible). Put the call at the
    // very start (capped out) and its result at the very end (visible): the result
    // must still render standalone, not vanish.
    const messages: RunMessage[] = [
      m(1, "tool_use", { id: "straddle-call", name: "Read", input: { file_path: "/x" } }),
    ];
    for (let seq = 2; seq <= 1001; seq++) messages.push(m(seq, "text", { text: `filler ${seq}` }));
    messages.push(m(1002, "tool_result", { tool_use_id: "straddle-call", content: "straddle result" }));

    const { container } = render(
      <ActivityFeed messages={messages} runningLive={false} connected={true} terminal={true} />,
    );
    const text = container.textContent ?? "";
    // The cap is active (expander shown) and the call is NOT in the visible slice…
    expect(text).toContain("earlier messages");
    // …so the result renders standalone (surfacing its id) rather than disappearing.
    expect(text).toContain("straddle result");
    expect(text).toContain("straddle-call");
  });

  it("renders the reconnecting banner only when disconnected", () => {
    const messages = [m(1, "text", { text: "hi" })];
    const online = render(
      <ActivityFeed messages={messages} runningLive={true} connected={true} terminal={false} />,
    );
    expect(online.container.textContent).not.toContain("Reconnecting");
  });

  it("promotes a persistent disconnect to a reconnecting banner after ~3s", () => {
    vi.useFakeTimers();
    const messages = [m(1, "text", { text: "hi" })];
    const { container } = render(
      <ActivityFeed messages={messages} runningLive={true} connected={false} terminal={false} />,
    );
    // Not shown immediately — a brief blip must not flash the banner.
    expect(container.textContent).not.toContain("Reconnecting");
    act(() => {
      vi.advanceTimersByTime(3000);
    });
    expect(container.textContent).toContain("Reconnecting");
  });
});

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

describe("ActivityFeed agent collapse (M3)", () => {
  it("collapsing lead reduces BOTH lead blocks to header rows, worker stays open", () => {
    const { getAllByRole, getByRole } = render(
      <ActivityFeed messages={leadWorkerLead()} runningLive={true} connected={true} terminal={false} />,
    );
    // Three blocks, all expanded initially.
    const leadToggles = getAllByRole("button", { name: /lead activity$/ });
    expect(leadToggles).toHaveLength(2);
    for (const t of leadToggles) expect(t.getAttribute("aria-expanded")).toBe("true");

    // Collapse lead via the first lead block's chevron: both lead blocks collapse.
    fireEvent.click(leadToggles[0]);
    for (const t of getAllByRole("button", { name: /lead activity$/ }))
      expect(t.getAttribute("aria-expanded")).toBe("false");
    // Worker block is untouched.
    expect(getByRole("button", { name: /worker activity$/ }).getAttribute("aria-expanded")).toBe("true");

    // Both lead bodies are hidden; the worker body is not.
    const leadA = document.getElementById("agent-body-1");
    const leadC = document.getElementById("agent-body-7");
    const worker = document.getElementById("agent-body-4");
    expect(leadA?.hidden).toBe(true);
    expect(leadC?.hidden).toBe(true);
    expect(worker?.hidden).toBe(false);
    // Worker prose still shows; a collapsed lead block still contains its body in
    // the DOM (hidden, not unmounted) so aria-controls stays valid.
    expect(worker?.textContent).toContain("implementing now");
  });

  it("shows a correct hidden-content summary per collapsed block", () => {
    const { getAllByRole, container } = render(
      <ActivityFeed messages={leadWorkerLead()} runningLive={true} connected={true} terminal={false} />,
    );
    // No summary while expanded.
    expect(container.textContent).not.toContain("tool call");

    fireEvent.click(getAllByRole("button", { name: /lead activity$/ })[0]);
    // Each lead block: 1 prose message + 1 tool call (its result is folded, not a row).
    const summaries = Array.from(container.querySelectorAll("span")).filter((s) =>
      s.textContent === "1 message, 1 tool call hidden",
    );
    expect(summaries).toHaveLength(2);
  });

  it("a new message from a collapsed agent keeps it collapsed and counts in the pill", () => {
    const base = [m(1, "text", { text: "planning" }, "lead")];
    const { container, getByRole, rerender } = render(
      <ActivityFeed messages={base} runningLive={true} connected={true} terminal={false} />,
    );
    // Collapse lead.
    fireEvent.click(getByRole("button", { name: /lead activity$/ }));
    // The user scrolls up, so follow is paused.
    const log = container.querySelector('[role="log"]') as HTMLElement;
    stubMetrics(log, 1000, 200);
    log.scrollTop = 0;
    fireEvent.scroll(log);

    // A new lead message arrives while collapsed + paused.
    rerender(
      <ActivityFeed
        messages={[...base, m(2, "text", { text: "still going" }, "lead")]}
        runningLive={true}
        connected={true}
        terminal={false}
      />,
    );
    // Still collapsed (no auto-expand)…
    expect(getByRole("button", { name: /lead activity$/ }).getAttribute("aria-expanded")).toBe("false");
    // …but the follow pill counted it.
    expect(container.textContent).toContain("1 new");
  });

  it("re-pins to the tail when toggling while following (no un-follow)", () => {
    const { container, getByRole } = render(
      <ActivityFeed messages={leadWorkerLead()} runningLive={true} connected={true} terminal={false} />,
    );
    const log = container.querySelector('[role="log"]') as HTMLElement;
    stubMetrics(log, 1000, 200);
    // Drift from the bottom WITHOUT a user scroll (mimics a height change): the
    // follow ref is still armed. Toggling must snap the view back to the tail.
    log.scrollTop = 500;
    fireEvent.click(getByRole("button", { name: /worker activity$/ }));
    expect(log.scrollTop).toBe(1000);
  });

  it("toggles aria-expanded on chevron click", () => {
    const { getByRole } = render(
      <ActivityFeed messages={[m(1, "text", { text: "hi" })]} runningLive={false} connected={true} terminal={true} />,
    );
    expect(getByRole("button", { name: /lead activity$/ }).getAttribute("aria-expanded")).toBe("true");
    fireEvent.click(getByRole("button", { name: /lead activity$/ }));
    expect(getByRole("button", { name: /lead activity$/ }).getAttribute("aria-expanded")).toBe("false");
  });
});

describe("ActivityFeed header emphasis (M3)", () => {
  it("marks the active agent's badge with a pulsing dot", () => {
    const { getByText, container } = render(
      <ActivityFeed messages={[m(1, "text", { text: "hi" })]} runningLive={true} connected={true} terminal={false} />,
    );
    expect(getByText("active")).toBeTruthy();
    // The active badge's dot pulses (built-in animate-pulse, no CSS file).
    expect(container.querySelector(".animate-pulse")).not.toBeNull();
  });

  it("shows a static (non-pulsing) dot for an idle agent", () => {
    const { getByText, container } = render(
      <ActivityFeed messages={[m(1, "text", { text: "hi" })]} runningLive={false} connected={true} terminal={true} />,
    );
    expect(getByText("idle")).toBeTruthy();
    expect(container.querySelector(".animate-pulse")).toBeNull();
  });

  it("renders a relative timestamp with the absolute ISO in the title", () => {
    const iso = new Date(Date.now() - 6 * 60 * 1000).toISOString();
    const { getByTitle } = render(
      <ActivityFeed
        messages={[m(1, "text", { text: "hi" }, "lead", iso)]}
        runningLive={true}
        connected={true}
        terminal={false}
      />,
    );
    expect(getByTitle(iso).textContent).toBe("6m ago");
  });

  it("renders 'just now' for a very recent timestamp", () => {
    const iso = new Date().toISOString();
    const { getByTitle } = render(
      <ActivityFeed
        messages={[m(1, "text", { text: "hi" }, "lead", iso)]}
        runningLive={true}
        connected={true}
        terminal={false}
      />,
    );
    expect(getByTitle(iso).textContent).toBe("just now");
  });

  it("renders status messages as hairline meta divider lines", () => {
    const { container } = render(
      <ActivityFeed
        messages={[
          m(1, "status", { event: "init", model: "claude-opus-4-8" }, "lead"),
          m(2, "text", { text: "starting" }, "lead"),
        ]}
        runningLive={true}
        connected={true}
        terminal={false}
      />,
    );
    // describeStatus output renders (as a divider, flanked by hairline rules).
    expect(container.textContent).toContain("agent started (claude-opus-4-8)");
    expect(container.querySelector(".h-px")).not.toBeNull();
  });
});

describe("ActivityFeed accessibility (M4)", () => {
  it("routes only meaningful transitions to the live region, never tool frames", () => {
    const region = () => document.querySelector('[aria-live="polite"]') as HTMLElement;
    const base = [
      m(1, "status", { event: "init", model: "claude-opus-4-8" }, "lead"),
      m(2, "text", { text: "planning" }, "lead"),
      m(3, "tool_use", { id: "u1", name: "Read", input: { file_path: "/x" } }, "lead"),
    ];
    const { rerender } = render(
      <ActivityFeed messages={base} runningLive={true} connected={true} terminal={false} />,
    );
    const afterMount = region().textContent;
    expect(afterMount).toContain("agent started (claude-opus-4-8)");

    // Appending only tool frames must NOT change the announced text.
    const withTools = [
      ...base,
      m(4, "tool_result", { tool_use_id: "u1", content: "ok" }, "lead"),
      m(5, "tool_use", { id: "u2", name: "Bash", input: { command: "ls" } }, "lead"),
    ];
    rerender(
      <ActivityFeed messages={withTools} runningLive={true} connected={true} terminal={false} />,
    );
    expect(region().textContent).toBe(afterMount);

    // An error transition DOES update it.
    rerender(
      <ActivityFeed
        messages={[...withTools, m(6, "error", { text: "push failed" }, "lead")]}
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
    render(
      <ActivityFeed messages={[m(1, "status", { text: huge }, "lead")]} runningLive={true} connected={true} terminal={false} />,
    );
    const text = region().textContent ?? "";
    expect(text.length).toBeLessThanOrEqual(200);
    expect(text).toContain("xxx"); // the (truncated) status still shows
  });

  it("mutes the scroll container's implicit live region to aria-live=off", () => {
    const { container } = render(
      <ActivityFeed messages={[m(1, "text", { text: "hi" })]} runningLive={true} connected={true} terminal={false} />,
    );
    expect(container.querySelector('[role="log"]')?.getAttribute("aria-live")).toBe("off");
  });

  it("gathers consecutive tool rows into ONE rail, split by interleaved prose", () => {
    const twoTools = [
      m(1, "tool_use", { id: "a", name: "Read", input: { file_path: "/x" } }, "lead"),
      m(2, "tool_use", { id: "b", name: "Grep", input: { pattern: "y" } }, "lead"),
    ];
    const { container, unmount } = render(
      <ActivityFeed messages={twoTools} runningLive={false} connected={true} terminal={true} />,
    );
    // A single continuous rail wraps both tools (was one border per row).
    expect(container.querySelectorAll('[class*="tool-rail"]')).toHaveLength(1);
    unmount();

    const split = [
      m(1, "tool_use", { id: "a", name: "Read", input: { file_path: "/x" } }, "lead"),
      m(2, "text", { text: "note between" }, "lead"),
      m(3, "tool_use", { id: "b", name: "Grep", input: { pattern: "y" } }, "lead"),
    ];
    const { container: c2 } = render(
      <ActivityFeed messages={split} runningLive={false} connected={true} terminal={true} />,
    );
    // Interleaved prose breaks the rail into two.
    expect(c2.querySelectorAll('[class*="tool-rail"]')).toHaveLength(2);
  });
});
