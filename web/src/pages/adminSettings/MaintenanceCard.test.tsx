// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { MaintenanceCard } from "./MaintenanceCard";
import { api, type AdminMaintenance, type MaintenanceRunItem } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { getAdminMaintenance: vi.fn(), setMaintenance: vi.fn() } };
});
const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
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

  it("shows the server error when a PUT fails and keeps the switch state", async () => {
    await renderCard(doc());
    mockApi.setMaintenance.mockRejectedValueOnce(new Error("forbidden"));
    fireEvent.click(screen.getByRole("switch", { name: "Maintenance mode" }));
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByRole("switch", { name: "Maintenance mode" }).getAttribute("aria-checked")).toBe("false");
  });
});
