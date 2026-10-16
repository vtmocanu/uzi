// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { api, type Run } from "../../lib/api";
import { mockRuns } from "../../mocks/data/runs";
import { MilestoneResetHistory } from "./MilestoneResetHistory";

vi.mock("../../lib/api", async original => {
  const actual = await original<typeof import("../../lib/api")>();
  return { ...actual, api: { ...actual.api, getRun: vi.fn() } };
});
afterEach(() => { cleanup(); vi.clearAllMocks(); });
function run(over: Partial<Run> = {}): Run {
  return { ...mockRuns[0], id: "r1", milestone_reconciliations: [{
    gate_revision: 12, created_at: "2026-10-15", reason: "New scope",
    before_state: { milestones_frozen: [{ id: "m1", title: "Original" }], milestones_completed: ["m1"], scope_ceiling: 1 },
    after_state: { milestones_frozen: [], milestones_completed: [], scope_ceiling: null },
  }], milestone_reconciliations_next_before_gate_revision: 12, ...over };
}
it("renders sanitized escaped audit text, malformed details and omission notices", () => {
  const unsafe = "safe\u202E\u0007[click](https://example.com)<img src=x>";
  const { container } = render(<MilestoneResetHistory run={run({ milestone_reconciliations: [
    { gate_revision: unsafe, created_at: unsafe, reason: unsafe, reason_truncated: true, details_omitted: true,
      before_state: { milestones_frozen: [{ id: unsafe, title: unsafe }], stop_reason: unsafe },
      after_state: "unexpected" },
    null as unknown as Record<string, unknown>, { reason: { ["key\u202E"]: unsafe }, before_state: [] },
  ] })} />);
  expect(container.textContent).not.toContain("\u202E");
  expect(container.textContent).not.toContain("\u0007");
  expect(container.textContent).toContain("safe[click](https://example.com)<img src=x>");
  expect(container.querySelector("a, img")).toBeNull();
  expect(screen.getByText("Reason truncated.")).toBeTruthy();
  expect(screen.getByText(/Some audit details were omitted/)).toBeTruthy();
  expect(screen.getAllByText(/details unavailable/).length).toBeGreaterThan(0);
});
it("keeps every record in a server page reachable, including explicit empty snapshots", () => {
  const records = Array.from({ length: 50 }, (_, i) => ({
    gate_revision: 100 - i, reason: "Reset " + i,
    before_state: {}, after_state: { milestones_frozen: [], milestones_completed: [] },
  }));
  render(<MilestoneResetHistory run={run({ milestone_reconciliations: records })} />);
  expect(screen.getByText("Reason: Reset 0")).toBeTruthy();
  expect(screen.getByText("Reason: Reset 49")).toBeTruthy();
  expect(screen.getAllByText("Milestones: []")).toHaveLength(50);
  expect(api.getRun).not.toHaveBeenCalled();
});

it("requests one older page explicitly with the cursor and replaces the bounded readout", async () => {
  vi.mocked(api.getRun).mockResolvedValue({ run: run({
    milestone_reconciliations: [{ reason: "Older scope", before_state: null, after_state: {} }],
    milestone_reconciliations_next_before_gate_revision: 8,
  }) });
  render(<MilestoneResetHistory run={run()} />);
  expect(api.getRun).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Load older" }));
  fireEvent.click(screen.getByRole("button", { name: "Load older" }));
  await waitFor(() => expect(screen.getByText("Reason: Older scope")).toBeTruthy());
  expect(api.getRun).toHaveBeenCalledExactlyOnceWith("r1", 12);
  expect(screen.queryByText("Reason: New scope")).toBeNull();
});
it("rejects late results from a previous run including a return visit", async () => {
  let resolve!: (value: { run: Run }) => void;
  vi.mocked(api.getRun).mockImplementation(() => new Promise(done => { resolve = done; }));
  const view = render(<MilestoneResetHistory run={run()} />);
  fireEvent.click(screen.getByRole("button", { name: "Load older" }));
  view.rerender(<MilestoneResetHistory run={run({ id: "r2" })} />);
  view.rerender(<MilestoneResetHistory run={run()} />);
  await act(async () => resolve({ run: run({ milestone_reconciliations: [{ reason: "Late old history" }] }) }));
  expect(screen.queryByText(/Late old history/)).toBeNull();
  expect(screen.getByText("Reason: New scope")).toBeTruthy();
});
it("keeps the current page and permits deliberate retry on malformed pagination or refusal", async () => {
  vi.mocked(api.getRun).mockResolvedValue({ run: run({ milestone_reconciliations_next_before_gate_revision: 15 }) });
  render(<MilestoneResetHistory run={run()} />);
  fireEvent.click(screen.getByRole("button", { name: "Load older" }));
  await screen.findByText("Could not load older reset history.");
  expect(screen.getByText("Reason: New scope")).toBeTruthy();
  expect(api.getRun).toHaveBeenCalledTimes(1);
});
