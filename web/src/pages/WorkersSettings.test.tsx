// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { WorkersSettings } from "./WorkersSettings";
import { api, type Worker } from "../lib/api";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listWorkers: vi.fn(),
      createWorker: vi.fn(),
      deleteWorker: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.useRealTimers();
});

function aWorker(over: Partial<Worker> = {}): Worker {
  return {
    id: "w1",
    name: "laptop",
    status: "online",
    busy: false,
    active_runs: 0,
    max_concurrent_runs: null,
    template_declared: null,
    template_reported: "base",
    version: "0.4.2",
    last_heartbeat_at: "2026-07-14T00:00:00Z",
    created_at: "2026-07-01T00:00:00Z",
    stats_cpu_pct: null,
    stats_mem_bytes: null,
    stats_mem_limit_bytes: null,
    stats_source: null,
    ...over,
  };
}

const fleet: Worker[] = [
  aWorker({ id: "w-lap", name: "laptop", stats_cpu_pct: 34, stats_mem_bytes: 2254857830, stats_mem_limit_bytes: 4294967296, stats_source: "cgroup" }),
  aWorker({ id: "w-off", name: "ci", status: "offline", stats_cpu_pct: 12, stats_mem_bytes: 1610612736, stats_mem_limit_bytes: 2147483648, stats_source: "cgroup" }),
  aWorker({ id: "w-nas", name: "nas", stats_cpu_pct: 8, stats_mem_bytes: 503316480, stats_mem_limit_bytes: null, stats_source: "process" }),
];

function renderPage() {
  return render(
    <MemoryRouter>
      <WorkersSettings />
    </MemoryRouter>,
  );
}

describe("WorkersSettings resource gauges (PRD #49)", () => {
  it("renders per-worker CPU + memory gauges, a no-limit absolute readout, and the process-source label", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: fleet });
    renderPage();

    // Limited worker → used/limit memory bar with a percentage.
    expect(await screen.findByText(/2\.1\/4 GiB · 52%/)).toBeTruthy();
    // Process-source, no limit → absolute usage, no percentage, labeled.
    expect(screen.getByText(/480 MiB/)).toBeTruthy();
    expect(screen.getByText(/no limit/)).toBeTruthy();
    expect(screen.getByText("worker process only")).toBeTruthy();
  });

  it("dims an offline worker's gauges (last-known, not live-looking)", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: fleet });
    renderPage();
    const offlineBlock = await screen.findByLabelText(/worker offline/i);
    expect(offlineBlock.className).toMatch(/opacity-50/);
  });

  it("renders no gauges for a worker that has not reported a sample yet", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker({ name: "fresh" })] });
    renderPage();
    expect(await screen.findByText("fresh")).toBeTruthy();
    // A worker with no sample keeps its plain row — no progressbars at all.
    expect(screen.queryAllByRole("progressbar")).toHaveLength(0);
  });

  it("re-polls the fleet every 10s while visible (live liveness)", async () => {
    vi.useFakeTimers();
    mockApi.listWorkers.mockResolvedValue({ workers: fleet });
    renderPage();
    await vi.advanceTimersByTimeAsync(0); // flush the initial load
    expect(mockApi.listWorkers).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(10000); // one poll interval
    expect(mockApi.listWorkers).toHaveBeenCalledTimes(2);
  });
});
