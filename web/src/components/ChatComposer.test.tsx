// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ChatComposer } from "./ChatComposer";
import type { ComposerGate } from "../lib/chat";

const enabled: ComposerGate = { enabled: true, reason: "" };
const ended: ComposerGate = { enabled: false, reason: "This conversation has ended." };
const capped: ComposerGate = { enabled: false, reason: "Turn limit reached (50 turns)." };

function renderComposer(props: Partial<React.ComponentProps<typeof ChatComposer>> = {}) {
  const onSend = vi.fn();
  const onEnd = vi.fn();
  render(
    <MemoryRouter>
      <ChatComposer
        gate={enabled}
        busy={false}
        workerOffline={false}
        turnNotice={null}
        queuedBehindActive={false}
        onSend={onSend}
        onEnd={onEnd}
        {...props}
      />
    </MemoryRouter>,
  );
  return { onSend, onEnd };
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("ChatComposer — enabled", () => {
  it("sends trimmed text and clears the box", () => {
    const { onSend } = renderComposer();
    const box = screen.getByLabelText("Message uzi") as HTMLTextAreaElement;
    fireEvent.change(box, { target: { value: "  hello uzi  " } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    expect(onSend).toHaveBeenCalledWith("hello uzi");
    expect(box.value).toBe("");
  });

  it("Send is disabled for empty/whitespace input", () => {
    renderComposer();
    const btn = screen.getByRole("button", { name: "Send" }) as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Message uzi"), { target: { value: "   " } });
    expect(btn.disabled).toBe(true);
  });

  it("End chat calls onEnd", () => {
    const { onEnd } = renderComposer();
    fireEvent.click(screen.getByRole("button", { name: "End chat" }));
    expect(onEnd).toHaveBeenCalled();
  });
});

describe("ChatComposer — disabled gates (terminal / turn-cap)", () => {
  it("on an ended conversation, shows the reason and no input box", () => {
    renderComposer({ gate: ended });
    expect(screen.getByText("This conversation has ended.")).toBeTruthy();
    expect(screen.queryByLabelText("Message uzi")).toBeNull();
    expect(screen.queryByRole("button", { name: "Send" })).toBeNull();
  });

  it("at the turn cap, shows the turn-limit reason and no input box", () => {
    renderComposer({ gate: capped });
    expect(screen.getByText("Turn limit reached (50 turns).")).toBeTruthy();
    expect(screen.queryByLabelText("Message uzi")).toBeNull();
  });
});

describe("ChatComposer — the three notices", () => {
  it("shows the worker-offline banner but keeps the box enabled (Decision 15)", () => {
    renderComposer({ workerOffline: true });
    expect(screen.getByText(/No worker connected/)).toBeTruthy();
    // Not a gate: input is still available so the message can queue.
    expect(screen.getByLabelText("Message uzi")).toBeTruthy();
  });

  it("shows the one-live-conversation note when queued behind an active chat", () => {
    renderComposer({ queuedBehindActive: true });
    expect(screen.getByText(/Only one chat runs at a time/)).toBeTruthy();
  });

  it("shows the turn-cap heads-up notice", () => {
    renderComposer({ turnNotice: "2 turns left in this conversation." });
    expect(screen.getByText("2 turns left in this conversation.")).toBeTruthy();
  });
});
