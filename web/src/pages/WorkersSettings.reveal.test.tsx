// @vitest-environment jsdom
//
// Issue #4373: `/workers?tab=workers#worker-row-<encodeURIComponent(id)>` reveals the worker
// row on direct entry and on in-app navigation (even from the Add tab), once per
// location.key, and not again on poll re-renders.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, useNavigate } from "react-router-dom";
import { WorkersSettings } from "./WorkersSettings";
import { api, type Worker } from "../lib/api";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listWorkers: vi.fn(),
      version: vi.fn().mockResolvedValue({ version: "" }),
      listSecrets: vi.fn(),
      setWorkerBindMode: vi.fn(),
      createWorker: vi.fn(),
      deleteWorker: vi.fn(),
      hostedConfig: vi.fn(),
      provisionHostedWorker: vi.fn(),
      getRecoveryHolds: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ user: null, refresh: vi.fn() }) }));

const mockApi = vi.mocked(api);
const TRICKY_ID = "a b/c#d";

function aWorker(over: Partial<Worker> = {}): Worker {
  return {
    hibernation_state: "none",
    hibernation_reason: null,
    hibernated_at: null,
    wake_requested_at: null,
    wake_error: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    id: "w1",
    name: "laptop",
    status: "online",
    busy: false,
    kind: "external",
    hosted_size: null,
    active_runs: 0,
    max_concurrent_runs: null,
    active_cross_checks: 0,
    max_cross_check_slots: null,
    template_declared: null,
    template_reported: "base",
    version: "0.4.2",
    upgrade_status: "up_to_date",
    upgrade_detail: null,
    upgrade_target: "0.4.2",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: "2026-07-14T00:00:00Z",
    created_at: "2026-07-01T00:00:00Z",
    stats_cpu_pct: null,
    stats_mem_bytes: null,
    stats_mem_limit_bytes: null,
    stats_source: null,
    stats_disk_nix_bytes: null,
    stats_disk_nix_total_bytes: null,
    stats_disk_data_bytes: null,
    stats_disk_data_total_bytes: null,
    stats_disk_dind_bytes: null,
    stats_disk_dind_total_bytes: null,
    stats_disk_dind_inodes: null,
    stats_disk_dind_total_inodes: null,
    draining_since: null,
    ...over,
  };
}

const linkFor = (id: string) => `/workers?tab=workers#worker-row-${encodeURIComponent(id)}`;
const rowEl = (id: string) => document.getElementById(`worker-row-${id}`);

function Nav({ to }: { to: string }) {
  const navigate = useNavigate();
  return (
    <button type="button" onClick={() => navigate(to)}>
      go
    </button>
  );
}

function renderAt(entry: string, navTo?: string) {
  return render(
    <MemoryRouter initialEntries={[entry]}>
      {navTo && <Nav to={navTo} />}
      <WorkersSettings />
    </MemoryRouter>,
  );
}

let scrollSpy: ReturnType<typeof vi.fn>;

beforeEach(() => {
  scrollSpy = vi.fn();
  HTMLElement.prototype.scrollIntoView = scrollSpy as unknown as typeof HTMLElement.prototype.scrollIntoView;
  mockApi.hostedConfig.mockResolvedValue({ enabled: false, quota: 0, ephemeral_enabled: false });
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockApi.getRecoveryHolds.mockResolvedValue({
    aggregate: { open_holds: 0, admission_counted_holds: 0, custody_hold_limit: 8, decision_needed: 0, blocked_runs: 0 },
    holds: [],
  });
  mockApi.listWorkers.mockResolvedValue({
    workers: [aWorker({ id: "w1", name: "laptop" }), aWorker({ id: TRICKY_ID, name: "odd-box" })],
  });
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("WorkersSettings deep-link reveal (#4373)", () => {
  it("direct entry scrolls to, focuses and highlights the row after load", async () => {
    renderAt(linkFor("w1"));
    await waitFor(() => expect(document.activeElement).toBe(rowEl("w1")));
    expect(rowEl("w1")?.getAttribute("data-revealed")).toBe("true");
    expect(scrollSpy.mock.contexts).toContain(rowEl("w1"));
  });

  it("in-app navigation from the Add tab shows the workers panel and reveals the row", async () => {
    renderAt("/workers?tab=add", linkFor("w1"));
    await screen.findByRole("tab", { name: /Add a worker/ });
    await waitFor(() => expect(rowEl("w1")).not.toBeNull());
    expect(rowEl("w1")?.closest("[hidden]")).not.toBeNull();
    expect(document.activeElement).not.toBe(rowEl("w1"));
    fireEvent.click(screen.getByRole("button", { name: "go" }));
    await waitFor(() => expect(document.activeElement).toBe(rowEl("w1")));
    expect(rowEl("w1")?.closest("[hidden]")).toBeNull();
    expect(rowEl("w1")?.getAttribute("data-revealed")).toBe("true");
  });

  it("round-trips an id with reserved characters", async () => {
    renderAt(linkFor(TRICKY_ID));
    await waitFor(() => expect(document.activeElement).toBe(rowEl(TRICKY_ID)));
    expect(rowEl(TRICKY_ID)?.getAttribute("data-revealed")).toBe("true");
  });

  it("an unknown id is a no-op: no scroll and focus unchanged", async () => {
    const before = document.activeElement;
    renderAt(linkFor("nope"));
    await waitFor(() => expect(rowEl("w1")).not.toBeNull());
    await act(async () => {});
    expect(scrollSpy).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(before);
    expect(document.querySelector("[data-revealed]")).toBeNull();
  });

  it("a malformed escape is a no-op", async () => {
    const before = document.activeElement;
    renderAt("/workers?tab=workers#worker-row-%E0%A4%A");
    await waitFor(() => expect(rowEl("w1")).not.toBeNull());
    await act(async () => {});
    expect(scrollSpy).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(before);
  });

  it("blur clears the highlight", async () => {
    renderAt(linkFor("w1"));
    await waitFor(() => expect(rowEl("w1")?.getAttribute("data-revealed")).toBe("true"));
    act(() => rowEl("w1")?.blur());
    await waitFor(() => expect(rowEl("w1")?.hasAttribute("data-revealed")).toBe(false));
  });

  it("a poll re-render does not re-reveal", async () => {
    renderAt(linkFor("w1"));
    await waitFor(() => expect(document.activeElement).toBe(rowEl("w1")));
    expect(scrollSpy).toHaveBeenCalledTimes(1);
    act(() => rowEl("w1")?.blur());
    const polls = mockApi.listWorkers.mock.calls.length;
    mockApi.listWorkers.mockResolvedValue({
      workers: [aWorker({ id: "w1", name: "laptop", busy: true }), aWorker({ id: TRICKY_ID, name: "odd-box" })],
    });
    await act(async () => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await waitFor(() => expect(mockApi.listWorkers.mock.calls.length).toBeGreaterThan(polls));
    await act(async () => {});
    expect(scrollSpy).toHaveBeenCalledTimes(1);
    expect(document.activeElement).not.toBe(rowEl("w1"));
    expect(rowEl("w1")?.hasAttribute("data-revealed")).toBe(false);
  });
});
