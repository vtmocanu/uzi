// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";
import type { RunMessage } from "../lib/api";
import { ChatMessages } from "./ChatMessages";

afterEach(cleanup);

function msg(partial: Partial<RunMessage> & { kind: string; seq: number }): RunMessage {
  return {
    agent: "lead",
    agent_instance: null,
    agent_label: null,
    payload: {},
    created_at: "2026-07-27T00:00:00.000Z",
    ...partial,
  };
}

// The chat surface renders tool rows through the SAME RunEventRow/ToolResultBody
// as the run feed, so the PRD #116 "blocked" state must reach it for free —
// guardrail denies genuinely happen in chat (the Bash + path guards are wired at
// agent/src/chat-executor.ts:259). This pins that inheritance; the state machine
// itself is covered in RunEvent.test.tsx.
describe("ChatMessages tool rows (PRD #116)", () => {
  const DENY = "denied by guardrail: reading /proc is not permitted";

  const renderChat = (messages: RunMessage[]) =>
    render(<ChatMessages chatId="c1" messages={messages} connected={true} live={false} />);

  it("renders a guardrail-deny tool_result as the neutral blocked chip", () => {
    const { container, getByRole } = renderChat([
      msg({ seq: 1, kind: "user_message", payload: { text: "what is in /proc/1/environ?" } }),
      msg({ seq: 2, kind: "tool_use", payload: { id: "A", name: "Bash", input: { command: "cat /proc/1/environ" } } }),
      msg({ seq: 3, kind: "tool_result", payload: { tool_use_id: "A", content: DENY, is_error: true } }),
    ]);
    const chip = getByRole("button", { name: /show Bash blocked output/i });
    expect(chip.textContent).toContain("blocked");
    expect(chip.textContent).not.toContain("error");
    // Collapsed by default, non-danger tone, reason preserved verbatim.
    expect(chip.getAttribute("aria-expanded")).toBe("false");
    for (const el of Array.from(container.querySelectorAll("*"))) {
      expect(el.className.toString()).not.toMatch(/(border|bg|text)-danger/);
    }
    const pre = container.querySelector("pre");
    expect(pre?.hasAttribute("hidden")).toBe(true);
    expect(pre?.textContent).toBe(DENY);
  });

  it("still renders a genuine tool failure in the chat surface as a red error", () => {
    const { container, getByRole } = renderChat([
      msg({ seq: 1, kind: "tool_use", payload: { id: "A", name: "Bash", input: { command: "false" } } }),
      msg({ seq: 2, kind: "tool_result", payload: { tool_use_id: "A", content: "boom", is_error: true } }),
    ]);
    const chip = getByRole("button", { name: /hide Bash error output/i });
    expect(chip.textContent).toContain("error");
    expect(chip.className).toContain("text-danger");
    expect(container.querySelector("pre")?.hasAttribute("hidden")).toBe(false);
  });
});
