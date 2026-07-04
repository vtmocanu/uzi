// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render } from "@testing-library/react";
import type { RunMessage } from "../lib/api";
import { ActivityFeed } from "./ActivityFeed";

afterEach(cleanup);

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

  it("renders the reconnecting banner only when disconnected", () => {
    const messages = [m(1, "text", { text: "hi" })];
    const online = render(
      <ActivityFeed messages={messages} runningLive={true} connected={true} terminal={false} />,
    );
    expect(online.container.textContent).not.toContain("Reconnecting");
  });
});
