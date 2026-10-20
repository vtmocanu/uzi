// @vitest-environment jsdom
//
// CIWatchDetail (PRD #2865): a ci_watch schedule's detail panel shows the approval
// note, the watched workflows (or the all-workflows wording), the first episode page from
// the detail GET, and follows next_cursor on "Load more" until it is null. Forge text
// (workflow keys, failure signatures) renders as plain text.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { CIWatchDetail } from "./CIWatchDetail";
import { ScheduleListRow } from "./ScheduleListRow";
import { api, type CIEpisodeDTO, type Schedule } from "../lib/api";
import { mockApi as realMockApi } from "../mocks/mockApi";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { getSchedule: vi.fn(), listScheduleCIEpisodes: vi.fn() } };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ uziLabel: "uzi" }) }));
const mockApi = vi.mocked(api);

const NOTE =
  "Off by default: this job reads CI logs, which can contain text from dependencies, tests or outside contributors. Review the plan before the run changes code. You can turn it on.";

// Defaults to the reset shape (handled == observed, no attempts, no signature).
const episode = (workflow_key: string, over: Partial<CIEpisodeDTO> = {}): CIEpisodeDTO => ({
  workflow_key,
  last_handled_run_id: 1,
  observed_created_at: "2026-10-01T09:50:00Z",
  observed_updated_at: "2026-10-01T09:58:00Z",
  observed_run_id: 1,
  last_signature: null,
  attempts: 0,
  halted_reason: null,
  updated_at: "2026-10-01T10:00:00Z",
  ...over,
});

let base: Schedule;
beforeEach(async () => {
  // The seeded default watches every workflow; give it a selector so the chips render.
  base = { ...(await realMockApi.getSchedule("sch-def-ci-uzi")), ci_workflows: ["ci.yml", "kind-smoke.yml"] };
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderDetail(s: Schedule) {
  return render(<CIWatchDetail s={s} panelId="ci-watch-test" />);
}

const rowKeys = () =>
  within(screen.getByRole("table"))
    .getAllByRole("row")
    .slice(1)
    .map((r) => r.querySelector("td .font-mono")?.textContent);

const rowStates = () =>
  within(screen.getByRole("table"))
    .getAllByRole("row")
    .slice(1)
    .map((r) => r.querySelectorAll("td")[1]?.querySelector("span")?.textContent?.trim());

// The three row shapes the server commits (api/internal/store/queries/ci_watch.sql): a
// red observation creates the row inside Start or a halt; a fresh green Resets it.
const started = episode("start.yml", {
  observed_run_id: 41, last_handled_run_id: 41, attempts: 1, last_signature: "test: TestX failed",
});
const reset = episode("reset.yml", {
  observed_run_id: 52, last_handled_run_id: 52, attempts: 0, last_signature: null,
});
const haltedCap = episode("cap.yml", {
  observed_run_id: 63, last_handled_run_id: 63, attempts: 5, last_signature: "publish failed", halted_reason: "attempt_cap",
});
const haltedStuck = episode("stuck.yml", {
  observed_run_id: 74, last_handled_run_id: 74, attempts: 2, last_signature: "lint failed", halted_reason: "no_progress",
});

describe("CIWatchDetail", () => {
  it("labels each server-written episode shape", async () => {
    mockApi.getSchedule.mockResolvedValue({
      ...base,
      ci_episodes: { episodes: [started, reset, haltedCap, haltedStuck], next_cursor: null },
    });
    renderDetail(base);
    await waitFor(() => expect(rowKeys()).toEqual(["start.yml", "reset.yml", "cap.yml", "stuck.yml"]));
    expect(rowStates()).toEqual([
      "Repair attempted",
      "Passing again",
      "Stopped: attempt limit reached",
      "Stopped: no progress",
    ]);
  });

  it("counts repeated repair attempts and falls back to a neutral label on an unknown shape", async () => {
    mockApi.getSchedule.mockResolvedValue({
      ...base,
      ci_episodes: {
        episodes: [
          { ...started, workflow_key: "again.yml", attempts: 3 },
          // Not a shape the server writes (no attempts, but a signature): stay neutral.
          episode("odd.yml", { observed_run_id: 9, last_handled_run_id: 9, last_signature: "x" }),
        ],
        next_cursor: null,
      },
    });
    renderDetail(base);
    await waitFor(() => expect(rowKeys()).toEqual(["again.yml", "odd.yml"]));
    expect(rowStates()).toEqual(["Repair attempted (3 times)", "Failure seen"]);
  });

  it("shows the target, the approval note and the watched workflows", async () => {
    mockApi.getSchedule.mockResolvedValue({ ...base, ci_episodes: { episodes: [], next_cursor: null } });
    renderDetail(base);
    expect(screen.getByRole("heading", { name: "Default-branch CI repair" })).toBeTruthy();
    expect(screen.getByText(NOTE)).toBeTruthy();
    const list = screen.getByRole("list", { name: "Watched workflows" });
    expect(within(list).getAllByRole("listitem").map((li) => li.textContent)).toEqual(["ci.yml", "kind-smoke.yml"]);
    expect(
      await screen.findByText(
        "No failed run seen yet. A workflow appears here the first time it fails on the watched branch.",
      ),
    ).toBeTruthy();
  });

  it("says all workflows in the listed window when no selector is set", async () => {
    const s = { ...base, ci_workflows: undefined };
    mockApi.getSchedule.mockResolvedValue({ ...s, ci_episodes: { episodes: [], next_cursor: null } });
    renderDetail(s);
    expect(screen.getByText("All workflows in the listed window (newest 100 executions; 50 on Forgejo)")).toBeTruthy();
    expect(screen.queryByRole("list", { name: "Watched workflows" })).toBeNull();
  });

  it("loads more with the cursor, appends, and hides the control once exhausted", async () => {
    mockApi.getSchedule.mockResolvedValue({
      ...base,
      ci_episodes: { episodes: [episode("a.yml"), episode("b.yml")], next_cursor: "cursor-1" },
    });
    mockApi.listScheduleCIEpisodes
      .mockResolvedValueOnce({ episodes: [episode("c.yml")], next_cursor: "cursor-2" })
      .mockResolvedValueOnce({ episodes: [episode("d.yml")], next_cursor: null });
    renderDetail(base);

    await waitFor(() => expect(rowKeys()).toEqual(["a.yml", "b.yml"]));
    expect(mockApi.getSchedule).toHaveBeenCalledWith(base.id);

    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    await waitFor(() => expect(rowKeys()).toEqual(["a.yml", "b.yml", "c.yml"]));
    expect(mockApi.listScheduleCIEpisodes).toHaveBeenNthCalledWith(1, base.id, "cursor-1");
    expect(screen.getByRole("button", { name: "Load more" })).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    await waitFor(() => expect(rowKeys()).toEqual(["a.yml", "b.yml", "c.yml", "d.yml"]));
    expect(mockApi.listScheduleCIEpisodes).toHaveBeenNthCalledWith(2, base.id, "cursor-2");
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
  });

  it("walks the mock's two seeded pages end to end", async () => {
    mockApi.getSchedule.mockImplementation((id) => realMockApi.getSchedule(id));
    mockApi.listScheduleCIEpisodes.mockImplementation((id, c) => realMockApi.listScheduleCIEpisodes(id, c));
    renderDetail(base);
    await waitFor(() => expect(rowKeys()).toEqual(["ci.yml", "codeql.yml", "docs.yml"]));
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    await waitFor(() =>
      expect(rowKeys()).toEqual(["ci.yml", "codeql.yml", "docs.yml", "kind-smoke.yml", "release.yml"]),
    );
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
    expect(rowStates()).toEqual([
      "Repair attempted",
      "Repair attempted (2 times)",
      "Stopped: no progress",
      "Passing again",
      "Stopped: attempt limit reached",
    ]);
  });

  it("keeps the loaded rows and offers the control again when a page fails", async () => {
    mockApi.getSchedule.mockResolvedValue({
      ...base,
      ci_episodes: { episodes: [episode("a.yml")], next_cursor: "cursor-1" },
    });
    mockApi.listScheduleCIEpisodes.mockRejectedValueOnce(new Error("boom"));
    renderDetail(base);
    await waitFor(() => expect(rowKeys()).toEqual(["a.yml"]));
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(rowKeys()).toEqual(["a.yml"]);
    expect(screen.getByRole("button", { name: "Load more" })).toBeTruthy();
  });

  it("renders forge-supplied workflow keys and signatures as plain text", async () => {
    const hostile = '<img src=x onerror="alert(1)">';
    const s = { ...base, ci_workflows: [hostile] };
    mockApi.getSchedule.mockResolvedValue({
      ...s,
      ci_episodes: {
        episodes: [episode(hostile, { last_signature: "<b>boom</b>", attempts: 1 })],
        next_cursor: null,
      },
    });
    const { container } = renderDetail(s);
    await waitFor(() => expect(rowKeys()).toEqual([hostile]));
    expect(within(screen.getByRole("list", { name: "Watched workflows" })).getByText(hostile)).toBeTruthy();
    expect(screen.getByText("<b>boom</b>")).toBeTruthy();
    // A positive control for the query: the panel does render real elements.
    expect(container.querySelectorAll("td").length).toBeGreaterThan(0);
    expect(container.querySelector("img, b")).toBeNull();
  });
});

describe("ScheduleListRow — ci_watch", () => {
  function row(s: Schedule) {
    return render(
      <MemoryRouter>
        <table>
          <tbody>
            <ScheduleListRow s={s} name="Main CI" busy={false} addBusy={false} pauseNote={null} repos={[]}
              onToggle={vi.fn()} onRunNow={vi.fn()} onEdit={vi.fn()} onReset={vi.fn()} onClone={vi.fn()}
              onRemove={vi.fn()} onAddRepo={vi.fn()} />
          </tbody>
        </table>
      </MemoryRouter>,
    );
  }

  it("labels the row CI repair and opens the detail panel from its disclosure", async () => {
    mockApi.getSchedule.mockResolvedValue({ ...base, ci_episodes: { episodes: [], next_cursor: null } });
    row(base);
    // The kind pill names the target, apart from the row's own name.
    expect(screen.getByText("CI repair")).toBeTruthy();
    const toggle = screen.getByRole("button", { name: "CI state: Main CI on vtmocanu/uzi" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByText(NOTE)).toBeNull();
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    const panel = document.getElementById(toggle.getAttribute("aria-controls")!)!;
    expect(within(panel).getByText(NOTE)).toBeTruthy();
    await waitFor(() => expect(mockApi.getSchedule).toHaveBeenCalledWith("sch-def-ci-uzi"));
  });

  it("describes artifact and workflow reset on the CI row", () => {
    row({ ...base, customized: true });
    const reset = screen.getByRole("button", { name: /Reset Main CI/ });
    expect(reset.title).toContain("clears artifact selectors (fetch none)");
    expect(reset.title).toContain("workflow selectors (watch all)");
  });

  it("offers no CI disclosure on a non-ci_watch row (control)", async () => {
    const sweep = (await realMockApi.listSchedules()).find((s) => s.target === "sweep")!;
    row(sweep);
    expect(screen.getAllByRole("button").length).toBeGreaterThan(0);
    expect(screen.queryByRole("button", { name: /CI state/ })).toBeNull();
  });
});


it("shows explicit no artifacts and sanitizes selector display without mutating identity", async () => {
  mockApi.getSchedule.mockResolvedValue(base);
  const view = renderDetail(base);
  expect(screen.getByText("None — no artifacts fetched")).toBeTruthy();
  const raw = "logs-\u0000\u202e" + "x".repeat(200);
  const s = { ...base, ci_artifacts: [raw] };
  view.rerender(<CIWatchDetail s={s} panelId="ci-watch-test" />);
  const list = screen.getByRole("list", { name: "Artifact selectors" });
  const item = within(list).getByRole("listitem");
  expect(item.textContent).toBe("logs-" + "x".repeat(200));
  expect(item.className).toContain("break-all");
  for (const el of [item, ...item.querySelectorAll("*")]) {
    for (const attr of el.attributes) expect(attr.value).not.toMatch(/[\p{Cc}\p{Cf}]/u);
  }
  expect(s.ci_artifacts).toEqual([raw]);
  expect(mockApi.getSchedule).toHaveBeenCalledWith(base.id);
});
