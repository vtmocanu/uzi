// @vitest-environment jsdom
//
// StartPreview (issue #3261): the shared read-only "if started now" line. Rendered inside the
// real store provider with the two api reads mocked, so each state is the one a surface sees.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { api } from "../lib/api";
import type { StartPreview as StartPreviewDTO } from "../lib/apiTypes";
import { RECORDED_EFFECTIVE_SCENARIOS } from "../mocks/data/effectiveSettings";
import { deferred } from "../test-helpers";
import { DEFAULT_START_PREVIEW_REQUEST } from "../lib/startPreview";
import { StartPreviewProvider } from "../lib/startPreviewStore";
import { StartPreview, type StartPreviewVariant } from "./StartPreview";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { getEffectiveSettings: vi.fn(), previewStartSettings: vi.fn() } };
});
vi.mock("../auth/AuthContext", () => ({ useOptionalAuth: () => ({ user: { id: "u1" } }) }));

const mockApi = vi.mocked(api);
const DEFAULT = RECORDED_EFFECTIVE_SCENARIOS[0];
const preview = (name: string): StartPreviewDTO =>
  structuredClone(DEFAULT.start_previews.find((p) => p.name === name)!.response);

beforeEach(() => {
  mockApi.getEffectiveSettings.mockResolvedValue(structuredClone(DEFAULT.get));
  mockApi.previewStartSettings.mockResolvedValue(preview("claude"));
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderPreview(variant: StartPreviewVariant, generation: number | null = 1) {
  return render(
    <MemoryRouter>
      <StartPreviewProvider generation={generation}>
        <StartPreview variant={variant} request={DEFAULT_START_PREVIEW_REQUEST} />
      </StartPreviewProvider>
    </MemoryRouter>,
  );
}

describe("StartPreview compact", () => {
  it("is a closed real button with aria-expanded, aria-controls and a 24px target class", async () => {
    renderPreview("compact");
    const button = await screen.findByRole("button", { name: /If started now: Claude · opus · medium/ });
    expect(button.getAttribute("type")).toBe("button");
    expect(button.getAttribute("aria-expanded")).toBe("false");
    expect(button.className).toContain("min-h-6");
    const panel = document.getElementById(button.getAttribute("aria-controls")!)!;
    expect(panel.hidden).toBe(true);
  });

  it("toggles open and shut and shows the server values with their sources", async () => {
    renderPreview("compact");
    const button = await screen.findByRole("button", { name: /If started now/ });
    fireEvent.click(button);
    expect(button.getAttribute("aria-expanded")).toBe("true");
    const panel = document.getElementById(button.getAttribute("aria-controls")!)!;
    expect(panel.hidden).toBe(false);
    const rows = within(panel);
    expect(rows.getByText("Harness").nextElementSibling?.textContent).toContain("Claude");
    expect(rows.getByText("Model").nextElementSibling?.textContent).toContain("opus");
    expect(rows.getByText("Effort").nextElementSibling?.textContent).toContain("medium");
    expect(rows.getByText("Token")).toBeTruthy();
    fireEvent.click(button);
    expect(button.getAttribute("aria-expanded")).toBe("false");
    expect(panel.hidden).toBe(true);
  });

  it("words the note from fixed_at and links to Run defaults", async () => {
    renderPreview("compact");
    fireEvent.click(await screen.findByRole("button", { name: /If started now/ }));
    // The claude preview mixes a claim-fixed model and credential with a run_override harness.
    expect(screen.getByText(/A prediction: claim-time settings are resolved when a worker picks the run up/)).toBeTruthy();
    expect(screen.getByText(/A choice made here is fixed on the run now/)).toBeTruthy();
    expect(screen.getByRole("link", { name: "Run defaults" }).getAttribute("href")).toBe("/settings/run-defaults");
  });

  it("chosen at create: summary says so and the panel lists one entry per alternative", async () => {
    mockApi.previewStartSettings.mockResolvedValue(preview("default start"));
    renderPreview("compact");
    const button = await screen.findByRole("button", { name: /If started now: Chosen when started/ });
    fireEvent.click(button);
    const panel = document.getElementById(button.getAttribute("aria-controls")!)!;
    const rows = within(panel);
    expect(rows.getByText("Claude").nextElementSibling?.textContent).toContain("opus");
    expect(rows.getByText("Codex").nextElementSibling?.textContent).toContain("gpt-6-sol");
    // A chosen harness is never invented.
    expect(rows.queryByText("Model")).toBeNull();
    // The harness row says it once, with the server's note and no repeated source label.
    expect(rows.getByText("Harness").nextElementSibling?.textContent).toBe(
      "Chosen when started · chosen by the soonest weekly reset",
    );
  });

  it("a default start with a create-fixed credential does not claim a choice was made", async () => {
    mockApi.previewStartSettings.mockResolvedValue(preview("default start"));
    renderPreview("compact");
    fireEvent.click(await screen.findByRole("button", { name: /If started now/ }));
    expect(screen.queryByText(/A choice made here/)).toBeNull();
    expect(screen.queryByText(/fixed when the run is created/)).toBeNull();
  });

  it("showNote=false hides the note lines but keeps the link", async () => {
    render(
      <MemoryRouter>
        <StartPreviewProvider generation={1}>
          <StartPreview variant="open" showNote={false} request={DEFAULT_START_PREVIEW_REQUEST} />
        </StartPreviewProvider>
      </MemoryRouter>,
    );
    await screen.findByRole("button", { name: /If it started now/ });
    expect(screen.queryByText(/A prediction/)).toBeNull();
    expect(screen.queryByText(/A choice made here/)).toBeNull();
    expect(screen.getByRole("link", { name: "Run defaults" }).className).toContain("min-h-6");
  });

  it("keeps the server's harness notes when the harness is undecided (panel and static)", async () => {
    const overflow = RECORDED_EFFECTIVE_SCENARIOS.find((s) => s.name === "overflowCodexDefault")!;
    mockApi.getEffectiveSettings.mockResolvedValue(structuredClone(overflow.get));
    mockApi.previewStartSettings.mockResolvedValue(
      structuredClone(overflow.start_previews.find((p) => p.name === "default start")!.response),
    );
    const note = "may move to the other harness near its limit";
    const { unmount } = renderPreview("compact");
    fireEvent.click(await screen.findByRole("button", { name: /If started now/ }));
    expect(screen.getByText("Harness").nextElementSibling?.textContent).toContain(note);
    unmount();
    renderPreview("static");
    expect(await screen.findByText(note)).toBeTruthy();
  });

  it("a refusal renders as text with no model or effort rows", async () => {
    mockApi.previewStartSettings.mockResolvedValue(preview("codex with a token override"));
    renderPreview("compact");
    const button = await screen.findByRole("button", { name: /A token choice does not apply to this harness/ });
    fireEvent.click(button);
    expect(screen.queryByText("Model")).toBeNull();
    expect(screen.queryByText("Effort")).toBeNull();
  });

  it("shows Resolving… while loading and the error text when the preview fails", async () => {
    const d = deferred<StartPreviewDTO>();
    mockApi.previewStartSettings.mockReturnValue(d.promise);
    renderPreview("compact");
    expect(await screen.findByText("Resolving…")).toBeTruthy();
    d.reject(new Error("boom"));
    expect(await screen.findByText("Preview unavailable; Start will validate your choices.")).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("fetches nothing and shows Resolving… until the page applies its first generation", async () => {
    renderPreview("compact", null);
    expect(await screen.findByText("Resolving…")).toBeTruthy();
    expect(mockApi.getEffectiveSettings).not.toHaveBeenCalled();
    expect(mockApi.previewStartSettings).not.toHaveBeenCalled();
  });

  it("renders nothing outside a provider", () => {
    const { container } = render(
      <MemoryRouter>
        <StartPreview variant="compact" request={DEFAULT_START_PREVIEW_REQUEST} />
      </MemoryRouter>,
    );
    expect(container.textContent).toBe("");
  });

  it("renders nothing for a null request", () => {
    const { container } = render(
      <MemoryRouter>
        <StartPreviewProvider generation={1}>
          <StartPreview variant="compact" request={null} />
        </StartPreviewProvider>
      </MemoryRouter>,
    );
    expect(container.textContent).toBe("");
    expect(mockApi.previewStartSettings).not.toHaveBeenCalled();
  });
});

describe("StartPreview open", () => {
  it("is headed 'If it started now', open by default, and can be collapsed", async () => {
    renderPreview("open");
    const button = await screen.findByRole("button", { name: /If it started now: Claude · opus · medium/ });
    expect(button.getAttribute("aria-expanded")).toBe("true");
    expect(document.getElementById(button.getAttribute("aria-controls")!)!.hidden).toBe(false);
    fireEvent.click(button);
    expect(button.getAttribute("aria-expanded")).toBe("false");
  });
});

describe("StartPreview static", () => {
  it("is one line with the sources and the Run defaults link, no disclosure", async () => {
    renderPreview("static");
    const line = await screen.findByText(/If started now:/);
    expect(line.textContent).toContain("Claude · opus · medium");
    expect(line.textContent).toContain("model: ");
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.getByRole("link", { name: "Run defaults" }).getAttribute("href")).toBe("/settings/run-defaults");
  });

  it("chosen at create: one line per alternative, naming no chosen harness", async () => {
    mockApi.previewStartSettings.mockResolvedValue(preview("default start"));
    renderPreview("static");
    expect(await screen.findByText("If started now: Chosen when started")).toBeTruthy();
    expect(screen.getByText(/^Claude · opus \(/)).toBeTruthy();
    expect(screen.getByText(/^Codex · gpt-6-sol \(/)).toBeTruthy();
  });

  it("a refused alternative shows its refusal", async () => {
    const p = preview("default start");
    p.alternatives![1] = { harness: "codex", credential: null, refusal: "no_credential_for_harness", refusal_detail: null };
    mockApi.previewStartSettings.mockResolvedValue(p);
    renderPreview("static");
    await waitFor(() => expect(screen.getByText("Codex: No usable credential for this harness")).toBeTruthy());
  });
});
