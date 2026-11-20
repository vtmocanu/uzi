// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ScheduleListRow } from "./ScheduleListRow";
import { mockApi } from "../mocks/mockApi";
import { api, type Schedule } from "../lib/api";
import type { ScheduleSettingsPreview } from "../lib/apiTypes";
import { RECORDED_EFFECTIVE_SCENARIOS } from "../mocks/data/effectiveSettings";

vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ uziLabel: "uzi" }) }));
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api, previewScheduleSettings: vi.fn() } };
});
const previewMock = vi.mocked(api.previewScheduleSettings);
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});
function tree(s: Schedule) {
  return <MemoryRouter><table><tbody><ScheduleListRow s={s} name="Sweep"
    busy={false} addBusy={false} pauseNote={null} repos={[]} onToggle={vi.fn()}
    onRunNow={vi.fn()} onEdit={vi.fn()} onReset={vi.fn()} onClone={vi.fn()}
    onRemove={vi.fn()} onAddRepo={vi.fn()} /></tbody></table></MemoryRouter>;
}
function row(s: Schedule) {
  return render(tree(s));
}
describe("schedule capacity options", () => {
  it.each(["user", "default"] as const)("shows gated and ungated batch options for %s rows", async (origin) => {
    const template = (await mockApi.listSchedules()).find((s) => s.target === "sweep")!;
    const s = { ...template, origin, capacity_limit: 4, capacity_room_needed: 2, max_issues: 3 };
    const view = row(s);
    const limit = screen.getByText("limit 4 · room 2");
    expect(limit.className).toContain("text-brand");
    expect(screen.getByText("3 at a time")).toBeTruthy();
    view.unmount();
    row({ ...s, capacity_limit: null, capacity_room_needed: null, max_issues: null });
    expect(screen.getByText("no limit").className).toContain("text-muted");
    expect(screen.getByText("unlimited at a time")).toBeTruthy();
  });
  it("describes null N as available room and shows a blocked last fire", async () => {
    const s = await mockApi.getSchedule("sch-capacity-blocked");
    row({ ...s, max_issues: null });
    expect(screen.getByText("available room at a time")).toBeTruthy();
    expect(screen.getByText("Waiting for room")).toBeTruthy();
    expect(screen.getByText(/space for 1 more run; needs 2/)).toBeTruthy();
  });
});

describe("schedule model chips", () => {
  const base = async (over: Partial<Schedule>) => {
    const template = (await mockApi.listSchedules()).find((s) => s.target === "sweep")!;
    return { ...template, model: null, override_subagent_model: false, ...over } as Schedule;
  };
  it("default-origin row: unpinned reads default, pinned is marked, + agents only when set", async () => {
    const view = row(await base({ origin: "default" }));
    expect(screen.getByText("model: default")).toBeTruthy();
    expect(screen.queryByText("+ agents")).toBeNull();
    view.unmount();
    row(await base({ origin: "default", model: "claude-opus-4-1", override_subagent_model: true }));
    expect(screen.getByText("model: claude-opus-4-1 · pinned")).toBeTruthy();
    expect(screen.getByText("+ agents")).toBeTruthy();
  });
  it("user-origin row shows the model chip with and without a pin, and + agents", async () => {
    const view = row(await base({ origin: "user" }));
    expect(screen.getByText("model: default")).toBeTruthy();
    expect(screen.queryByText("+ agents")).toBeNull();
    view.unmount();
    row(await base({ origin: "user", model: "claude-opus-4-1", override_subagent_model: true }));
    expect(screen.getByText("model: claude-opus-4-1 · pinned")).toBeTruthy();
    expect(screen.getByText("+ agents")).toBeTruthy();
  });
  it("a user row with no run flags still shows defaults beside the model chip", async () => {
    const s = await base({
      origin: "user", target: "prompt", model: "claude-opus-4-1", wait_on_limit: false,
      auto_approve: false, mr_rework_enabled: null,
    });
    row(s);
    expect(screen.getByText("defaults")).toBeTruthy();
    expect(screen.getByText("model: claude-opus-4-1 · pinned")).toBeTruthy();
  });
});

 it.each(["user", "default"] as const)("shows removes label only for enabled removal on %s rows", async (origin) => {
   const template = (await mockApi.listSchedules()).find((s) => s.target === "sweep")!;
   const view = row({ ...template, origin, remove_label_on_dispatch: true });
   expect(screen.getByText("removes label")).toBeTruthy();
   view.unmount();
   row({ ...template, origin, remove_label_on_dispatch: false });
   expect(screen.queryByText("removes label")).toBeNull();
 });

// Issue #3260: the unpinned model chip carries the server's prediction.
describe("unpinned model chip prediction", () => {
  const fixture = (scenario: string, name: string): ScheduleSettingsPreview =>
    structuredClone(
      RECORDED_EFFECTIVE_SCENARIOS.find((x) => x.name === scenario)!.schedule_previews.find((p) => p.name === name)!
        .response,
    );
  const unpinned = async (over: Partial<Schedule> = {}) => {
    const template = (await mockApi.listSchedules()).find((s) => s.target === "sweep")!;
    return { ...template, origin: "user", model: null, override_subagent_model: false, ...over } as Schedule;
  };

  it("appends the decided harness prediction and requests it for this schedule", async () => {
    previewMock.mockResolvedValue(fixture("default", "prompt with an explicit inherit"));
    const s = await unpinned();
    row(s);
    expect(await screen.findByText("model: default · now opus (Claude, predicted)")).toBeTruthy();
    expect(previewMock).toHaveBeenCalledTimes(1);
    expect(previewMock.mock.calls[0]?.[0]).toEqual({ schedule_id: s.id, target: s.target });
  });

  it("appends the chosen-at-create suffix", async () => {
    const p = fixture("default", "unpinned sweep");
    previewMock.mockResolvedValue(p);
    row(await unpinned());
    const [a, b] = p.alternatives!;
    expect(
      await screen.findByText(
        `model: default · now ${a.worker_model!.resolved_model} (Claude) or ${b.worker_model!.resolved_model} (Codex), predicted`,
      ),
    ).toBeTruthy();
  });

  it("stays the plain chip when the preview fails", async () => {
    previewMock.mockRejectedValue(new Error("boom"));
    row(await unpinned());
    await waitFor(() => expect(previewMock).toHaveBeenCalledTimes(1));
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.getByText("model: default")).toBeTruthy();
    expect(screen.queryByText(/now /)).toBeNull();
  });

  it("a pinned chip is unchanged and sends no request", async () => {
    row(await unpinned({ model: "claude-opus-4-1" }));
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.getByText("model: claude-opus-4-1 · pinned")).toBeTruthy();
    expect(previewMock).not.toHaveBeenCalled();
  });

  it("a changed harness refetches", async () => {
    previewMock.mockResolvedValue(fixture("default", "prompt with an explicit inherit"));
    const s = await unpinned({ harness: null });
    const view = row(s);
    await waitFor(() => expect(previewMock).toHaveBeenCalledTimes(1));
    view.rerender(tree({ ...s, harness: "codex" }));
    await waitFor(() => expect(previewMock).toHaveBeenCalledTimes(2));
  });
});
