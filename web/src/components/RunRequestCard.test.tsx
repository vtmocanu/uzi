// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunRequestCard } from "./RunRequestCard";
import { api, ApiError, type RunRequest } from "../lib/api";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { startRunFromChat: vi.fn() } };
});

const mockApi = vi.mocked(api);

function aRequest(over: Partial<RunRequest> = {}): RunRequest {
  return { repo_path: "grp/proj", issue_iid: 42, title: "Speed up the poller", ...over };
}

function renderCard(request: RunRequest) {
  return render(
    <MemoryRouter>
      <RunRequestCard request={request} />
    </MemoryRouter>,
  );
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("RunRequestCard — model text is inert", () => {
  it("strips bidi/zero-width chars and never renders a model link before start", () => {
    const { container } = renderCard(
      aRequest({ title: "look at ‮http://evil.example", repo_path: "grp/​proj" }),
    );
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    // A pending card renders ZERO anchors (Link only appears after a successful start).
    expect(container.querySelector("a")).toBeNull();
    expect(screen.getByText("grp/proj")).toBeTruthy();
  });

  it("shows Start run / Dismiss and the issue number", () => {
    renderCard(aRequest());
    expect(screen.getByRole("button", { name: "Start run" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Dismiss" })).toBeTruthy();
    expect(screen.getByText(/#42/)).toBeTruthy();
  });
});

describe("RunRequestCard — start is the only write path", () => {
  it("on Start, calls startRunFromChat and links to the started run", async () => {
    mockApi.startRunFromChat.mockResolvedValue({ run: { id: "run-9" } as never });

    const { container } = renderCard(aRequest());
    fireEvent.click(screen.getByRole("button", { name: "Start run" }));

    await waitFor(() => expect(screen.getByText(/Run started/)).toBeTruthy());
    expect(mockApi.startRunFromChat).toHaveBeenCalledWith("grp/proj", 42);
    const anchors = container.querySelectorAll("a");
    expect(anchors).toHaveLength(1);
    expect(anchors[0].getAttribute("href")).toBe("/runs/run-9");
  });

  it("surfaces a PRD-gate refusal as the card error, starts nothing visible", async () => {
    mockApi.startRunFromChat.mockRejectedValue(
      new ApiError(422, "issue has no PRD link; add a prds/*.md link before starting a run"),
    );

    renderCard(aRequest());
    fireEvent.click(screen.getByRole("button", { name: "Start run" }));

    await waitFor(() => expect(screen.getByText(/no PRD link/)).toBeTruthy());
    expect(screen.queryByText(/Run started/)).toBeNull();
  });

  it("on Dismiss, starts nothing", () => {
    renderCard(aRequest());
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.getByText(/No run was started/)).toBeTruthy();
    expect(mockApi.startRunFromChat).not.toHaveBeenCalled();
  });
});
