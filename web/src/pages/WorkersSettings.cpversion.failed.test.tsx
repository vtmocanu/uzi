// @vitest-environment jsdom
// THE ARM THAT HAD NEVER EXISTED: GET /api/version FAILS, and the fleet panel says
// so out loud instead of sitting on a blank "pending" forever.
//
// Before the discriminated snapshot in AppShell.tsx, a rejected fetch resolved the
// shared promise to `null` — the same value an unsettled promise produces — so
// `cpVersion` was null, `versionPending` was true, and FleetUpgradePanel rendered
// `&nbsp;` permanently. The operator of a fleet whose control-plane version call
// was 500ing saw a blank where the answer should be, with nothing anywhere on the
// page saying classification had stopped.
//
// Driven through the WHOLE chain rather than by passing the prop:
//   api.version rejects -> useBuildInfoSnapshot catch -> {status:"failed"}
//   -> useAppVersion "" -> WorkersSettings cpVersion -> FleetUpgradePanel copy.
// WorkerUpgradeBadge.test.tsx already covers the copy given `cpVersion=""` as a
// PROP; that is a different claim, and it stayed green through the entire period
// this arm was unreachable in production.
//
// Own file: the promise is memoised at module scope with no reset seam, and vitest
// isolates per FILE — a file that resolved it cannot reject it.

import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
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
      version: vi.fn(),
      listSecrets: vi.fn(),
      setWorkerBindMode: vi.fn(),
      createWorker: vi.fn(),
      deleteWorker: vi.fn(),
      hostedConfig: vi.fn(),
      provisionHostedWorker: vi.fn(),
    },
  };
});
const mockApi = vi.mocked(api);

function aWorker(over: Partial<Worker> = {}): Worker {
  return {
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
    template_declared: null,
    template_reported: "base",
    version: "0.11.99",
    upgrade_status: "up_to_date",
    upgrade_detail: null,
    upgrade_target: "0.11.99",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: "2026-07-14T00:00:00Z",
    created_at: "2026-07-01T00:00:00Z",
    stats_cpu_pct: null,
    stats_mem_bytes: null,
    stats_mem_limit_bytes: null,
    stats_source: null,
    ...over,
  };
}

beforeEach(() => {
  // A 500 on the unauthenticated build endpoint. The shell must survive it — that
  // is pinned in AppShell.buildinfo.failure.test.tsx — and the fleet panel must now
  // also SAY something about it.
  mockApi.version.mockRejectedValue(new Error("500 from /api/version"));
  mockApi.hostedConfig.mockResolvedValue({ enabled: false, quota: 0 });
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("a FAILED /api/version reaches the fleet panel as settled-unknown", () => {
  it("names the degraded reading instead of pending forever", async () => {
    render(
      <MemoryRouter>
        <WorkersSettings />
      </MemoryRouter>,
    );

    // The third arm, which no production code path could reach before the
    // discriminated snapshot.
    expect(await screen.findByText(/control-plane release unknown — targets unchecked/)).toBeTruthy();

    // …and it must NOT claim classification stopped. It did not: every count, the
    // bar and the attention line below come from each worker's server-computed
    // upgrade_status and are unaffected by this fetch failing. The copy said
    // "classification off" while that arm was unreachable, so nobody had ever read
    // it against the panel it renders in.
    expect(screen.queryByText(/classification off/)).toBeNull();

    // NOT the pending arm and NOT the stamped arm. Asserting the absence of the
    // stamped copy matters because the fixture's worker DOES carry 0.11.99 in its
    // own fields: if the panel ever sourced the target release from a worker rather
    // than from the control plane, this test would be the one to notice.
    expect(screen.queryByText(/target release v/)).toBeNull();

    // The rejection was swallowed, not rethrown: the page around it still rendered.
    expect(screen.getByRole("heading", { name: /Workers/i })).toBeTruthy();

    expect(mockApi.version).toHaveBeenCalledTimes(1);
  });
});
