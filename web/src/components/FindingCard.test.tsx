// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { FindingCard } from "./FindingCard";
import { RunEventRow } from "./RunEvent";
import { api, ApiError, type RunMessage } from "../lib/api";

// Mock only the network; the inert-text helpers (stripUnsafeChars) run for real.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: { fileFinding: vi.fn(), dismissFinding: vi.fn(), findingIssueDraft: vi.fn() },
  };
});

const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function findingMessage(payload: Record<string, unknown>): RunMessage {
  return {
    seq: 1,
    kind: "finding",
    agent: "coder",
    agent_instance: null,
    agent_label: null,
    payload,
    created_at: "2026-07-05T12:00:00Z",
  };
}

describe("FindingCard (PRD #333 M7)", () => {
  it("renders info-toned with three actions and inert text", () => {
    const { container } = render(
      <FindingCard
        id="find-1"
        title="Leaked ticker in sweepLoop"
        location="api/internal/sweeper.go#sweepLoop"
        confidence="high"
        labels={["bug"]}
      />,
    );
    // Info/blue accent (D10), not the amber gate / orange action tones.
    expect(container.querySelector(".border-info\\/40")).toBeTruthy();
    // The three human-gated actions.
    expect(screen.getByRole("button", { name: "File" })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Edit .* file/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Dismiss" })).toBeTruthy();
    // The agent text is present as escaped children.
    expect(screen.getByText("Leaked ticker in sweepLoop")).toBeTruthy();
    expect(screen.getByText("api/internal/sweeper.go#sweepLoop")).toBeTruthy();
  });

  it("renders a bidi/control-laden payload INERT — no format char reaches the DOM", () => {
    const RLO = "\u202E";
    const ESC = "\u001B";
    const ZWSP = "\u200B";
    const { container } = render(
      <RunEventRow
        msg={findingMessage({
          id: "find-1",
          title: `Leak${RLO}ed ${ESC}ticker`,
          location: `api/${ZWSP}sweeper.go#loop`,
          labels: [],
        })}
        live={false}
      />,
    );
    const rendered = container.textContent ?? "";
    // The dispatch worked (the card, not the unrenderable fallback).
    expect(container.querySelector(".border-info\\/40")).toBeTruthy();
    // No control/format character survived to the DOM (issue #124: escaping does not strip Cf).
    expect(rendered).not.toMatch(/[\p{Cc}\p{Cf}]/u);
    // The markup never became an element.
    expect(rendered).toContain("Leaked");
  });

  it("File flips the card to a filed state with the issue link", async () => {
    mockApi.fileFinding.mockResolvedValue({
      issue: { iid: 512, web_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/512", title: "Leaked ticker" },
    });
    render(<FindingCard id="find-1" title="Leaked ticker" location="a.go#loop" labels={[]} />);

    fireEvent.click(screen.getByRole("button", { name: "File" }));

    await waitFor(() => expect(screen.getByText("Issue filed.")).toBeTruthy());
    expect(mockApi.fileFinding).toHaveBeenCalledWith("find-1", undefined);
    const link = screen.getByRole("link", { name: /#512/ });
    expect(link.getAttribute("href")).toBe("https://gitlab.example.com/vtmocanu/uzi/-/issues/512");
  });

  it("surfaces a created-with-warning note on the filed card", async () => {
    mockApi.fileFinding.mockResolvedValue({
      issue: { iid: 512, web_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/512", title: "Leaked ticker" },
      warning: "The issue was created on the forge, but recording it in uzi failed.",
    });
    render(<FindingCard id="find-1" title="Leaked ticker" location="a.go#loop" labels={[]} />);

    fireEvent.click(screen.getByRole("button", { name: "File" }));

    // A warning is a success (the issue exists), so the card still shows filed AND the note.
    await waitFor(() => expect(screen.getByText("Issue filed.")).toBeTruthy());
    expect(screen.getByText(/recording it in uzi failed/)).toBeTruthy();
  });

  it("a stale File that 409s shows the friendly 'already filed / resolved' state, never a crash", async () => {
    mockApi.fileFinding.mockRejectedValue(new ApiError(409, "this finding is already filed or being filed"));
    const { container } = render(<FindingCard id="find-1" title="Leaked ticker" location="a.go#loop" labels={[]} />);

    fireEvent.click(screen.getByRole("button", { name: "File" }));

    await waitFor(() => expect(screen.getByText(/Already filed or resolved/)).toBeTruthy());
    // The scary raw error text never surfaces, and the resolved badge shows.
    expect(container.textContent).not.toContain("already filed or being filed");
    expect(screen.getByText("resolved")).toBeTruthy();
  });

  it("dispatches to the unrenderable fallback when the finding payload carries no id", () => {
    const { container } = render(
      <RunEventRow msg={findingMessage({ title: "no id here", location: "a.go#x", labels: [] })} live={false} />,
    );
    // No actionable id → the muted unrenderable line, not a dead-button card.
    expect(container.querySelector(".border-info\\/40")).toBeNull();
    expect(container.textContent).toContain("unrenderable finding event");
  });
});
