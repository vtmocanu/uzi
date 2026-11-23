// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunRequestCard } from "./RunRequestCard";
import { api, ApiError, type RunRequest } from "../lib/api";
import { RECORDED_EFFECTIVE_SCENARIOS } from "../mocks/data/effectiveSettings";
import { StartPreviewProvider } from "../lib/startPreviewStore";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { startRunFromChat: vi.fn(), getEffectiveSettings: vi.fn(), previewStartSettings: vi.fn() } };
});
// The run-start preview store reads the signed-in user; the cards below render inside a provider.
vi.mock("../auth/AuthContext", () => ({ useOptionalAuth: () => ({ user: { id: "u1" } }) }));

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

// Issue #3261: the static prediction on a pending start card.
describe("RunRequestCard — start preview", () => {
  const DEFAULT = RECORDED_EFFECTIVE_SCENARIOS[0];
  const preview = (name: string) => structuredClone(DEFAULT.start_previews.find((p) => p.name === name)!.response);

  function renderWithPreview(request: RunRequest) {
    return render(
      <MemoryRouter>
        <StartPreviewProvider generation={0}>
          <RunRequestCard request={request} />
        </StartPreviewProvider>
      </MemoryRouter>,
    );
  }

  beforeEach(() => {
    mockApi.getEffectiveSettings.mockResolvedValue(structuredClone(DEFAULT.get));
    mockApi.previewStartSettings.mockResolvedValue(preview("claude"));
  });

  it("shows the prediction with its sources and the Models link while pending", async () => {
    renderWithPreview(aRequest());
    const line = await screen.findByText(/If started now:/);
    expect(line.textContent).toContain("Claude · opus · medium");
    expect(line.textContent).toContain("model: lead template");
    expect(screen.getByRole("link", { name: "Models" }).getAttribute("href")).toBe("/settings/models");
  });

  it("chosen at create: one line per alternative and no chosen harness", async () => {
    mockApi.previewStartSettings.mockResolvedValue(preview("default start"));
    renderWithPreview(aRequest());
    expect(await screen.findByText("If started now: Chosen when started")).toBeTruthy();
    expect(screen.getByText(/^Claude · opus \(/)).toBeTruthy();
    expect(screen.getByText(/^Codex · gpt-6-sol \(/)).toBeTruthy();
  });

  it("a refused alternative shows its refusal", async () => {
    const p = preview("default start");
    p.alternatives![1] = { harness: "codex", credential: null, refusal: "no_credential_for_harness", refusal_detail: null };
    mockApi.previewStartSettings.mockResolvedValue(p);
    renderWithPreview(aRequest());
    expect(await screen.findByText("Codex: No usable credential for this harness")).toBeTruthy();
  });

  it("leaves startRunFromChat called with (repo_path, issue_iid) only", async () => {
    mockApi.startRunFromChat.mockResolvedValue({ run: { id: "r1" } } as never);
    renderWithPreview(aRequest());
    await screen.findByText(/If started now:/);
    fireEvent.click(screen.getByRole("button", { name: "Start run" }));
    await waitFor(() => expect(mockApi.startRunFromChat).toHaveBeenCalledTimes(1));
    expect(mockApi.startRunFromChat.mock.calls[0]).toEqual(["grp/proj", 42]);
  });

  it("a preview failure leaves Start enabled and the card usable", async () => {
    mockApi.previewStartSettings.mockRejectedValue(new Error("boom"));
    renderWithPreview(aRequest());
    expect(await screen.findByText("Preview unavailable; Start will validate your choices.")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Start run" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("hides the prediction once the request is started or dismissed", async () => {
    mockApi.startRunFromChat.mockResolvedValue({ run: { id: "r1" } } as never);
    renderWithPreview(aRequest());
    await screen.findByText(/If started now:/);
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText(/If started now:/)).toBeNull();
  });
});
