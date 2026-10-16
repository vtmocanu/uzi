// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { PlanPanel } from "./PlanPanel";
import { api, type Run } from "../../lib/api";
import { mockRuns } from "../../mocks/data/runs";

vi.mock("../../lib/api", async original => {
  const actual = await original<typeof import("../../lib/api")>();
  return { ...actual, api: { ...actual.api, listSecrets: vi.fn().mockResolvedValue({ secrets: [] }),
    setRunCredential: vi.fn().mockResolvedValue({}) } };
});
beforeEach(() => vi.clearAllMocks());
afterEach(cleanup);

function run(over: Partial<Run> = {}): Run {
  return { ...mockRuns[0], id: "reset-run", status: "awaiting_approval", harness: "claude",
    plan_md: "A replacement plan", gate_revision: 4, completion_revision: 2, reset_state_digest: "digest-a",
    milestones: [{ id: "m1", title: "Old title" }], milestones_candidate: [{ id: "m1", title: "New title" }],
    milestones_completed: ["m1"], milestones_in_progress: [], milestones_agents: [],
    scope_ceiling: 1, stop_kind: null, stop_reason: null, completion_accepted: [], completion_deferred: [],
    own_agents: [], repo_agents: [], ...over };
}
function mount(initial = run(), canSteer = true) {
  const onApprove = vi.fn();
  const onResetApprove = vi.fn().mockResolvedValue(true);
  const props = { messages: [], busy: false, canSteer, onApprove, onResetApprove, onReject: vi.fn() };
  const rendered = render(<PlanPanel run={initial} {...props} />);
  return { ...rendered, onApprove, onResetApprove,
    update: (next: Run) => rendered.rerender(<PlanPanel run={next} {...props} />) };
}
function open() { fireEvent.click(screen.getByRole("button", { name: /^Approve plan/ })); }
function confirmation() { return within(screen.getByRole("region", { name: "Confirm milestone reset" })); }
function fill() {
  fireEvent.change(confirmation().getByLabelText("Reason for reset"), { target: { value: "New scope" } });
  fireEvent.click(confirmation().getByRole("checkbox"));
}
function confirm() { fireEvent.click(confirmation().getByRole("button", { name: "Reset progress and approve plan" })); }

describe("owner milestone reset confirmation", () => {
  it("requires a reason and acknowledgement, clears cancellation drafts, never writes credentials", () => {
    const view = mount();
    open();
    const button = confirmation().getByRole("button", { name: "Reset progress and approve plan" });
    expect((button as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(confirmation().getByLabelText("Reason for reset"), { target: { value: "   " } });
    fireEvent.click(confirmation().getByRole("checkbox"));
    expect((button as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(confirmation().getByRole("checkbox"));
    fireEvent.change(confirmation().getByLabelText("Reason for reset"), { target: { value: "why" } });
    expect((button as HTMLButtonElement).disabled).toBe(true);
    fill();
    fireEvent.click(confirmation().getByRole("button", { name: "Cancel reset" }));
    open();
    expect((confirmation().getByLabelText("Reason for reset") as HTMLTextAreaElement).value).toBe("");
    expect((confirmation().getByRole("checkbox") as HTMLInputElement).checked).toBe(false);
    expect(view.onResetApprove).not.toHaveBeenCalled();
    expect(view.onApprove).not.toHaveBeenCalled();
    expect(api.setRunCredential).not.toHaveBeenCalled();
    expect((screen.getByRole("combobox", { name: /Anthropic token/ }) as HTMLSelectElement).disabled).toBe(true);
  });

  it("shows old and proposed titles for a reused ID and resets with legacy null", async () => {
    const view = mount(run({ completion_revision: null }));
    open();
    expect(confirmation().getByText(/Frozen milestone IDs/).textContent).toContain("Old title");
    expect(confirmation().getByText(/Proposed milestone IDs/).textContent).toContain("New title");
    expect(confirmation().getByText(/Reused milestone IDs/)).toBeTruthy();
    fill(); confirm();
    await act(async () => {});
    expect(view.onResetApprove).toHaveBeenCalledWith({ source: "own", exclusions: [] }, false, 4, {
      reconciliation: { mode: "reset", reason: "New scope", expected_completion_revision: null },
      expected_reset_state_digest: "digest-a",
    });
    expect(view.onApprove).not.toHaveBeenCalled();
    expect(api.setRunCredential).not.toHaveBeenCalled();
  });

  it("compares JSON objects independently of key order and preserves ordinary argument count", () => {
    const view = mount(run({ gate_revision: undefined,
      milestones_candidate: [{ title: "Old title", id: "m1" }] }));
    open();
    expect(screen.queryByRole("region", { name: "Confirm milestone reset" })).toBeNull();
    expect(view.onApprove).toHaveBeenCalledWith({ source: "own", exclusions: [] });
    expect(view.onApprove.mock.calls[0]).toHaveLength(1);
  });

  it("treats an explicit empty replacement as changed", () => {
    const view = mount(run({ milestones_candidate: [] }));
    open();
    expect(confirmation().getByRole("heading", { name: "Reset progress and approve plan" })).toBeTruthy();
    expect(confirmation().getByText(/Explicit empty replacement/)).toBeTruthy();
    expect(view.onApprove).not.toHaveBeenCalled();
  });

  it("compares frozen empty arrays and ordered definition lists", () => {
    const view = mount(run({ milestones: [], milestones_candidate: null }));
    open();
    expect(screen.getByText(/Reset metadata is unavailable/)).toBeTruthy();
    expect(view.onApprove).not.toHaveBeenCalled();
    view.update(run({ milestones: [{ id: "a", title: "A" }, { id: "b", title: "B" }],
      milestones_candidate: [{ id: "b", title: "B" }, { id: "a", title: "A" }] }));
    expect(screen.queryByRole("region", { name: "Confirm milestone reset" })).toBeNull();
    open(); expect(confirmation()).toBeTruthy();
  });

  it.each([
    { reset_state_digest: undefined }, { reset_state_digest: "" },
    { gate_revision: undefined }, { completion_revision: undefined },
    { milestones_candidate: undefined }, { milestones_candidate: null },
  ])("fails closed with missing metadata %j", metadata => {
    const view = mount(run(metadata));
    open();
    expect(screen.getByText(/Reset metadata is unavailable/)).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Confirm milestone reset" })).toBeNull();
    expect(view.onApprove).not.toHaveBeenCalled();
    expect(view.onResetApprove).not.toHaveBeenCalled();
    expect(api.setRunCredential).not.toHaveBeenCalled();
  });

  it.each([
    { reset_state_digest: "digest-b" }, { gate_revision: 5 }, { completion_revision: 3 },
    { status: "running" }, { id: "other-run" }, { plan_md: "Another plan" },
    { milestones_completed: [] }, { milestones_in_progress: ["m1"] }, { scope_ceiling: 2 },
    { stop_reason: "new stop" }, { completion_accepted: [{ id: "criterion" }] },
    { milestones_candidate: [{ id: "m1", title: "Changed again" }] },
  ] as Partial<Run>[])("invalidates the entire snapshot for %j", change => {
    const view = mount();
    open(); fill();
    view.update(run(change));
    expect(screen.queryByRole("region", { name: "Confirm milestone reset" })).toBeNull();
    expect(screen.getByText(/displayed plan or reset effects changed/)).toBeTruthy();
    expect(view.onResetApprove).not.toHaveBeenCalled();
    expect(view.onApprove).not.toHaveBeenCalled();
  });

  it("hides owner actions for a non-owner", () => {
    const view = mount(run(), false);
    expect(screen.queryByRole("button", { name: /Approve plan/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Run without/ })).toBeNull();
    expect(view.onResetApprove).not.toHaveBeenCalled();
  });

  it("capability override opens its own reset review and refuses without keeping the draft", async () => {
    const view = mount(run({ required_capabilities: ["docker"] }));
    view.onResetApprove.mockResolvedValue(false);
    fireEvent.click(screen.getByRole("button", { name: /Run without docker/ }));
    expect(confirmation().getByText(/overrides missing capabilities/)).toBeTruthy();
    fill(); confirm();
    await act(async () => {});
    expect(view.onResetApprove.mock.calls[0][1]).toBe(true);
    expect(screen.queryByRole("region", { name: "Confirm milestone reset" })).toBeNull();
    expect(screen.getByText(/Reset was refused/)).toBeTruthy();
    open();
    expect((confirmation().getByLabelText("Reason for reset") as HTMLTextAreaElement).value).toBe("");
    expect(api.setRunCredential).not.toHaveBeenCalled();
  });
});


it("an ordinary credential operation cannot authorize a reset after a stream update", async () => {
  let finish!: () => void;
  vi.mocked(api.setRunCredential).mockImplementation(() => new Promise(resolve => {
    finish = () => resolve({ run: run() });
  }));
  const ordinary = run({ milestones_candidate: [{ id: "m1", title: "Old title" }] });
  const view = mount(ordinary);
  fireEvent.change(screen.getByRole("combobox", { name: /Anthropic token/ }), { target: { value: "mode:auto" } });
  open();
  expect(api.setRunCredential).toHaveBeenCalledTimes(1);
  view.update(run());
  await act(async () => finish());
  expect(view.onApprove).not.toHaveBeenCalled();
  expect(view.onResetApprove).not.toHaveBeenCalled();
  expect(screen.getByText(/displayed plan changed/)).toBeTruthy();
  open(); fill(); confirm();
  await act(async () => {});
  expect(view.onResetApprove).toHaveBeenCalledTimes(1);
  expect(api.setRunCredential).toHaveBeenCalledTimes(1);
});

it("keeps ordinary credential approval when only reset metadata refreshes", async () => {
  let finish!: () => void;
  vi.mocked(api.setRunCredential).mockImplementation(() => new Promise(resolve => {
    finish = () => resolve({ run: run() });
  }));
  const ordinary = run({ milestones_candidate: [{ id: "m1", title: "Old title" }] });
  const view = mount(ordinary);
  fireEvent.change(screen.getByRole("combobox", { name: /Anthropic token/ }), { target: { value: "mode:auto" } });
  open();
  view.update({ ...ordinary, reset_state_digest: "digest-after-credential-change" });
  await act(async () => finish());
  expect(view.onApprove).toHaveBeenCalledWith({ source: "own", exclusions: [] }, undefined, 4);
  expect(view.onResetApprove).not.toHaveBeenCalled();
});

it("guards duplicate submits while awaiting a refused reset and clears the reason", async () => {
  let finish!: (accepted: boolean) => void;
  const view = mount();
  view.onResetApprove.mockImplementation(() => new Promise(resolve => { finish = resolve; }));
  open(); fill();
  const button = confirmation().getByRole("button", { name: "Reset progress and approve plan" });
  fireEvent.click(button); fireEvent.click(button);
  expect(view.onResetApprove).toHaveBeenCalledTimes(1);
  view.update(run({ reset_state_digest: "digest-b" }));
  await act(async () => finish(false));
  expect(screen.queryByRole("region", { name: "Confirm milestone reset" })).toBeNull();
  open();
  expect((confirmation().getByLabelText("Reason for reset") as HTMLTextAreaElement).value).toBe("");
  expect((confirmation().getByRole("checkbox") as HTMLInputElement).checked).toBe(false);
  expect(view.onResetApprove).toHaveBeenCalledTimes(1);
});
