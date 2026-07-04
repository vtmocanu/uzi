// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, render } from "@testing-library/react";
import type { RunMessage } from "../lib/api";
import { ActivityFeed } from "./ActivityFeed";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

function m(seq: number, kind: string, payload: unknown): RunMessage {
  return { seq, kind, agent: "lead", payload, created_at: "2026-07-04T00:00:00.000Z" };
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
