// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { WorkersSettings } from "./WorkersSettings";
import { ApiError } from "../lib/apiError";
import { api, type Worker } from "../lib/api";
import { incidentFleetWorkers } from "../mocks/data/health";

vi.mock("../lib/api", async (original) => ({
  ...await original<typeof import("../lib/api")>(),
  api: Object.fromEntries(["listWorkers", "listSecrets", "hostedConfig", "getRecoveryHolds",
    "cordonWorker", "uncordonWorker", "deleteWorker", "setWorkerBindMode", "version"].map((key) => [key, vi.fn()])),
}));
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ user: null, refresh: vi.fn() }) }));
const mock = vi.mocked(api);
function worker(over: Partial<Worker> = {}): Worker {
  return { ...incidentFleetWorkers()[0], id: "target", name: "target", kind: "external",
    status: "offline", busy: false, ephemeral: false, draining_since: null,
    manual_cordoned_at: null, manual_cordoned_by: null, manual_cordoned_by_label: null,
    manual_cordon_reason: null, effective_cordoned: false, ...over };
}
const held = worker({ manual_cordoned_at: "2026-10-01T00:00:00Z",
  manual_cordoned_by: "first-id", manual_cordoned_by_label: "first actor",
  manual_cordon_reason: "first reason", effective_cordoned: true });
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: Error) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
function mount() { return render(<MemoryRouter><WorkersSettings /></MemoryRouter>); }
function row() { return screen.getByText("target", { selector: "span" }).closest("li")!; }
async function ready(list = [worker()]) {
  mock.listWorkers.mockResolvedValue({ workers: list });
  mount();
  await screen.findByRole("button", { name: /^(Cordon|Uncordon) target/ });
}
function cordon(reason?: string) {
  fireEvent.click(screen.getByRole("button", { name: "Cordon target" }));
  if (reason !== undefined) fireEvent.change(screen.getByLabelText("Reason (optional)"), { target: { value: reason } });
  fireEvent.click(within(row()).getByRole("button", { name: "Cordon" }));
}
function poll() { act(() => document.dispatchEvent(new Event("visibilitychange"))); }
beforeEach(() => {
  vi.resetAllMocks();
  mock.version.mockResolvedValue({ version: "", founded: "" });
  mock.listSecrets.mockResolvedValue({ secrets: [] });
  mock.hostedConfig.mockResolvedValue({ enabled: false, quota: 0, ephemeral_enabled: false });
  mock.getRecoveryHolds.mockResolvedValue({ aggregate: { open_holds: 0, admission_counted_holds: 0,
    custody_hold_limit: 8, decision_needed: 0, blocked_runs: 0 }, holds: [] });
  Object.defineProperty(document, "hidden", { configurable: true, value: false });
});
afterEach(() => { cleanup(); vi.useRealTimers(); });

describe("owner manual intake controls", () => {
  it("offers persistent external and hosted actions with hosting off, excluding an ephemeral peer and accepting old optional fields", async () => {
    await ready([worker({ manual_cordoned_at: undefined }), worker({ id: "host", name: "host", kind: "hosted" }),
      worker({ id: "temp", name: "temp", ephemeral: true })]);
    expect(screen.getByRole("button", { name: "Cordon target" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Cordon host" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Cordon temp" })).toBeNull();
  });
  it.each([" ".repeat(501), "😀".repeat(501)])("rejects over 500 raw code points before trimming without calling the API (%#)", async (reason) => {
    await ready(); cordon(reason);
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("500 Unicode");
    expect(document.activeElement).toBe(alert);
    expect(mock.cordonWorker).not.toHaveBeenCalled();
  });
  it.each(["😀".repeat(500), '  <script>x</script>\n\t\u202Eraw  ', undefined])("sends raw optional reasons unchanged (%#)", async (reason) => {
    mock.cordonWorker.mockResolvedValue({ worker: held });
    await ready(); cordon(reason);
    await screen.findByRole("button", { name: "Uncordon target" });
    expect(mock.cordonWorker).toHaveBeenCalledWith("target", reason);
  });
  it("focuses open/cancel/success and uses first canonical no-op attribution rather than draft", async () => {
    mock.cordonWorker.mockResolvedValue({ worker: held });
    await ready();
    const trigger = screen.getByRole("button", { name: "Cordon target" });
    fireEvent.click(trigger);
    expect(document.activeElement).toBe(screen.getByLabelText("Reason (optional)"));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Cordon target" }));
    cordon("second draft");
    const uncordon = await screen.findByRole("button", { name: "Uncordon target" });
    expect(document.activeElement).toBe(uncordon);
    expect(within(row()).getByText("first actor")).toBeTruthy();
    expect(within(row()).getByText("2026-10-01T00:00:00.000Z")).toBeTruthy();
    expect(row().textContent).toContain("first reason");
    expect(row().textContent).not.toContain("second draft");
  });
  it.each([false, true])("keeps confirmed state and focuses an accessible failure including persistent-worker 409 (held=%s)", async (isHeld) => {
    const message = "manual cordon is only supported for persistent workers";
    mock.cordonWorker.mockRejectedValue(new ApiError(409, message));
    mock.uncordonWorker.mockRejectedValue(new ApiError(409, message));
    await ready([isHeld ? held : worker()]);
    if (isHeld) fireEvent.click(screen.getByRole("button", { name: "Uncordon target" }));
    else cordon();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(message);
    expect(document.activeElement).toBe(alert);
    expect(screen.getByRole("button", { name: isHeld ? "Uncordon target" : "Cordon" })).toBeTruthy();
    if (isHeld) expect(row().textContent).toContain("first reason");
  });
  it("blocks duplicate manual events and conflicting bind/delete writes until the ACK", async () => {
    const pending = deferred<{ worker: Worker }>();
    mock.cordonWorker.mockReturnValue(pending.promise);
    mock.listSecrets.mockResolvedValue({ secrets: [
      { id: "a", kind: "anthropic_token", label: "a" }, { id: "b", kind: "anthropic_token", label: "b" },
    ] as never });
    await ready();
    cordon();
    const button = within(row()).getByRole("button", { name: "Cordon" });
    fireEvent.submit(button.closest("form")!); fireEvent.submit(button.closest("form")!);
    expect(mock.cordonWorker).toHaveBeenCalledTimes(1);
    expect(within(row()).getByRole("combobox").hasAttribute("disabled")).toBe(true);
    expect(within(row()).getByRole("button", { name: "Delete" }).hasAttribute("disabled")).toBe(true);
    fireEvent.change(within(row()).getByRole("combobox"), { target: { value: "b" } });
    expect(mock.setWorkerBindMode).not.toHaveBeenCalled();
    await act(async () => pending.resolve({ worker: held }));
  });
  it("blocks manual actions while a same-row rebind is pending and preserves the returned custody projection", async () => {
    const pending = deferred<{ worker: Worker }>();
    mock.setWorkerBindMode.mockReturnValue(pending.promise);
    mock.listSecrets.mockResolvedValue({ secrets: [
      { id: "a", kind: "anthropic_token", label: "a" }, { id: "b", kind: "anthropic_token", label: "b" },
    ] as never });
    await ready();
    fireEvent.change(within(row()).getByRole("combobox"), { target: { value: "b" } });
    expect(screen.getByRole("button", { name: "Cordon target" }).hasAttribute("disabled")).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Cordon target" }));
    expect(mock.cordonWorker).not.toHaveBeenCalled();
    await act(async () => pending.resolve({ worker: { ...held, custody_decisions_needed: undefined } }));
    await screen.findByRole("button", { name: "Uncordon target" });
    expect(row().textContent).toContain("first reason");
  });
  it("renders hostile canonical metadata as escaped sanitized cells and attributes", async () => {
    const hostile = '<img src=x>\n\t\u202Eactor';
    mock.cordonWorker.mockResolvedValue({ worker: { ...held, manual_cordoned_by_label: hostile, manual_cordon_reason: hostile } });
    await ready([worker({ name: "target" })]); cordon();
    await screen.findByRole("button", { name: "Uncordon target" });
    const actor = within(row()).getByText("<img src=x>actor", { exact: true });
    expect(actor.getAttribute("title")).toBe("<img src=x>actor");
    expect(within(row()).getByText("Reason: <img src=x>actor").getAttribute("title")).toBe("<img src=x>actor");
    expect(row().querySelector("img")).toBeNull();
  });
});

describe.each(["reload", "poll"] as const)("canonical mutation versus older %s lists", (writer) => {
  it.each([{ action: "cordon", uncordon: false }, { action: "uncordon", uncordon: true }])(
    "retains canonical manual attribution and automatic draining after $action", async ({ uncordon }) => {
    const before = uncordon ? held : worker();
    const other = worker({ id: "other", name: "other" });
    const stale = deferred<{ workers: Worker[] }>();
    const pending = deferred<{ worker: Worker }>();
    const canonical = { ...(uncordon ? worker() : { ...held, manual_cordon_reason: "canonical reason" }),
      draining_since: "2026-09-01T00:00:00Z", active_runs: 1, effective_cordoned: true };
    mock.cordonWorker.mockReturnValue(pending.promise);
    mock.uncordonWorker.mockReturnValue(pending.promise);
    mock.deleteWorker.mockResolvedValue({ ok: true } as never);
    await ready([before, other]);
    mock.listWorkers.mockReturnValue(stale.promise);
    if (writer === "reload") {
      fireEvent.click(within(screen.getByText("other", { selector: "span" }).closest("li")!).getByRole("button", { name: "Delete" }));
      await waitFor(() => expect(mock.listWorkers).toHaveBeenCalledTimes(2));
      expect(screen.getByText("Deleted other.")).toBeTruthy();
    }
    if (uncordon) fireEvent.click(screen.getByRole("button", { name: "Uncordon target" }));
    else cordon();
    // Poll begins DURING the mutation, so invalidation only at mutation start fails.
    if (writer === "poll") poll();
    await waitFor(() => expect(mock.listWorkers).toHaveBeenCalledTimes(2));
    await act(async () => pending.resolve({ worker: canonical }));
    await act(async () => stale.resolve({ workers: [before, other] }));
    expect(screen.getByRole("button", { name: uncordon ? "Cordon target" : "Uncordon target" })).toBeTruthy();
    expect(within(row()).getByText("draining")).toBeTruthy();
    if (!uncordon) expect(row().textContent).toContain("canonical reason");
    if (uncordon) {
      expect(row().textContent).not.toContain("first reason");
      expect(row().textContent).not.toContain("cordoned (manual)");
    } else {
      expect(row().textContent).toContain("first actor");
      expect(row().textContent).toContain("2026-10-01T00:00:00.000Z");
    }
    if (writer === "reload") expect(screen.queryByText("other", { selector: "span" })).toBeNull();
  });
});

describe("manual operation while an open form receives a real poll", () => {
  async function openAndPoll(reason?: string) {
    await ready();
    fireEvent.click(screen.getByRole("button", { name: "Cordon target" }));
    if (reason !== undefined) fireEvent.change(screen.getByLabelText("Reason (optional)"), { target: { value: reason } });
    mock.listWorkers.mockResolvedValue({ workers: [held] });
    poll();
    await within(row()).findByText("Reason: first reason");
    expect(mock.listWorkers).toHaveBeenCalledTimes(2);
    expect((screen.getByLabelText("Reason (optional)") as HTMLTextAreaElement).value).toBe(reason ?? "");
    return within(row()).getByRole("button", { name: "Cordon" });
  }

  it.each([undefined, "  still cordon  "])("keeps the visible Cordon operation after the poll sets a manual hold (%#)", async (reason) => {
    mock.cordonWorker.mockResolvedValue({ worker: held });
    mock.uncordonWorker.mockResolvedValue({ worker: worker() });
    const submit = await openAndPoll(reason);
    fireEvent.click(submit);
    expect(mock.cordonWorker).toHaveBeenCalledExactlyOnceWith("target", reason);
    expect(mock.uncordonWorker).not.toHaveBeenCalled();
    await screen.findByRole("button", { name: "Uncordon target" });
  });

  it.each([" ".repeat(501), "😀".repeat(501)])("validates the raw Cordon reason after the poll sets a manual hold (%#)", async (reason) => {
    const submit = await openAndPoll(reason);
    fireEvent.click(submit);
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("500 Unicode");
    expect(document.activeElement).toBe(alert);
    expect(mock.cordonWorker).not.toHaveBeenCalled();
    expect(mock.uncordonWorker).not.toHaveBeenCalled();
  });
});

describe("list success ordering and canonical fields", () => {
  it("accepts a valid older initial list when a newer poll fails", async () => {
    const initial = deferred<{ workers: Worker[] }>();
    mock.listWorkers.mockReturnValueOnce(initial.promise).mockRejectedValueOnce(new Error("poll failed"));
    mount(); poll();
    await act(async () => initial.resolve({ workers: [held] }));
    await screen.findByRole("button", { name: "Uncordon target" });
    expect(row().textContent).toContain("first reason");
  });
  it("keeps a newer successful poll when an older initial list later resolves", async () => {
    const initial = deferred<{ workers: Worker[] }>();
    mock.listWorkers.mockReturnValueOnce(initial.promise).mockResolvedValueOnce({ workers: [held] });
    mount(); poll();
    await act(async () => {});
    await act(async () => initial.resolve({ workers: [worker()] }));
    await screen.findByRole("button", { name: "Uncordon target" });
    expect(row().textContent).toContain("first reason");
  });
  it("retains the last-good list after a periodic poll failure", async () => {
    await ready([held]);
    mock.listWorkers.mockRejectedValue(new Error("poll failed"));
    poll(); await act(async () => {});
    expect(screen.getByRole("button", { name: "Uncordon target" })).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });
  it("honors effective intake and other fence fields from uncordon without deriving or clearing them", async () => {
    const canonical = worker({ effective_cordoned: true, memory_authority_lost_at: "2026-10-01T00:00:00Z",
      residue_quarantined_at: "2026-10-01T00:00:00Z", residue_quarantine_cause: "still fenced",
      custody_decisions_needed: undefined });
    mock.uncordonWorker.mockResolvedValue({ worker: canonical });
    await ready([{ ...held, custody_decisions_needed: 2 }]);
    fireEvent.click(screen.getByRole("button", { name: "Uncordon target" }));
    await screen.findByRole("button", { name: "Cordon target" });
    expect(mock.uncordonWorker).toHaveBeenCalledExactlyOnceWith("target");
    expect(mock.cordonWorker).not.toHaveBeenCalled();
    expect(within(row()).getByText("intake fenced")).toBeTruthy();
    expect(within(row()).getByText("memory authority lost")).toBeTruthy();
    expect(within(row()).getByText("quarantined")).toBeTruthy();
    expect(within(row()).getByText("retaining work")).toBeTruthy();
    expect(row().textContent).not.toContain("first reason");
  });
  it("sanitizes new accessible worker names while keeping the API coordinate raw", async () => {
    mock.cordonWorker.mockResolvedValue({ worker: held });
    await ready([worker({ name: "tar\u202Eget\n\t<img>" })]);
    const control = screen.getByRole("button", { name: "Cordon target<img>" });
    expect(control.getAttribute("aria-label")).toBe("Cordon target<img>");
    fireEvent.click(control);
    fireEvent.click(screen.getByRole("button", { name: "Cordon" }));
    await screen.findByRole("button", { name: "Uncordon target" });
    expect(mock.cordonWorker).toHaveBeenCalledWith("target", undefined);
  });
  it("does not publish a pending mutation after unmount", async () => {
    const pending = deferred<{ worker: Worker }>();
    mock.cordonWorker.mockReturnValue(pending.promise);
    await ready(); cordon();
    cleanup();
    await act(async () => pending.resolve({ worker: held }));
    expect(screen.queryByRole("button", { name: "Uncordon target" })).toBeNull();
  });
});

describe("persistent owner actions across fences", () => {
  it("keeps controls available while busy, offline, held and automatically cordoned irrespective of quota", async () => {
    mock.hostedConfig.mockResolvedValue({ enabled: true, quota: 0, ephemeral_enabled: false });
    await ready([worker({ kind: "hosted", busy: true, active_runs: 1,
      draining_since: "2026-10-01T00:00:00Z", effective_cordoned: true,
      custody_decisions_needed: 2, memory_authority_lost_at: "2026-10-01T00:00:00Z" })]);
    expect(screen.getByRole("button", { name: "Cordon target" }).hasAttribute("disabled")).toBe(false);
  });
  it("keeps a canonically cleared effective flag clear rather than inheriting the old hold", async () => {
    mock.uncordonWorker.mockResolvedValue({ worker: worker({ effective_cordoned: false }) });
    await ready([held]);
    fireEvent.click(screen.getByRole("button", { name: "Uncordon target" }));
    await screen.findByRole("button", { name: "Cordon target" });
    expect(within(row()).queryByText("intake fenced")).toBeNull();
    expect(within(row()).queryByText("cordoned (manual)")).toBeNull();
  });
});
