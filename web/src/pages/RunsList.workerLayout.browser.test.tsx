import "../index.css";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { page } from "vitest/browser";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RunsLayout, RunsList } from "./RunsList";
import { healthyFleetWorkers } from "../mocks/data/health";
import type { AdminWorker } from "../lib/api";

// Keep the real route, badges, resource readings and clocks; only network and auth
// are replaced so the admin fleet can render offline.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listRuns: vi.fn().mockResolvedValue({ runs: [] }),
      listSecrets: vi.fn().mockResolvedValue({ secrets: [] }),
      adminListRuns: vi.fn().mockResolvedValue({ runs: [] }),
      adminListWorkers: vi.fn().mockImplementation(async () => ({ workers: [worker()] })),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({
  useAuth: () => ({ user: { id: "admin-1", is_admin: true }, vaultUnlocked: true }),
}));

function worker(): AdminWorker {
  return {
    ...healthyFleetWorkers()[0],
    name: "w1",
    owner_email: "a@b.co",
    template_declared: null,
    template_reported: null,
    online_since: null,
    busy: true,
    active_runs: 1,
    max_concurrent_runs: 2,
    active_cross_checks: 0,
    max_cross_check_slots: null,
    draining_since: "2026-10-01T09:00:00Z",
    reported_runs: [{
      run_id: "00000000-0000-4000-8000-000000000001",
      phase: "running",
      claim_generation: 1,
      terminal_pending: true,
      terminal_pending_since: null,
    }],
    custody_decisions_needed: 1,
    stats_source: "cgroup",
    stats_cpu_pct: 1,
    // Small values keep each individual reading comfortably inside the mobile
    // content width, isolating the badge group's wrapping from oversized readings.
    stats_mem_bytes: 1,
    stats_mem_limit_bytes: 2,
    stats_disk_data_bytes: 1,
    stats_disk_data_total_bytes: 2,
    cleanup_pending: true,
    residue_quarantined_at: "2026-10-01T09:00:00Z",
    residue_quarantine_cause: "unattributed process",
  };
}

let previousTheme: string | null;

beforeEach(() => {
  previousTheme = document.documentElement.getAttribute("data-theme");
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  if (previousTheme === null) document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", previousTheme);
});

describe.each(["ember", "dawn"])("Runs worker badges at 320px in %s", (theme) => {
  it("keeps every badge and nested resource reading inside the worker content edge", async () => {
    await page.viewport(320, 844);
    document.documentElement.setAttribute("data-theme", theme);
    render(
      <MemoryRouter initialEntries={["/runs"]}>
        <main className="min-w-0 px-4 py-6 sm:px-6 lg:py-8">
          <Routes>
            <Route element={<RunsLayout />}>
              <Route path="/runs" element={<RunsList />} />
            </Route>
          </Routes>
        </main>
      </MemoryRouter>,
    );

    const name = await screen.findByText("w1");
    const row = name.closest("li");
    expect(row).not.toBeNull();
    if (!row) throw new Error("Worker name must render inside its real fleet li");
    const fleet = within(row);
    const labels = [
      "online",
      "cleanup pending",
      "quarantined",
      "draining",
      "1/2 runs",
      "1 pending outcome",
      "retaining work",
    ];
    const badges = labels.map((label) => fleet.getByText(label, { exact: true }));
    const resources = fleet.getByText("cleanup pending", { exact: true }).parentElement;
    expect(resources).not.toBeNull();
    if (!resources) throw new Error("Cleanup badge must render inside AdminWorkerResources");
    const readings = ["cpu 1%", "mem 1/2 B", "disk 1/2 B"];
    const resourceText = resources.textContent?.replace(/\s+/g, " ");
    for (const reading of readings) expect(resourceText).toContain(reading);
    await document.fonts.ready;

    const style = getComputedStyle(row);
    const bounds = row.getBoundingClientRect();
    const contentRight = bounds.right - parseFloat(style.paddingRight) - parseFloat(style.borderRightWidth);
    const contentLeft = bounds.left + parseFloat(style.paddingLeft) + parseFloat(style.borderLeftWidth);

    // Enumerate the actual text-bearing resource spans as well as the outer
    // container: nested readings can overflow even when their parent fits.
    // Dots/icons have no text; the quarantine's sr-only description is not a badge.
    const resourceElements = [resources, ...resources.querySelectorAll("span")]
      .filter((element) => !element.classList.contains("sr-only") && element.textContent?.trim());
    const measured = [...new Set([...badges, ...resourceElements])].map((element) => ({
      label: element.textContent?.replace(/\s+/g, " ").trim(),
      rect: element.getBoundingClientRect(),
    }));
    expect(measured.length).toBeGreaterThan(labels.length);
    for (const { label, rect } of measured) {
      expect.soft(rect.width, `${label}: visible width`).toBeGreaterThan(0);
      expect.soft(rect.height, `${label}: visible height`).toBeGreaterThan(0);
      expect.soft(rect.left, `${label}: content left edge`).toBeGreaterThanOrEqual(contentLeft - 1);
      expect.soft(rect.right, `${label}: content right edge`).toBeLessThanOrEqual(contentRight + 1);
    }
    expect.soft(document.documentElement.scrollWidth, "document stays inside the 320px viewport")
      .toBeLessThanOrEqual(window.innerWidth);
    expect(window.innerWidth).toBe(320);
  });
});
