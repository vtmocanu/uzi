// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { MaintenanceCard } from "./MaintenanceCard";
import { setDemoMode } from "../../lib/demoMode";
import { api, type AdminMaintenance, type MaintenanceRunItem } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { getAdminMaintenance: vi.fn(), setMaintenance: vi.fn() } };
});
const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
  setDemoMode(false);
  vi.resetAllMocks();
});

const EMPTY = { count: 0, items: [] };
const AS_OF = "2026-10-25T12:30:05Z";

function item(over: Partial<MaintenanceRunItem> & Pick<MaintenanceRunItem, "run_id" | "status">): MaintenanceRunItem {
  return { kind: "issue", owner_label: "Ada", repo: "acme/uzi", since: "2026-10-25T12:00:00Z", tag: null, hold_id: null, ...over };
}

function doc(over: { active?: boolean; reason?: string | null; clear?: boolean } = {}): AdminMaintenance {
  const clear = over.clear ?? true;
  return {
    state: {
      active: over.active ?? false,
      since: over.active ? "2026-10-25T12:00:00Z" : null,
      reason: over.reason ?? null,
      by_label: over.active ? "Ada" : null,
    },
    readout: clear
      ? { as_of: AS_OF, clear: true, active: EMPTY, held: EMPTY, custody: EMPTY, queued: EMPTY, idle_bound_workers: EMPTY }
      : {
          as_of: AS_OF,
          clear: false,
          active: { count: 1, items: [item({ run_id: "run-a", status: "running" })] },
          held: {
            count: 60,
            items: [
              item({ run_id: "run-h1", status: "awaiting_input", tag: "needs_owner" }),
              item({ run_id: "run-h2", status: "limit_wait", tag: "progressing" }),
            ],
          },
          custody: EMPTY,
          queued: { count: 1, items: [item({ run_id: "run-q", status: "queued" })] },
          idle_bound_workers: EMPTY,
        },
  };
}

async function renderCard(initial: AdminMaintenance) {
  mockApi.getAdminMaintenance.mockResolvedValue(initial);
  render(
    <MemoryRouter>
      <MaintenanceCard />
    </MemoryRouter>,
  );
  await screen.findByRole("switch", { name: "Maintenance mode" });
}

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

const reasonInput = () => screen.getByLabelText("Reason shown to every user") as HTMLInputElement;

describe("MaintenanceCard", () => {
  it("says Clear with the as-of time when the readout is clear, plus the refresh note", async () => {
    await renderCard(doc());
    const line = document.querySelector('[data-maintenance-clear="true"]')!;
    expect(line.textContent).toMatch(/^Clear: no running or held work \(as of .+\)$/);
    expect(document.querySelector('[data-maintenance-clear="false"]')).toBeNull();
    expect(
      screen.getByText("Refresh once more right before evicting nodes: a claim in flight at enable can still appear."),
    ).toBeTruthy();
    expect(reasonInput().maxLength).toBe(280);
  });

  it("says Not yet clear and lists blockers with links, tags and the cap", async () => {
    await renderCard(doc({ active: true, reason: "Node pool upgrade", clear: false }));
    const line = document.querySelector('[data-maintenance-clear="false"]')!;
    expect(line.textContent).toMatch(/^Not yet clear: 61 blockers left \(as of .+\)$/);
    expect(document.querySelector('[data-maintenance-clear="true"]')).toBeNull();

    const held = screen.getByRole("region", { name: "Held" });
    const row = held.querySelector('[data-maintenance-run="run-h1"]') as HTMLElement;
    expect(within(row).getByText("needs owner")).toBeTruthy();
    expect(within(row).getByRole("link", { name: "run-h1" }).getAttribute("href")).toBe("/runs/run-h1");
    expect(within(row).getByText("awaiting input")).toBeTruthy();
    expect(within(row).getByText("Ada")).toBeTruthy();
    expect(within(row).getByText("acme/uzi")).toBeTruthy();
    const other = held.querySelector('[data-maintenance-run="run-h2"]') as HTMLElement;
    expect(within(other).getByText("progressing")).toBeTruthy();
    expect(within(other).queryByText("needs owner")).toBeNull();
    expect(within(held).getByText("Showing 2 of 60.")).toBeTruthy();
    expect(reasonInput().value).toBe("Node pool upgrade");
  });

  it("trusts the server's clear verdict: not clear with every count at zero", async () => {
    // Every listed category is empty, yet the server says not clear (a claim landed between
    // its reads). The card follows readout.clear and never says "0 blockers left".
    const d = doc({ active: true });
    d.readout = { ...d.readout, clear: false };
    await renderCard(d);
    const line = document.querySelector('[data-maintenance-clear="false"]')!;
    expect(line.textContent).toMatch(/^Not yet clear \(as of .+\)$/);
    expect(line.textContent).not.toMatch(/blocker/);
    expect(document.querySelector('[data-maintenance-clear="true"]')).toBeNull();
  });

  it("masks the turned-on-by and owner labels in demo mode, email and name alike", async () => {
    setDemoMode(true);
    const d = doc({ active: true, clear: false });
    d.state.by_label = "ada.lovelace@example.com";
    d.readout.active.items = [item({ run_id: "run-a", status: "running", owner_label: "Grace Hopper" })];
    await renderCard(d);
    expect(document.getElementById("maintenance-state")!.textContent).toContain("turned on by Ada");
    expect(document.body.textContent).not.toContain("lovelace");
    expect(document.body.textContent).not.toContain("example.com");
    const row = document.querySelector('[data-maintenance-run="run-a"]') as HTMLElement;
    expect(within(row).getByText("Grace")).toBeTruthy();
    expect(document.body.textContent).not.toContain("Hopper");
  });

  it("turning on PUTs active with the typed reason, and turning off PUTs inactive", async () => {
    await renderCard(doc());
    mockApi.setMaintenance.mockResolvedValueOnce(doc({ active: true, reason: "Drain for upgrade" }));
    fireEvent.change(reasonInput(), { target: { value: "  Drain for upgrade " } });
    fireEvent.click(screen.getByRole("switch", { name: "Maintenance mode" }));
    expect(await screen.findByText("Maintenance mode is on. New runs wait in the queue.")).toBeTruthy();
    expect(mockApi.setMaintenance).toHaveBeenLastCalledWith(true, "Drain for upgrade");
    expect(screen.getByRole("switch", { name: "Maintenance mode" }).getAttribute("aria-checked")).toBe("true");

    mockApi.setMaintenance.mockResolvedValueOnce(doc());
    fireEvent.click(screen.getByRole("switch", { name: "Maintenance mode" }));
    expect(await screen.findByText("Maintenance mode is off. Queued runs start as workers free up.")).toBeTruthy();
    expect(mockApi.setMaintenance).toHaveBeenLastCalledWith(false, "Drain for upgrade");
    expect(mockApi.setMaintenance).toHaveBeenCalledTimes(2);
  });

  it("saving a new reason while on PUTs active=true with the reason", async () => {
    await renderCard(doc({ active: true, reason: "Old" }));
    const save = screen.getByRole("button", { name: "Save reason" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    mockApi.setMaintenance.mockResolvedValueOnce(doc({ active: true, reason: "New reason" }));
    fireEvent.change(reasonInput(), { target: { value: "New reason" } });
    expect(save.disabled).toBe(false);
    fireEvent.click(save);
    expect(await screen.findByText("Reason saved.")).toBeTruthy();
    expect(mockApi.setMaintenance).toHaveBeenCalledWith(true, "New reason");
  });

  it("Refresh re-reads the readout", async () => {
    await renderCard(doc({ active: true, clear: false }));
    mockApi.getAdminMaintenance.mockResolvedValueOnce(doc({ active: true }));
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(document.querySelector('[data-maintenance-clear="true"]')).toBeTruthy());
    expect(mockApi.getAdminMaintenance).toHaveBeenCalledTimes(2);
  });

  it("drops a Refresh GET that resolves after a toggle-off PUT settled", async () => {
    await renderCard(doc({ active: true, clear: false }));
    const get = deferred<AdminMaintenance>();
    mockApi.getAdminMaintenance.mockReturnValueOnce(get.promise);
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));

    mockApi.setMaintenance.mockResolvedValueOnce(doc({ active: false }));
    fireEvent.click(screen.getByRole("switch", { name: "Maintenance mode" }));
    expect(await screen.findByText("Maintenance mode is off. Queued runs start as workers free up.")).toBeTruthy();

    // The stale read (taken before the PUT) now lands: it must not resurrect "on".
    await act(async () => get.resolve(doc({ active: true, clear: false })));
    await waitFor(() => expect((screen.getByRole("button", { name: "Refresh" }) as HTMLButtonElement).disabled).toBe(false));
    expect(screen.getByRole("switch", { name: "Maintenance mode" }).getAttribute("aria-checked")).toBe("false");
    expect(document.getElementById("maintenance-state")!.textContent).toMatch(/^Off/);
    expect(document.querySelector('[data-maintenance-clear="true"]')).toBeTruthy();
  });

  it("drops a GET that resolves while a PUT is still in flight, and keeps the PUT's result", async () => {
    await renderCard(doc({ active: false }));
    const get = deferred<AdminMaintenance>();
    const put = deferred<AdminMaintenance>();
    mockApi.getAdminMaintenance.mockReturnValueOnce(get.promise);
    mockApi.setMaintenance.mockReturnValueOnce(put.promise);
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    fireEvent.click(screen.getByRole("switch", { name: "Maintenance mode" }));

    await act(async () => get.resolve(doc({ active: false, clear: false })));
    expect(document.querySelector('[data-maintenance-clear="false"]')).toBeNull();

    await act(async () => put.resolve(doc({ active: true, reason: "Drain" })));
    expect(await screen.findByText("Maintenance mode is on. New runs wait in the queue.")).toBeTruthy();
    expect(screen.getByRole("switch", { name: "Maintenance mode" }).getAttribute("aria-checked")).toBe("true");
    expect(document.querySelector('[data-maintenance-clear="true"]')).toBeTruthy();
    expect(reasonInput().value).toBe("Drain");
  });

  it("drops the error of a Refresh GET that fails after a PUT overtook it", async () => {
    await renderCard(doc({ active: true, clear: false }));
    let reject!: (e: unknown) => void;
    mockApi.getAdminMaintenance.mockReturnValueOnce(new Promise<AdminMaintenance>((_, r) => (reject = r)));
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));

    mockApi.setMaintenance.mockResolvedValueOnce(doc({ active: false }));
    fireEvent.click(screen.getByRole("switch", { name: "Maintenance mode" }));
    expect(await screen.findByText("Maintenance mode is off. Queued runs start as workers free up.")).toBeTruthy();

    await act(async () => reject(new Error("bad gateway")));
    await waitFor(() => expect((screen.getByRole("button", { name: "Refresh" }) as HTMLButtonElement).disabled).toBe(false));
    expect(screen.queryByText(/Failed to refresh the readout|bad gateway/)).toBeNull();
    expect(screen.getByText("Maintenance mode is off. Queued runs start as workers free up.")).toBeTruthy();
    expect(screen.getByRole("switch", { name: "Maintenance mode" }).getAttribute("aria-checked")).toBe("false");
  });

  it("shows the error of a Refresh GET that fails with no write in between", async () => {
    await renderCard(doc({ active: true, clear: false }));
    mockApi.getAdminMaintenance.mockRejectedValueOnce(new Error("bad gateway"));
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    expect(await screen.findByText(/bad gateway|Failed to refresh the readout/)).toBeTruthy();
  });

  it("keeps reason text typed while a Refresh GET is in flight", async () => {
    await renderCard(doc({ active: true, reason: "Old" }));
    const get = deferred<AdminMaintenance>();
    mockApi.getAdminMaintenance.mockReturnValueOnce(get.promise);
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    fireEvent.change(reasonInput(), { target: { value: "Typed during refresh" } });

    await act(async () => get.resolve(doc({ active: true, reason: "Server reason" })));
    await waitFor(() => expect((screen.getByRole("button", { name: "Refresh" }) as HTMLButtonElement).disabled).toBe(false));
    expect(reasonInput().value).toBe("Typed during refresh");
    // The edit stays dirty against the newly read reason, so it can still be saved.
    expect((screen.getByRole("button", { name: "Save reason" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("an untouched reason field adopts the reason a Refresh reads", async () => {
    await renderCard(doc({ active: true, reason: "Old" }));
    mockApi.getAdminMaintenance.mockResolvedValueOnce(doc({ active: true, reason: "Changed elsewhere" }));
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(reasonInput().value).toBe("Changed elsewhere"));
  });

  it("disables Refresh while a write is busy, after the last Refresh settled", async () => {
    // No read can start during a write: the stale-read guard relies on this.
    await renderCard(doc({ active: true }));
    mockApi.getAdminMaintenance.mockResolvedValueOnce(doc({ active: true }));
    const refresh = () => screen.getByRole("button", { name: /Refresh/ }) as HTMLButtonElement;
    fireEvent.click(refresh());
    await waitFor(() => expect(refresh().textContent).toBe("Refresh"));
    expect(refresh().disabled).toBe(false);

    const put = deferred<AdminMaintenance>();
    mockApi.setMaintenance.mockReturnValueOnce(put.promise);
    fireEvent.click(screen.getByRole("switch", { name: "Maintenance mode" }));
    expect(refresh().textContent).toBe("Refresh");
    expect(refresh().disabled).toBe(true);

    await act(async () => put.resolve(doc()));
    await waitFor(() => expect(refresh().disabled).toBe(false));
  });

  it("a Refresh with no write in between updates the readout", async () => {
    await renderCard(doc({ active: true, clear: false }));
    const get = deferred<AdminMaintenance>();
    mockApi.getAdminMaintenance.mockReturnValueOnce(get.promise);
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await act(async () => get.resolve(doc({ active: false })));
    await waitFor(() => expect(document.querySelector('[data-maintenance-clear="true"]')).toBeTruthy());
    expect(screen.getByRole("switch", { name: "Maintenance mode" }).getAttribute("aria-checked")).toBe("false");
  });

  it("shows the server error when a PUT fails and keeps the switch state", async () => {
    await renderCard(doc());
    mockApi.setMaintenance.mockRejectedValueOnce(new Error("forbidden"));
    fireEvent.click(screen.getByRole("switch", { name: "Maintenance mode" }));
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByRole("switch", { name: "Maintenance mode" }).getAttribute("aria-checked")).toBe("false");
  });
});
