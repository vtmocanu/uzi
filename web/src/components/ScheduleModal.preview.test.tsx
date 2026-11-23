// @vitest-environment jsdom
//
// Issue #3260: the schedule modal's read-only "If fired and claimed now" preview. The server
// decides every value, source and timing (recorded responses in mocks/data/effectiveSettings.ts);
// the modal only displays them and never lets the preview gate Save.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ScheduleModal } from "./ScheduleModal";
import { api, ApiError, type SecretMeta } from "../lib/api";
import type { ScheduleSettingsPreview } from "../lib/apiTypes";
import { useAuth } from "../auth/AuthContext";
import { browserTimezone } from "../lib/timezone";
import { RECORDED_EFFECTIVE_SCENARIOS } from "../mocks/data/effectiveSettings";
import { mockApi as realMockApi } from "../mocks/mockApi";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listScheduleCatalog: vi.fn().mockResolvedValue({ entries: [], enablements: [] }),
      listRepos: vi.fn().mockResolvedValue({ repos: [] }),
      previewSchedule: vi.fn(),
      previewScheduleSettings: vi.fn(),
      createSchedule: vi.fn(),
      updateSchedule: vi.fn(),
      deleteSchedule: vi.fn(),
      checkRepoLabels: vi.fn(),
      ensureRepoLabels: vi.fn(),
      listSecrets: vi.fn(),
      getMySettings: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));
vi.mock("../lib/timezone", () => ({ browserTimezone: vi.fn() }));

const mockApi = vi.mocked(api);
const WAIT = { timeout: 3000 };

function fixture(scenario: string, name: string): ScheduleSettingsPreview {
  const p = RECORDED_EFFECTIVE_SCENARIOS.find((s) => s.name === scenario)?.schedule_previews.find((x) => x.name === name);
  if (!p) throw new Error(`missing fixture ${scenario}/${name}`);
  return structuredClone(p.response);
}

function secret(over: Partial<SecretMeta>): SecretMeta {
  return {
    id: "sec-1",
    kind: "anthropic_token",
    label: "key",
    is_default: false,
    enabled: true,
    disabled_at: null,
    auto_eligible: true,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}
const bothHarnesses = () => [
  secret({ id: "sec-a" }),
  secret({ id: "sec-c", kind: "openai_api_key", label: "codex-key", is_default: true, auto_eligible: false }),
];

beforeEach(async () => {
  vi.mocked(browserTimezone).mockReturnValue("UTC");
  vi.mocked(useAuth).mockReturnValue({ uziLabel: "uzi" } as unknown as ReturnType<typeof useAuth>);
  mockApi.listScheduleCatalog.mockResolvedValue(await realMockApi.listScheduleCatalog());
  mockApi.previewSchedule.mockResolvedValue({ fires: [] });
  mockApi.listRepos.mockResolvedValue({
    repos: [{ id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" }] as unknown as Awaited<
      ReturnType<typeof api.listRepos>
    >["repos"],
  });
  mockApi.checkRepoLabels.mockResolvedValue({ missing: [] });
  mockApi.ensureRepoLabels.mockResolvedValue({ ensured: [] });
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockApi.getMySettings.mockResolvedValue({
    settings: { harness_overflow_enabled: false, default_harness: null },
  } as unknown as Awaited<ReturnType<typeof api.getMySettings>>);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderModal() {
  return render(
    <MemoryRouter>
      <ScheduleModal pinned={undefined} onClose={vi.fn()} onSaved={vi.fn()} />
    </MemoryRouter>,
  );
}

const region = () => screen.findByRole("region", { name: "If fired and claimed now" }, WAIT);

// The block renders the heading first, then fills in once the debounced fetch lands.
async function readyRegion() {
  const r = await region();
  await waitFor(() => expect(within(r).queryByText("Resolving…")).toBeNull(), WAIT);
  return r;
}

async function save() {
  await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
  fireEvent.change(screen.getByLabelText("Issue number"), { target: { value: "142" } });
  const btn = screen.getByRole("button", { name: "Create schedule" });
  await waitFor(() => expect(btn.hasAttribute("disabled")).toBe(false));
  fireEvent.click(btn);
}

describe("schedule modal preview", () => {
  it("shows a pinned harness, predicted values with the server's sources and timing, and the footer", async () => {
    const p = fixture("default", "sweep pinned to claude with subagents following the lead");
    mockApi.previewScheduleSettings.mockResolvedValue(p);
    renderModal();

    const r = await region();
    expect(within(r).getByText("Resolving…")).toBeTruthy();
    expect(within(r).getByText("updates as you change the picks above")).toBeTruthy();
    await waitFor(() => expect(within(r).getByText("Harness")).toBeTruthy(), WAIT);

    const harnessValue = within(r).getByText("Harness").nextElementSibling!;
    expect(harnessValue.textContent).toBe("Claude");
    expect(harnessValue.nextElementSibling?.textContent).toContain("schedule pin · pinned · fixed when it fires");
    const modelValue = within(r).getByText(p.worker_model!.resolved_model);
    expect(modelValue.nextElementSibling?.textContent).toBe(
      "lead template · predicted · read when a worker picks the run up",
    );
    const credential = within(r).getByText("Credential").nextElementSibling!;
    expect(credential.textContent).toBe("Anthropic");
    expect(r.textContent).not.toMatch(/sec-|secret/i);
    expect(within(r).getByText("follow the lead")).toBeTruthy();

    for (const line of [
      "Pinned values are fixed when the schedule fires.",
      'Predicted values marked "fixed when it fires" (such as the harness) are decided when the schedule fires, from your settings and credentials at that moment.',
      'Values marked "read when a worker picks the run up" can still change after the schedule fires, until a worker claims the run.',
      "This preview is advisory; Save validates again.",
    ]) {
      expect(within(r).getByText(line)).toBeTruthy();
    }
  });

  it("shows a pinned model as pinned, and an automatic harness as predicted", async () => {
    mockApi.previewScheduleSettings.mockResolvedValue(fixture("default", "sweep with a model pin and no harness"));
    renderModal();
    const r = await readyRegion();
    const modelDetail = within(r).getByText("sonnet").nextElementSibling!;
    expect(modelDetail.textContent).toBe("schedule pin · pinned · fixed when it fires");
    const harnessDetail = within(r).getByText("Claude").nextElementSibling!;
    expect(harnessDetail.textContent).toContain("automatic · predicted · fixed when it fires");
  });

  it("chosen at create: one group per alternative, and the Models link appears once", async () => {
    const p = fixture("default", "unpinned sweep");
    mockApi.previewScheduleSettings.mockResolvedValue(p);
    renderModal();
    const r = await readyRegion();
    const harnessDetail = within(r).getByText("chosen when it fires").nextElementSibling!;
    expect(harnessDetail.textContent).toContain("chosen when the run is created");
    expect(harnessDetail.textContent).toContain("predicted · fixed when it fires");
    expect(within(r).getByText("Claude model")).toBeTruthy();
    expect(within(r).getByText("Codex model")).toBeTruthy();
    expect(within(r).getByText(p.alternatives![1].worker_model!.resolved_model)).toBeTruthy();
    expect(within(r).getAllByRole("link", { name: "Models (opens in new tab)" })).toHaveLength(1);
  });

  it("a refusal shows one Refused row and no duplicate per-alternative refusals", async () => {
    mockApi.previewScheduleSettings.mockResolvedValue(fixture("default", "invalid model"));
    renderModal();
    const r = await readyRegion();
    expect(within(r).getByText("Refused")).toBeTruthy();
    expect(within(r).getByText("This model ID is not valid")).toBeTruthy();
    expect(within(r).getByText("Save will validate your choices.")).toBeTruthy();
    expect(within(r).queryByText("Model")).toBeNull();
  });

  it("links the effort row to Models", async () => {
    mockApi.previewScheduleSettings.mockResolvedValue(
      fixture("default", "sweep pinned to claude with subagents following the lead"),
    );
    renderModal();
    const r = await readyRegion();
    const link = within(r).getByRole("link", { name: "Models (opens in new tab)" });
    expect(link.getAttribute("href")).toBe("/settings/models");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    expect(link.closest("dd")?.textContent).toContain("schedules can't change it");
  });

  it("an incompatible pin warns with the fallback from the response, and Save still sends", async () => {
    const p = fixture("default", "sweep pinned to codex with a claude model pin");
    mockApi.previewScheduleSettings.mockResolvedValue(p);
    mockApi.createSchedule.mockResolvedValue({} as Awaited<ReturnType<typeof api.createSchedule>>);
    renderModal();
    const r = await readyRegion();
    const fallback = p.worker_model!.resolved_model;
    expect(within(r).getByRole("status").textContent).toContain(
      `The sonnet pin does not apply to Codex runs; this schedule would use ${fallback} (lead template).`,
    );
    await save();
    await waitFor(() => expect(mockApi.createSchedule).toHaveBeenCalledTimes(1));
  });

  it("advises pinning the other harness when both are usable", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: bothHarnesses() });
    mockApi.previewScheduleSettings.mockResolvedValue(fixture("default", "sweep pinned to codex with a claude model pin"));
    renderModal();
    await screen.findByLabelText("Harness for this schedule", {}, WAIT);
    const r = await readyRegion();
    const text = within(r).getByRole("status").textContent;
    expect(text).toContain("Pin the harness to Claude.");
    expect(text).not.toContain("Pick a Codex model.");
  });

  it("advises picking a model of that harness when only one harness is usable", async () => {
    mockApi.previewScheduleSettings.mockResolvedValue(fixture("codexOnly", "sweep with a claude model pin"));
    renderModal();
    const r = await readyRegion();
    expect(screen.queryByLabelText("Harness for this schedule")).toBeNull();
    const text = within(r).getByRole("status").textContent;
    expect(text).toContain("Pick a Codex model.");
    expect(text).not.toContain("Pin the harness");
  });

  it("an ignored pin inside a chosen-at-create alternative warns with that branch's fallback", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: bothHarnesses() });
    const p = fixture("default", "unpinned sweep");
    p.alternatives![1].worker_model!.ignored = { value: "sonnet", source: "schedule_pin" };
    mockApi.previewScheduleSettings.mockResolvedValue(p);
    renderModal();
    await screen.findByLabelText("Harness for this schedule", {}, WAIT);
    const r = await readyRegion();
    const fallback = p.alternatives![1].worker_model!.resolved_model;
    const alerts = within(r).getAllByRole("status");
    expect(alerts).toHaveLength(1);
    expect(alerts[0].textContent).toContain(`The sonnet pin does not apply to Codex runs; this schedule would use ${fallback}`);
    expect(alerts[0].textContent).toContain("Pin the harness to Claude.");
  });

  it("while the preview is still loading, Save is enabled and sends", async () => {
    mockApi.previewScheduleSettings.mockReturnValue(new Promise<ScheduleSettingsPreview>(() => {}));
    mockApi.createSchedule.mockResolvedValue({} as Awaited<ReturnType<typeof api.createSchedule>>);
    renderModal();
    const r = await region();
    expect(within(r).getByText("Resolving…")).toBeTruthy();
    await save();
    await waitFor(() => expect(mockApi.createSchedule).toHaveBeenCalledTimes(1));
  });

  it("shows Resolving… and then the unavailable state when the preview rejects", async () => {
    mockApi.previewScheduleSettings.mockRejectedValue(new Error("boom"));
    renderModal();
    const r = await region();
    expect(within(r).getByText("Resolving…")).toBeTruthy();
    expect(await within(r).findByText("Preview unavailable; Save will validate your choices.", {}, WAIT)).toBeTruthy();
    expect(within(r).queryByText("Resolving…")).toBeNull();
  });

  it("a rejected preview does not block Save, and the API's 422 message still shows", async () => {
    mockApi.previewScheduleSettings.mockRejectedValue(new Error("boom"));
    mockApi.createSchedule.mockRejectedValue(new ApiError(422, "model sonnet is not available for this harness"));
    renderModal();
    const r = await region();
    expect(await within(r).findByText("Preview unavailable; Save will validate your choices.", {}, WAIT)).toBeTruthy();
    await save();
    await waitFor(() => expect(mockApi.createSchedule).toHaveBeenCalledTimes(1));
    expect(await screen.findByText("model sonnet is not available for this harness", {}, WAIT)).toBeTruthy();
  });

  it("after a successful preview, Save still calls createSchedule and surfaces its validation error", async () => {
    mockApi.previewScheduleSettings.mockResolvedValue(
      fixture("default", "sweep pinned to claude with subagents following the lead"),
    );
    mockApi.createSchedule.mockRejectedValue(new ApiError(422, "selector is invalid"));
    renderModal();
    await readyRegion();
    await save();
    await waitFor(() => expect(mockApi.createSchedule).toHaveBeenCalledTimes(1));
    expect(await screen.findByText("selector is invalid", {}, WAIT)).toBeTruthy();
  });

  it("changing the model sends a new request carrying the new model", async () => {
    mockApi.previewScheduleSettings.mockResolvedValue(
      fixture("default", "sweep pinned to claude with subagents following the lead"),
    );
    renderModal();
    await readyRegion();
    expect(mockApi.previewScheduleSettings.mock.calls[0]?.[0]).toMatchObject({ model: null, target: "issue" });

    fireEvent.change(screen.getByLabelText("Model (optional)"), { target: { value: "opus" } });
    await waitFor(
      () =>
        expect(mockApi.previewScheduleSettings.mock.calls.some(([req]) => req.model === "opus")).toBe(true),
      WAIT,
    );
  });

  it("shows the second request's content when the first resolves after it", async () => {
    let resolveFirst!: (p: ScheduleSettingsPreview) => void;
    const first = new Promise<ScheduleSettingsPreview>((res) => {
      resolveFirst = res;
    });
    const second = fixture("default", "sweep pinned to claude with subagents following the lead");
    mockApi.previewScheduleSettings.mockReturnValueOnce(first).mockResolvedValue(second);
    renderModal();
    await waitFor(() => expect(mockApi.previewScheduleSettings).toHaveBeenCalledTimes(1), WAIT);

    fireEvent.change(screen.getByLabelText("Model (optional)"), { target: { value: "opus" } });
    await waitFor(() => expect(mockApi.previewScheduleSettings).toHaveBeenCalledTimes(2), WAIT);
    const r = await readyRegion();
    expect(within(r).getByText("Claude")).toBeTruthy();

    resolveFirst(fixture("default", "sweep pinned to codex with a claude model pin"));
    await new Promise((res) => setTimeout(res, 50));
    expect(within(r).getByText("Claude")).toBeTruthy();
    expect(within(r).queryByText("Codex")).toBeNull();
    expect(within(r).queryByRole("status")).toBeNull();
  });
});
