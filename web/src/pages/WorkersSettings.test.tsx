// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
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
      hostedConfig: vi.fn(),
      provisionHostedWorker: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

// Hosting off unless a test says otherwise: that is the default an instance ships
// with (PRD #58 Decision 12, compose is zero-diff), so the pre-#58 tests below render
// the page they always rendered.
beforeEach(() => {
  mockApi.hostedConfig.mockResolvedValue({ enabled: false, quota: 0 });
});

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
    kind: "external",
    hosted_size: null,
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

describe("WorkersSettings hosted workers (PRD #58 M5)", () => {
  const hosted = aWorker({ id: "w-h", name: "base (M)", kind: "hosted", hosted_size: "m" });

  it("marks a hosted row with its kind and size, and leaves external rows unmarked", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [hosted, aWorker({ id: "w-x", name: "laptop" })] });
    renderPage();

    // One list, marked — not two lists: a hosted worker is an ordinary worker whose
    // container the controller runs, so it keeps the same row, status and delete.
    expect(await screen.findByText("hosted")).toBeTruthy();
    expect(screen.getByText("M")).toBeTruthy(); // upper-cased for reading; "m" on the wire
    expect(screen.getAllByText("hosted")).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: "Delete" })).toHaveLength(2);
  });

  it("gives the size chip an accessible name — a lone letter explains nothing", async () => {
    // The chip is the only trace of the size anywhere in the UI (names-only means no
    // quantities), so "M" on its own announces as "M" and tells a screen-reader user
    // nothing. title names it, mirroring the hosted badge beside it.
    mockApi.listWorkers.mockResolvedValue({ workers: [hosted] });
    renderPage();
    expect(await screen.findByTitle("Size M")).toBeTruthy();
    expect(screen.getByTitle(/controller starts and stops/i)).toBeTruthy();
  });

  it("badges a hosted row even when hosting is switched off (never leave a row lying)", async () => {
    // An admin can turn hosting off while a user still holds hosted workers. The rows
    // must stay listed and deletable — and stay honest about what they are, or they
    // read as workers the user forgot to start.
    mockApi.hostedConfig.mockResolvedValue({ enabled: false, quota: 0 });
    mockApi.listWorkers.mockResolvedValue({ workers: [hosted] });
    renderPage();
    expect(await screen.findByText("hosted")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Delete" })).toBeTruthy();
    expect(screen.queryByText("Provision a hosted worker")).toBeNull();
  });

  it("shows the provision card only when the instance has hosting on", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: fleet });
    renderPage();
    await screen.findByText("laptop");
    expect(screen.queryByText("Provision a hosted worker")).toBeNull();

    cleanup();
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    renderPage();
    expect(await screen.findByText("Provision a hosted worker")).toBeTruthy();
  });

  it("counts only hosted workers against the quota, not the whole fleet", async () => {
    // The count comes from the list the page already polls — three external workers
    // must not eat the hosted allowance.
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    mockApi.listWorkers.mockResolvedValue({ workers: [...fleet, hosted] });
    renderPage();
    expect(await screen.findByText(/1 of 2 used/)).toBeTruthy();
  });

  it("still shows the one-time token card for an EXTERNAL worker (the hand-run flow is untouched)", async () => {
    // The regression guard for the sibling flow: hosted provisioning returns no token,
    // and adding it must not have cost createWorker the token card that is the only
    // time its secret is ever shown.
    mockApi.listWorkers.mockResolvedValue({ workers: [] });
    mockApi.createWorker.mockResolvedValue({
      worker: aWorker({ id: "w-new", name: "nas" }),
      token: "uzi_wk_deadbeef",
    });
    renderPage();

    fireEvent.change(await screen.findByPlaceholderText(/laptop, ci-runner-1/), { target: { value: "nas" } });
    fireEvent.click(screen.getByRole("button", { name: "Generate join token" }));

    await waitFor(() => expect(mockApi.createWorker).toHaveBeenCalledWith("nas", "base"));
    expect(await screen.findByText("uzi_wk_deadbeef")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Copy" })).toBeTruthy();
  });
});

describe("WorkersSettings hosted quota is escapable (the primary journey)", () => {
  it("re-enables provisioning after the user deletes a hosted worker", async () => {
    // The one thing a component test of HostedWorkers alone CANNOT prove: it asserts
    // "disabled at quota" and passes whether or not the state is escapable. The count
    // is the page's — it comes from the fleet list — so only the page can show the gate
    // releasing rather than dead-ending the journey. Without this, the loop is only
    // ever checked by hand in the demo build.
    const h1 = aWorker({ id: "w-h1", name: "base (S)", kind: "hosted", hosted_size: "s" });
    const h2 = aWorker({ id: "w-h2", name: "base (M)", kind: "hosted", hosted_size: "m" });
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    mockApi.listWorkers.mockResolvedValue({ workers: [h1, h2] });
    mockApi.deleteWorker.mockResolvedValue(null);
    renderPage();

    const provision = () => screen.getByRole("button", { name: /^Provision$/ });
    expect(await screen.findByText(/2 of 2 used/)).toBeTruthy();
    expect(provision().hasAttribute("disabled")).toBe(true);

    // Delete one → the page reloads the fleet → the count drops → the gate lifts.
    mockApi.listWorkers.mockResolvedValue({ workers: [h1] });
    fireEvent.click(screen.getAllByRole("button", { name: "Delete" })[1]);

    await waitFor(() => expect(mockApi.deleteWorker).toHaveBeenCalledWith("w-h2"));
    expect(await screen.findByText(/1 of 2 used/)).toBeTruthy();
    await waitFor(() => expect(provision().hasAttribute("disabled")).toBe(false));
  });
});
